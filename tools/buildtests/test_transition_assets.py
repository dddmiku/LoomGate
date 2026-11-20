"""Bridge APK reuse and Linux package-generation contracts."""
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("transition_build", ROOT / "tools/build_transition.py")
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


class TransitionAssetTests(unittest.TestCase):
    def test_bridge_tag_cannot_skip_the_legacy_generation(self):
        self.assertEqual(bridge.bridge_tag("v3.17.1"), "v3.17.1")
        for tag in ("v1.0.6", "v3.16.2", "3.17.1", "v3.17.1-dirty"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                bridge.bridge_tag(tag)

    def test_bridge_reuses_a_new_loomgate_apk_with_its_own_version_name(self):
        identity = dict(package=bridge.android.APK_PACKAGE, version_name="1.0.6",
                        version_code=bridge.android.version_code("v1.0.6"))
        with patch.object(bridge.android, "verify_apk_signature") as signature, \
                patch.object(bridge.android, "read_apk_manifest", return_value=identity):
            self.assertEqual(bridge.android_bridge_asset(Path("candidate.apk"), Path("sdk")), identity)
        signature.assert_called_once()

    def test_bridge_refuses_an_old_epoch_or_renamed_legacy_apk(self):
        for name, code in [("1.0.6", 5_000_006), ("3.17.1", 7_017_001)]:
            identity = dict(package=bridge.android.APK_PACKAGE, version_name=name, version_code=code)
            with self.subTest(name=name), patch.object(bridge.android, "verify_apk_signature"), \
                    patch.object(bridge.android, "read_apk_manifest", return_value=identity), \
                    self.assertRaises(ValueError):
                bridge.android_bridge_asset(Path("candidate.apk"), Path("sdk"))

    def test_bridge_refuses_a_different_android_signing_identity(self):
        with patch.object(bridge.android, "verify_apk_signature", side_effect=SystemExit("signer mismatch")), \
                patch.object(bridge.android, "read_apk_manifest") as manifest, \
                self.assertRaises(SystemExit):
            bridge.android_bridge_asset(Path("candidate.apk"), Path("sdk"))
        manifest.assert_not_called()

    def test_bridge_refuses_a_signed_apk_from_a_different_source_version(self):
        identity = dict(package=bridge.android.APK_PACKAGE, version_name="1.0.5",
                        version_code=bridge.android.version_code("v1.0.5"))
        with patch.object(bridge.android, "verify_apk_signature"), \
                patch.object(bridge.android, "read_apk_manifest", return_value=identity), \
                patch.object(bridge.android, "read_version", return_value="v1.0.6"), \
                self.assertRaisesRegex(ValueError, "source version"):
            bridge.android_bridge_asset(Path("candidate.apk"), Path("sdk"))


if __name__ == "__main__":
    unittest.main()
