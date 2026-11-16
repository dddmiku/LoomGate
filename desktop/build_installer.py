#!/usr/bin/env python3
"""Build a local Windows acceptance installer; never signs or publishes releases."""
import argparse,hashlib,importlib.util,json,os,re,subprocess,shutil
from pathlib import Path

ROOT=Path(__file__).resolve().parents[1]
RUNTIME_FILES=('bridge.py','managed.py','stream_bridge.py','runtime.js','tasks_runtime.js','stream_runtime.js','probe_runtime.js','probe_cleanup.js','requirements.txt')
PAYLOAD=('LoomGate.exe','wb2api.exe','login.exe','credit.exe','fission_bin.exe','release-signing.pub','manifest.json','README.txt','LICENSE')+tuple('doubao-runtime/'+name for name in RUNTIME_FILES)
PROGRAMS={'LoomGate.exe':'desktop','wb2api.exe':'server','login.exe':'login','credit.exe':'credit','fission_bin.exe':'fission'}

def digest(path):return hashlib.sha256(path.read_bytes()).hexdigest()
def verify_payload(path):
    entries=list(path.rglob('*'))
    if any(p.is_symlink() or (hasattr(p,'is_junction') and p.is_junction()) for p in entries) or {p.relative_to(path).as_posix() for p in entries if p.is_file()}!=set(PAYLOAD) or any(p.is_dir() and p.relative_to(path).as_posix()!='doubao-runtime' for p in entries):
        raise ValueError('installer_payload_not_allowlisted')
    manifest=json.loads((path/'manifest.json').read_text(encoding='utf-8'))
    if set(manifest['files'])!=set(PAYLOAD)-{'manifest.json'}:raise ValueError('installer_manifest_incomplete')
    for name,expected in manifest['files'].items():
        if name not in PAYLOAD or digest(path/name)!=expected:raise ValueError('installer_payload_hash_mismatch')

def build(output,compiler,label):
    output=output.resolve()
    if output==ROOT or ROOT in output.parents or output==output.parent or not output.is_absolute():raise ValueError('output_must_be_separate_workspace')
    if not re.fullmatch(r'dev-[a-zA-Z0-9._-]{1,64}',label):raise ValueError('development_label_required')
    payload=output/'payload';artifacts=output/'artifacts'
    for path in [output,payload,artifacts]:
        if path.is_symlink():raise ValueError('output_link_rejected')
    if output.exists() and any(output.iterdir()):raise ValueError('output_not_empty_inspect_before_reuse')
    payload.mkdir(parents=True);artifacts.mkdir()
    spec=importlib.util.spec_from_file_location('desktop_builder',ROOT/'desktop/build.py');builder=importlib.util.module_from_spec(spec);spec.loader.exec_module(builder)
    commit=subprocess.check_output(['git','rev-parse','HEAD'],cwd=ROOT,text=True).strip()
    report={'label':label,'base_commit':commit,'source_dirty':bool(subprocess.check_output(['git','status','--porcelain'],cwd=ROOT)),'commands':[],'published':False,'release_signature':'NOT GENERATED','authenticode':'NOT SIGNED','trust_root_preserved':True}
    flags=f'-s -w -X workbuddy2api/internal/version.Version={label} -X workbuddy2api/internal/version.Commit={commit} -X workbuddy2api/internal/version.BuiltAt=local-acceptance'
    public=builder.release_public_key()
    for name,pkg in PROGRAMS.items():
        extra=' -X main.deployment=loomgate-installed -X main.version='+label if pkg=='desktop' else ''
        link=flags+extra+' -X workbuddy2api/internal/version.ReleaseKey='+public
        cmd=[builder.GO,'build','-p=1','-mod=readonly','-buildvcs=false','-trimpath','-ldflags',link,'-o',str(payload/name),'./cmd/'+pkg]
        with (output/(pkg+'-build.log')).open('wb') as log:result=subprocess.run(cmd,cwd=ROOT/'gateway',env=builder.build_env('windows','amd64'),stdout=log,stderr=subprocess.STDOUT,timeout=180)
        if result.returncode:raise RuntimeError('program_build_failed')
        metadata=subprocess.run([builder.GO,'version','-m',str(payload/name)],capture_output=True,text=True,timeout=30)
        settings={}
        for line in metadata.stdout.splitlines():
            parts=line.strip().split('\t',1)
            if len(parts)==2 and parts[0]=='build' and '=' in parts[1]:
                key,value=parts[1].split('=',1);settings[key]=value
        if metadata.returncode or any(settings.get(k)!=v for k,v in {'GOOS':'windows','GOARCH':'amd64','CGO_ENABLED':'0'}.items()):raise ValueError('program_platform_mismatch')
        if pkg in ['desktop','server']:
            info=subprocess.run([str(payload/name),'--loomgate-build-info'],capture_output=True,text=True,timeout=10,creationflags=subprocess.CREATE_NO_WINDOW)
            attestation=json.loads(info.stdout)
            if info.returncode or not attestation['verification_enabled'] or attestation['release_key_sha256']!=hashlib.sha256(bytes.fromhex(public)).hexdigest() or attestation['version']!=label or attestation['commit']!=commit:raise ValueError('native_trust_root_mismatch')
        report['commands'].append({'program':name,'build_exit_code':0,'go_metadata_verified':True})
    (payload/'release-signing.pub').write_bytes((ROOT/'gateway/release-signing.pub').read_bytes())
    (payload/'LICENSE').write_bytes((ROOT/'LICENSE').read_bytes())
    (payload/'doubao-runtime').mkdir()
    for name in RUNTIME_FILES:shutil.copyfile(ROOT/'tools/doubao-runtime'/name,payload/'doubao-runtime'/name)
    (payload/'README.txt').write_text('LoomGate '+label+'\n\nDevelopment acceptance build. No production release or automatic migration.\nStart program/LoomGate.exe. The new task engine is enabled; no legacy scheduler is started.\nData lives in workspace/ next to program/, or WB2A_DESKTOP_ROOT when explicitly set.\nRepair, upgrade and uninstall preserve workspace. Do not share workspace; it contains credentials.\nUpdates retain the existing release verification public key and are disabled by default.\nBefore production migration, use the offline migration dry-run and obtain independent acceptance.\n',encoding='utf-8')
    manifest={'brand':'LoomGate','label':label,'base_commit':commit,'source_dirty':report['source_dirty'],'files':{name:digest(payload/name) for name in PAYLOAD if name!='manifest.json'}}
    (payload/'manifest.json').write_text(json.dumps(manifest,indent=2),encoding='utf-8');verify_payload(payload)
    cmd=[str(compiler),'/Qp','/DPayloadDir='+str(payload),'/DOutputRoot='+str(artifacts),'/DBuildLabel='+label,str(ROOT/'desktop/loomgate.iss')]
    with (output/'compiler.log').open('wb') as log:result=subprocess.run(cmd,stdout=log,stderr=subprocess.STDOUT,timeout=180)
    report['installer_exit_code']=result.returncode
    if result.returncode:raise RuntimeError('installer_compile_failed')
    installer=artifacts/'LoomGate-setup-amd64.exe';report['installer_sha256']=digest(installer);report['installer_bytes']=installer.stat().st_size
    verify_payload(payload);(output/'build-report.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
    print(json.dumps({'installer':str(installer),'sha256':report['installer_sha256'],'published':False}))

if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('--output',type=Path,required=True);p.add_argument('--compiler',type=Path,required=True);p.add_argument('--label',required=True);a=p.parse_args()
    build(a.output,a.compiler,a.label)
