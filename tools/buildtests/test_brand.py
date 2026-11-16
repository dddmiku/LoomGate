"""Display branding and compatibility identities are separate contracts."""
import pathlib,unittest
ROOT=pathlib.Path(__file__).resolve().parents[2]
class BrandTests(unittest.TestCase):
 def test_current_display_surfaces_use_only_loomgate(self):
  for name in ['README.md','gateway/panel/index.html','gateway/panel/build.py','gateway/panel/src/body.html','gateway/panel/app.js','gateway/panel/console/index.html','gateway/panel/console/login.html','desktop/build.py','android/build.py','android/app/src/main/AndroidManifest.xml','android/app/src/main/java/com/workbuddy2api/app/GatewayService.java']:
   text=(ROOT/name).read_text(encoding='utf-8');self.assertNotIn('\u7ec7\u67a2',text,name);self.assertIn('LoomGate',text,name)
 def test_android_identity_retained_with_new_display_name(self):
  gradle=(ROOT/'android/app/build.gradle.kts').read_text(encoding='utf-8');manifest=(ROOT/'android/app/src/main/AndroidManifest.xml').read_text(encoding='utf-8')
  self.assertIn('applicationId = "com.workbuddy2api.app"',gradle);self.assertIn('android:label="LoomGate"',manifest);self.assertIn('com.workbuddy2api.app.apk',manifest)
if __name__=='__main__':unittest.main()
