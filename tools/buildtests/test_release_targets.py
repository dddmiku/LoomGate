"""Manifest-driven CI packaging tests; compiler calls are isolated, no release or deployment."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

REPO = Path(__file__).resolve().parents[2]
SCRIPT_DIR = REPO / "gateway" / "scripts"
sys.path.insert(0, str(SCRIPT_DIR))
try:
    spec = importlib.util.spec_from_file_location("ci_build_acceptance", SCRIPT_DIR / "build_acceptance.py")
    build = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(build)
finally:
    sys.path.pop(0)


class ReleaseTargetsTests(unittest.TestCase):
    def setUp(self):
        scratch = Path(os.environ.get("LOOMGATE_TEST_TMP", "D:/LoomGate-work/tmp/buildtests"
                                     if os.name == "nt" else str(REPO.parent / "LoomGate-work/tmp/buildtests")))
        scratch.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="targets-", dir=scratch)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.data = build.load_targets()
        self.gateway = self.root / "source" / "gateway"
        self.gateway.mkdir(parents=True)
        (self.gateway / "release-signing.pub").write_text("11" * 32, encoding="ascii")
        for name in build.runtime_bundle.FILES:
            if name not in build.PROGRAMS:
                path = self.gateway / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(b"synthetic source\r\n")
        guard = patch.object(build, "GATEWAY", self.gateway)
        guard.start()
        self.addCleanup(guard.stop)

    def fake_compile(self, command, **kwargs):
        output = Path(command[command.index("-o") + 1])
        output.write_bytes(b"synthetic compiled artifact")
        return ""

    def test_matrix_is_derived_only_from_ci_eligible_targets(self):
        matrix = build.ci_matrix(self.data)
        self.assertIn({"id": "linux-amd64"}, matrix["include"])
        self.assertIn({"id": "windows-amd64"}, matrix["include"])
        self.assertNotIn({"id": "android-arm64"}, matrix["include"])
        changed = copy.deepcopy(self.data)
        changed["targets"][0]["ci_build"] = False
        self.assertNotIn({"id": "linux-amd64"}, build.ci_matrix(changed)["include"])

    def test_manifest_refuses_collisions_paths_and_android_signing_substitution(self):
        mutations = [lambda d: d["targets"].append(copy.deepcopy(d["targets"][0])),
                     lambda d: d["targets"][0].update(update_asset="../config.json"),
                     lambda d: d["targets"][-1].update(ci_build=True),
                     lambda d: d.update(public_key="another-key.pub")]
        for index, mutate in enumerate(mutations):
            data = copy.deepcopy(self.data)
            mutate(data)
            path = self.root / ("manifest-%d.json" % index)
            path.write_text(json.dumps(data), encoding="utf-8")
            with self.subTest(index=index), self.assertRaises(ValueError):
                build.load_targets(path)

    def test_android_is_not_built_with_an_unrelated_ci_signing_identity(self):
        with patch.object(build, "checked") as checked, self.assertRaises(ValueError):
            build.build_target(self.data, "android-arm64", self.root / "work", "a" * 40)
        checked.assert_not_called()

    def test_linux_package_uses_legacy_alias_and_explicit_runtime_file_allowlist(self):
        work = self.root / "work"
        stage = work / "stage" / "linux-amd64"
        stage.mkdir(parents=True)
        (stage / "config.json").write_text("not part of the package")
        with patch.object(build, "checked", side_effect=self.fake_compile) as checked, \
                patch.object(build, "verify_platform") as verify, \
                patch.object(build, "verify_runtime_identity") as identity:
            report = build.build_target(self.data, "linux-amd64", work, "a" * 40)
        self.assertEqual(checked.call_count, len(build.PROGRAMS))
        self.assertEqual(verify.call_count, len(build.PROGRAMS))
        identity.assert_called_once()
        out = work / "artifacts" / "linux-amd64"
        self.assertTrue((out / "wb2api-linux-amd64").is_file())
        with tarfile.open(out / "wb2api-runtime-linux-amd64.tar.gz") as archive:
            self.assertEqual(set(archive.getnames()), set(build.runtime_bundle.FILES) | {"manifest.json"})
            self.assertNotIn("config.json", archive.getnames())
        self.assertEqual(report["version"], "dev-" + "a" * 12)
        self.assertFalse(report["published"])
        self.assertEqual(report["release_signature"], "NOT GENERATED")
        self.assertFalse((out / self.data["signature_asset"]).exists())

    def test_windows_ci_asset_is_the_updater_binary_not_a_false_complete_desktop_package(self):
        work = self.root / "work"
        with patch.object(build, "checked", side_effect=self.fake_compile) as checked, \
                patch.object(build, "verify_platform"), patch.object(build, "verify_runtime_identity"):
            report = build.build_target(self.data, "windows-amd64", work, "b" * 40)
        self.assertEqual(checked.call_count, 1)
        self.assertEqual(set(report["artifacts"]), {"wb2api-windows-amd64"})
        self.assertEqual(checked.call_args.kwargs["env"]["GOOS"], "windows")


    def test_runtime_identity_check_executes_the_artifact_not_metadata_strings(self):
        # A crafted artifact with the right strings in metadata is not enough:
        # the self-check compares real execution output with expected values.
        output = self.root / "selfcheck"
        output.mkdir()
        binary = output / "wb2api"
        binary.write_bytes(b"synthetic binary placeholder")
        fake = type("Result", (), {"returncode": 0, "stdout": json.dumps({
            "version": "v1.0.0-dev", "commit": "a" * 40, "goos": "linux", "goarch": "amd64",
            "verification_enabled": True,
            "release_key_sha256": "zzz"})})
        with patch.object(build, "host_platform", return_value=("linux", "amd64")), \
                patch.object(build, "BINARY_PLATFORMS", {str(binary): ("linux", "amd64")}), \
                patch.object(build.subprocess, "run", return_value=fake) as run:
            with self.assertRaisesRegex(ValueError, "key digest"):
                build.verify_runtime_identity(binary, "11" * 32, "v1.0.0-dev")
        self.assertEqual(run.call_args.args[0], [str(binary), "--loomgate-build-info"])

    def test_cross_compiled_artifact_refuses_identity_probe_instead_of_string_scan(self):
        binary = self.root / "wb2api.exe"
        binary.write_bytes(b"PE placeholder")
        with patch.object(build, "host_platform", return_value=("linux", "amd64")), \
                patch.object(build, "BINARY_PLATFORMS", {str(binary): ("windows", "amd64")}), \
                patch.object(build.subprocess, "run") as run:
            with self.assertRaisesRegex(ValueError, "target_runner"):
                build.verify_runtime_identity(binary, "11" * 32, "v1.0.0-dev")
        run.assert_not_called()

    def test_platform_metadata_does_not_require_ldflags(self):
        binary = self.root / "wb2api-metadata"
        binary.write_bytes(b"fixture")
        metadata = "fixture: go1.26.8\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH=amd64\n\tbuild\tCGO_ENABLED=0\n"
        with patch.object(build, "checked", return_value=metadata):
            build.verify_platform("go", binary, {"goos": "linux", "goarch": "amd64"})
        self.assertNotIn("-ldflags", metadata)

    def test_platform_metadata_rejects_wrong_platform_or_abi(self):
        binary = self.root / "wb2api-metadata-bad"
        binary.write_bytes(b"fixture")
        for metadata in (
                "fixture: go1.26.8\n\tbuild\tGOOS=windows\n\tbuild\tGOARCH=amd64\n\tbuild\tCGO_ENABLED=0\n",
                "fixture: go1.26.8\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH=amd64\n\tbuild\tCGO_ENABLED=1\n",
                "fixture: go1.26.8\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH=arm\n\tbuild\tGOARM=6\n\tbuild\tCGO_ENABLED=0\n"):
            with self.subTest(metadata=metadata), patch.object(build, "checked", return_value=metadata), \
                    self.assertRaises(ValueError):
                build.verify_platform("go", binary, {"goos": "linux", "goarch": "arm", "goarm": "5"})

    def test_unknown_artifact_files_are_preserved_and_block_upload_staging(self):
        out = self.root / "work" / "artifacts" / "linux-amd64"
        out.mkdir(parents=True)
        unexpected = out / "config.json"
        unexpected.write_text("must stay local")
        with patch.object(build, "checked") as checked, self.assertRaises(ValueError):
            build.build_target(self.data, "linux-amd64", self.root / "work", "a" * 40)
        checked.assert_not_called()
        self.assertEqual(unexpected.read_text(), "must stay local")

    def test_build_output_cannot_be_inside_source_checkout(self):
        with patch.object(build, "checked") as checked, self.assertRaises(ValueError):
            build.build_target(self.data, "linux-amd64", self.gateway / "dist", "a" * 40)
        checked.assert_not_called()


if __name__ == "__main__":
    unittest.main()



class RuntimeIdentityStatusTests(unittest.TestCase):
    """Cross-compiled CI artifacts record a deferred identity, never a fake pass."""

    def test_cross_compiled_artifact_is_deferred(self):
        with patch.object(build, "verify_runtime_identity",
                          side_effect=ValueError("cross_compiled_identity_requires_target_runner")):
            self.assertEqual(
                build.runtime_identity_status(Path("artifact"), "11" * 32, "v1.0.0-dev"),
                "deferred_to_target_runner")

    def test_host_artifact_is_verified(self):
        with patch.object(build, "verify_runtime_identity"):
            self.assertEqual(
                build.runtime_identity_status(Path("artifact"), "11" * 32, "v1.0.0-dev"),
                "verified_on_host")

    def test_other_identity_failures_are_not_deferred(self):
        with patch.object(build, "verify_runtime_identity", side_effect=ValueError("key digest mismatch")):
            with self.assertRaisesRegex(ValueError, "key digest"):
                build.runtime_identity_status(Path("artifact"), "11" * 32, "v1.0.0-dev")
