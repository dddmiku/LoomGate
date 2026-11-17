import importlib.util,json,os,tempfile,unittest
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('installer',ROOT/'desktop/build_installer.py');build=importlib.util.module_from_spec(spec);spec.loader.exec_module(build)

class InstallerGuards(unittest.TestCase):
 def setUp(self):
  root=Path(os.environ.get('LOOMGATE_TEST_TMP','D:/LoomGate-work/tmp/buildtests' if os.name=='nt' else str(ROOT.parent/'LoomGate-work/tmp/buildtests')));root.mkdir(parents=True,exist_ok=True)
  self.temp=tempfile.TemporaryDirectory(dir=root);self.addCleanup(self.temp.cleanup);self.path=Path(self.temp.name)
 def seed(self):
  for name in build.PAYLOAD:
   (self.path/name).parent.mkdir(parents=True,exist_ok=True);(self.path/name).write_bytes(b'synthetic-build')
  (self.path/'manifest.json').write_text(json.dumps({'files':{name:build.digest(self.path/name) for name in build.PAYLOAD if name!='manifest.json'}}))
 def test_payload_accepts_only_verified_runtime_files(self):
  self.seed();build.verify_payload(self.path);(self.path/'auths').mkdir()
  with self.assertRaises(ValueError):build.verify_payload(self.path)
 def test_tampered_program_is_rejected(self):
  self.seed();(self.path/'wb2api.exe').write_bytes(b'changed')
  with self.assertRaises(ValueError):build.verify_payload(self.path)
 def test_installer_never_registers_workspace_for_removal(self):
  source=(ROOT/'desktop/loomgate.iss').read_text(encoding='utf-8');self.assertNotIn('[UninstallDelete]',source);self.assertNotIn('recursesubdirs',source.lower());self.assertNotIn('DestDir: "{app}\\workspace',source);self.assertIn('PrivilegesRequired=lowest',source)
 def test_builder_cannot_overwrite_checkout_or_populated_workspace(self):
  with self.assertRaises(ValueError):build.build(ROOT/'dist',Path('unused'),'dev-test')
  (self.path/'keep.txt').write_text('preserve')
  with self.assertRaises(ValueError):build.build(self.path,Path('unused'),'dev-test')
  self.assertEqual((self.path/'keep.txt').read_text(),'preserve')

if __name__=='__main__':unittest.main()
