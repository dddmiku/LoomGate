"""Local Windows installer acceptance with an empty dedicated workspace only."""
import argparse,hashlib,json,os,socket,subprocess,sys,time,urllib.request
from pathlib import Path
from playwright.sync_api import sync_playwright

def sha(path):return hashlib.sha256(path.read_bytes()).hexdigest()
def inventory(root):
 return {str(p.relative_to(root)):sha(p) for p in root.rglob('*') if p.is_file()}

def run(installer,root,out):
 if os.name!='nt' or root.drive.upper()!='D:' or out.drive.upper()!='D:' or root.exists():raise ValueError('new_dedicated_D_workspace_required')
 out.mkdir(parents=True,exist_ok=True);root.parent.mkdir(parents=True,exist_ok=True)
 report={'checks':[],'commands':[],'real_provider_requests':0,'page_errors':[],'console_errors':[],'external_requests':[],'screenshots':[],'result':'INCOMPLETE'}
 env={**os.environ,'TEMP':str(out/'tmp'),'TMP':str(out/'tmp'),'LOOMGATE_DESKTOP_HEADLESS':'1','PYTHONDONTWRITEBYTECODE':'1'};Path(env['TEMP']).mkdir(exist_ok=True)
 for key in list(env):
  if key.startswith(('WB2A_','WB2API_','LOOMGATE_DOUBAO_')):env.pop(key)
 installer_digest=sha(installer);child=None;log=None
 def command(name,args):
  p=subprocess.run(args,env=env,capture_output=True,timeout=120,creationflags=subprocess.CREATE_NO_WINDOW)
  report['commands'].append({'check':name,'exit_code':p.returncode});(out/'installer-acceptance.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
  if p.returncode:raise RuntimeError(name+'_failed')
 def install(name):command(name,[str(installer),'/CURRENTUSER','/VERYSILENT','/SUPPRESSMSGBOXES','/NORESTART','/SP-','/NOICONS','/DIR='+str(root),'/GROUP=LoomGate Acceptance','/LOG='+str(out/(name+'.log'))])
 def start():
  nonlocal child,log
  with socket.socket() as probe:
   if probe.connect_ex(('127.0.0.1',17864))==0:raise ValueError('acceptance_port_in_use')
  log=(out/'runtime.log').open('wb');child=subprocess.Popen([str(root/'program/LoomGate.exe')],cwd=root/'program',env=env,stdin=subprocess.PIPE,stdout=log,stderr=log,creationflags=subprocess.CREATE_NO_WINDOW)
  (out/'owned-process.json').write_text(json.dumps({'pid':child.pid,'path':str(root/'program/LoomGate.exe'),'root':str(root),'port':17864}))
  opener=urllib.request.build_opener(urllib.request.ProxyHandler({}));deadline=time.monotonic()+35
  while time.monotonic()<deadline:
   if child.poll() is not None:raise RuntimeError('launcher_exited_before_ready')
   try:
    with opener.open('http://127.0.0.1:17864/admin/login',timeout=.5) as response:
     if response.status==200:return
   except OSError:pass
   time.sleep(.1)
  raise RuntimeError('launcher_not_ready')
 def stop():
  nonlocal child,log
  if child is None:return
  if child.poll() is None:child.stdin.write(b'stop\n');child.stdin.flush()
  code=child.wait(timeout=30);child.stdin.close();log.close();report['commands'].append({'check':'graceful_launcher_shutdown','exit_code':code});assert code==0
  with socket.socket() as probe:assert probe.connect_ex(('127.0.0.1',17864))!=0
  child=None
 try:
  install('install');manifest=json.loads((root/'program/manifest.json').read_text());assert manifest['brand']=='LoomGate'
  for name,digest in manifest['files'].items():assert sha(root/'program'/name)==digest
  assert not (root/'workspace').exists();report['checks'].append('installed_payload_matches_allowlist_no_runtime_data')
  # A synthetic custom configuration verifies that startup/repair never resets
  # user choices. It contains no account and uses a reserved acceptance port.
  workspace=root/'workspace';(workspace/'config').mkdir(parents=True)
  config={'listen':'127.0.0.1:17864','auth_dir':'./auths','api_keys_file':'./data/api_keys.json','state_file':'./data/state.json','usage_file':'./data/usage.json','update':{'enabled':False},'schedule':{'checkin_enabled':False,'trae_check_enabled':False,'travel_enabled':False,'activity_enabled':False,'keepalive_enabled':False,'school_enabled':False,'cat_enabled':False,'growth_enabled':False,'balance_enabled':False}}
  config_path=workspace/'config/config.json';config_path.write_text(json.dumps(config),encoding='utf-8');original_config=sha(config_path)
  start()
  with sync_playwright() as pw:
   browser=pw.chromium.launch(headless=True);page=browser.new_page(viewport={'width':1440,'height':1000})
   page.on('pageerror',lambda e:report['page_errors'].append(type(e).__name__));page.on('console',lambda m:report['console_errors'].append('console_error') if m.type=='error' else None)
   page.on('request',lambda req:report['external_requests'].append('external') if not req.url.startswith('http://127.0.0.1:17864/') else None)
   fields=dict(x.split(':',1) for x in (workspace/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in x)
   page.goto('http://127.0.0.1:17864/admin/');page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-overview]').wait_for()
   def api(path):return page.request.get('http://127.0.0.1:17864/admin/api/'+path).json()
   assert api('workbench')['schedules']==[] and api('workbench/accounts')['accounts']==[]
   page.locator('#navigation a[href="#tasks"]').click();page.locator('#task-list.loading').wait_for(state='detached');page.screenshot(path=str(out/'installed-desktop.png'),full_page=True);report['screenshots'].append('installed-desktop.png')
   page.set_viewport_size({'width':390,'height':844});assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1');page.screenshot(path=str(out/'installed-mobile.png'),full_page=True);report['screenshots'].append('installed-mobile.png')
   assert not report['page_errors'] and not report['console_errors'] and not report['external_requests'];browser.close()
  stop();assert sha(config_path)==original_config;report['checks'].append('installed_launcher_new_engine_login_ui_and_graceful_shutdown')
  (workspace/'data/preserve-fixture.txt').write_text('synthetic-preserved-file',encoding='utf-8');before=inventory(workspace)
  install('repair');assert inventory(workspace)==before;report['checks'].append('repair_preserves_all_workspace_bytes')
  start();stop();assert sha(config_path)==original_config;report['checks'].append('relaunch_after_repair_preserves_configuration')
  before=inventory(workspace);uninstaller=root/'program/uninstall/unins000.exe';assert uninstaller.exists()
  command('uninstall',[str(uninstaller),'/VERYSILENT','/SUPPRESSMSGBOXES','/NORESTART','/LOG='+str(out/'uninstall.log')])
  deadline=time.monotonic()+10
  while (root/'program/LoomGate.exe').exists() and time.monotonic()<deadline:time.sleep(.1)
  assert not (root/'program/LoomGate.exe').exists() and inventory(workspace)==before
  report['checks'].append('uninstall_removes_program_and_retains_every_workspace_file')
  assert sha(installer)==installer_digest;report.update(result='PASS',installer_sha256=installer_digest,data_preserved=True,authenticode='NOT SIGNED',production_migration='NOT RUN')
 finally:
  if child is not None:
   try:stop()
   except Exception:report['shutdown_failed']=True
  (out/'installer-acceptance.json').write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
 print(json.dumps({'result':report['result'],'checks':len(report['checks']),'real_provider_requests':0}))

if __name__=='__main__':
 p=argparse.ArgumentParser();p.add_argument('--installer',type=Path,required=True);p.add_argument('--root',type=Path,required=True);p.add_argument('--output',type=Path,required=True);args=p.parse_args();run(args.installer.resolve(),args.root.resolve(),args.output.resolve())
