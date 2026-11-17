"""构建守卫回归：模拟编译器/打包器输出，不启动 Go/Gradle 构建或访问私钥。"""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


REPO = Path(__file__).resolve().parents[2]
PUBLIC_KEY = "11" * 32
TAG = "v3.16.1"


def load_script(name, relative):
    spec = importlib.util.spec_from_file_location(name, REPO / relative)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


desktop = load_script("loomgate_desktop_build", "desktop/build.py")
android = load_script("loomgate_android_build", "android/build.py")


def metadata(goos="android", goarch="arm64", cgo="1", tag=TAG, key=PUBLIC_KEY, tags=""):
    # Go 1.26 does not record -ldflags here; the release key and tag are checked
    # in the artifact bytes instead.
    settings = {"GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": cgo,
                "-tags": json.dumps(tags)}
    return "fixture: go1.26.8\n" + "".join("\tbuild\t%s=%s\n" % item for item in settings.items())


def artifact(path, tag=TAG, key=PUBLIC_KEY):
    path.write_bytes(("fixture:" + tag + ":" + key).encode("ascii"))
    return path


ELF_BIONIC = """[Requesting program interpreter: /system/bin/linker64]
 0x00000001 (NEEDED) Shared library: [libc.so]
 19: 00000000 0 FUNC GLOBAL DEFAULT UND getaddrinfo@LIBC (2)
"""


def result(stdout="", code=0):
    return subprocess.CompletedProcess([], code, stdout=stdout, stderr="")


class BuildGuardTests(unittest.TestCase):
    def setUp(self):
        scratch = Path(os.environ.get("LOOMGATE_TEST_TMP", "D:/LoomGate-work/tmp/buildtests"
                                     if os.name == "nt" else str(REPO.parent / "LoomGate-work/tmp/buildtests")))
        scratch.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="guards-", dir=scratch)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.gateway = self.root / "gateway"
        self.gateway.mkdir()
        (self.gateway / "release-signing.pub").write_text(PUBLIC_KEY, encoding="ascii")
        (self.gateway / "VERSION").write_text(TAG, encoding="ascii")
        self.jnilibs = self.root / "jniLibs"
        self.clang = self.root / "ndk-bin" / ("aarch64-linux-android24-clang.cmd" if sys.platform == "win32"
                                             else "aarch64-linux-android24-clang")
        self.clang.parent.mkdir()
        self.clang.write_text("fixture", encoding="ascii")
        self.readelf = self.clang.parent / ("llvm-readelf.exe" if sys.platform == "win32" else "llvm-readelf")
        self.readelf.write_text("fixture", encoding="ascii")
        for module in (desktop, android):
            guard = patch.object(module, "GATEWAY", self.gateway)
            guard.start()
            self.addCleanup(guard.stop)
        guard = patch.object(android, "JNILIBS_ROOT", self.jnilibs)
        guard.start()
        self.addCleanup(guard.stop)

    def test_public_key_missing_or_malformed_fails_closed(self):
        public = self.gateway / "release-signing.pub"
        for value in ("", "0" * 64, "11" * 31, "xx" * 32, "11" * 64):
            public.write_text(value, encoding="ascii")
            for module in (desktop, android):
                with self.subTest(module=module.__name__, length=len(value)), self.assertRaises(SystemExit):
                    module.release_public_key()
        public.unlink()
        for module in (desktop, android):
            with self.assertRaises(SystemExit):
                module.release_public_key()

    def test_public_key_accepts_exact_existing_public_key(self):
        (self.gateway / "release-signing.pub").write_text(PUBLIC_KEY.upper() + "\n", encoding="ascii")
        self.assertEqual(desktop.release_public_key(), PUBLIC_KEY)
        self.assertEqual(android.release_public_key(), PUBLIC_KEY)

    def test_desktop_preserves_flags_and_injects_verified_public_key(self):
        prior = "-tags=example -p=8 -mod=mod"
        output = self.root / "wb2api.exe"
        artifact(output)
        with patch.dict(os.environ, {"GOFLAGS": prior, "GOMAXPROCS": "8"}), \
                patch.object(desktop.subprocess, "run", side_effect=[
                    result(), result(metadata("windows", "amd64", "0"))]) as run:
            desktop.go_build("server", output, "windows", "amd64")
        command = run.call_args_list[0].args[0]
        env = run.call_args_list[0].kwargs["env"]
        self.assertEqual(env["GOFLAGS"], prior)
        self.assertEqual(env["GOMAXPROCS"], "2")
        self.assertIn("-p=1", command)
        self.assertIn("-mod=readonly", command)
        self.assertIn("ReleaseKey=" + PUBLIC_KEY, command[command.index("-ldflags") + 1])
        self.assertEqual(run.call_args_list[1].args[0][1:3], ["version", "-m"])

    def test_desktop_helper_does_not_require_the_unused_release_key(self):
        output = self.root / "login.exe"
        output.write_bytes(b"helper without version package")
        with patch.object(desktop.subprocess, "run",
                          return_value=result(metadata("windows", "amd64", "0"))):
            desktop.verify_go_binary(output, "windows", "amd64", PUBLIC_KEY, require_key=False)

    def test_desktop_missing_key_fails_before_compiler(self):
        (self.gateway / "release-signing.pub").unlink()
        with patch.object(desktop.subprocess, "run") as run, self.assertRaises(SystemExit):
            desktop.go_build("server", self.root / "out" / "wb2api.exe", "windows", "amd64")
        run.assert_not_called()
        self.assertFalse((self.root / "out").exists())

    def test_desktop_rebuild_preserves_runtime_data(self):
        dist = self.root / "dist"
        for target_os, build in (("windows", desktop.build_windows), ("macos", desktop.build_macos)):
            target = dist / ("workbuddy2api-%s-amd64" % target_os)
            data = target / "data" / "fixture.txt"
            data.parent.mkdir(parents=True)
            data.write_text("must survive")
            with self.subTest(target=target_os), patch.object(desktop, "DIST", dist), \
                    patch.object(desktop, "stage_common") as stage, self.assertRaises(SystemExit):
                build("amd64", TAG)
            stage.assert_not_called()
            self.assertEqual(data.read_text(), "must survive")

    def test_desktop_rejects_unverified_or_wrong_platform_artifact(self):
        for label, raw, key in (
                ("wrong-key", metadata("windows", "amd64", "0", key="22" * 32), "33" * 32),
                ("wrong-arch", metadata("windows", "arm64", "0"), PUBLIC_KEY),
                ("wrong-cgo", metadata("windows", "amd64", "1"), PUBLIC_KEY),
                ("empty-metadata", "", PUBLIC_KEY)):
            output = self.root / "wb2api.exe"
            output.write_bytes(("fixture:" + TAG + ":" + key).encode("ascii"))
            if label == "wrong-key":
                output.write_bytes(("fixture:" + TAG + ":" + "33" * 32).encode("ascii"))
            with self.subTest(label=label), patch.object(
                    desktop.subprocess, "run", return_value=result(raw)), \
                    self.assertRaises(SystemExit):
                desktop.verify_go_binary(output, "windows", "amd64", PUBLIC_KEY)

    def test_android_missing_ndk_preserves_existing_artifact(self):
        target = self.jnilibs / "arm64-v8a" / "libwb2gateway.so"
        target.parent.mkdir(parents=True)
        target.write_bytes(b"existing")
        with patch.object(android, "run") as run, self.assertRaises(SystemExit):
            android.build_go_binaries("go", None, TAG, "arm64-v8a")
        run.assert_not_called()
        self.assertEqual(target.read_bytes(), b"existing")

    def test_android_missing_readelf_fails_before_compiler(self):
        self.readelf.unlink()
        with patch.object(android, "run") as run, self.assertRaises(SystemExit):
            android.build_go_binaries("go", self.clang, TAG, "arm64-v8a")
        run.assert_not_called()
        self.assertFalse(self.jnilibs.exists())

    def test_android_preserves_flags_and_requires_cgo(self):
        prior = "-tags=example -p=8 -mod=mod"
        with patch.dict(os.environ, {"GOFLAGS": prior, "CGO_ENABLED": "0", "GOMAXPROCS": "8"}):
            env = android.go_environment(self.clang, "arm64-v8a")
        self.assertEqual(env["GOFLAGS"], prior)
        self.assertEqual(env["CGO_ENABLED"], "1")
        self.assertEqual(env["GOOS"], "android")
        self.assertEqual(env["GOMAXPROCS"], "2")

    def test_android_refuses_netgo_even_with_cgo(self):
        for flags in ("-tags=netgo", "-tags=other,netgo", "-tags='other netgo'", "-tags netgo"):
            with self.subTest(flags=flags), patch.dict(os.environ, {"GOFLAGS": flags}), self.assertRaises(SystemExit):
                android.go_environment(self.clang, "arm64-v8a")

    def test_android_build_all_helpers_are_verified(self):
        calls = []
        def fake_build(command, **kwargs):
            calls.append((command, kwargs))
            Path(command[command.index("-o") + 1]).write_bytes(b"fixture binary")
        with patch.dict(os.environ, {"GOFLAGS": "-tags=example"}), \
                patch.object(android, "run", side_effect=fake_build), \
                patch.object(android, "verify_go_binary") as verify:
            android.build_go_binaries("go", self.clang, TAG, "arm64-v8a")
        self.assertEqual(len(calls), 1 + len(android.HELPER_PACKAGES))
        self.assertEqual(verify.call_count, len(calls))
        for command, kwargs in calls:
            self.assertIn("-p=1", command)
            self.assertIn("-mod=readonly", command)
            self.assertIn("-trimpath", command)
            self.assertEqual(kwargs["env"]["GOFLAGS"], "-tags=example")
            self.assertIn("ReleaseKey=" + PUBLIC_KEY, command[command.index("-ldflags") + 1])

    def test_android_accepts_verified_bionic_artifact_without_running_it(self):
        output = artifact(self.root / "libwb2gateway.so")
        with patch.object(android.subprocess, "run", side_effect=[result(metadata()), result(ELF_BIONIC)]) as run:
            android.verify_go_binary("go", output, self.clang, "arm64-v8a", TAG, PUBLIC_KEY)
        self.assertEqual(run.call_args_list[0].args[0][1:3], ["version", "-m"])
        self.assertEqual(Path(run.call_args_list[1].args[0][0]), self.readelf)

    def test_android_accepts_artifact_when_build_metadata_omits_ldflags(self):
        output = artifact(self.root / "libwb2gateway.so")
        raw = metadata()
        self.assertNotIn("-ldflags", raw)
        with patch.object(android.subprocess, "run",
                          side_effect=[result(raw), result(ELF_BIONIC)]) as run:
            android.verify_go_binary("go", output, self.clang, "arm64-v8a", TAG, PUBLIC_KEY)
        self.assertEqual(run.call_count, 2)

    def test_android_rejects_gateway_missing_injected_literals(self):
        output = self.root / "libwb2gateway.so"
        output.write_bytes(b"no injected values here")
        with patch.object(android.subprocess, "run",
                          side_effect=[result(metadata()), result(ELF_BIONIC)]), self.assertRaises(SystemExit):
            android.verify_go_binary("go", output, self.clang, "arm64-v8a", TAG, PUBLIC_KEY)

    def test_android_rejects_stale_or_non_bionic_build(self):
        for change in ({"cgo": "0"}, {"goarch": "amd64"}, {"goos": "linux"},
                       {"tag": "v3.16.0"}, {"key": "22" * 32}, {"tags": "netgo"}):
            output = artifact(self.root / "libwb2gateway.so")
            metadata_change = {k: v for k, v in change.items() if k != "key"}
            if "key" in change:
                output.write_bytes(("fixture:" + TAG + ":" + "33" * 32).encode("ascii"))
            elif "tag" in change:
                # A stale build embeds the stale tag; metadata alone no longer
                # carries it, so the artifact bytes are the rejecting signal.
                output.write_bytes(("fixture:" + change["tag"] + ":" + PUBLIC_KEY).encode("ascii"))
            with self.subTest(change=change), patch.object(
                    android.subprocess, "run",
                    side_effect=[result(metadata(**metadata_change)), result(ELF_BIONIC)]), \
                    self.assertRaises(SystemExit):
                android.verify_go_binary("go", output, self.clang, "arm64-v8a", TAG, PUBLIC_KEY)

    def test_android_rejects_elf_without_system_dns(self):
        for elf in (ELF_BIONIC.replace("getaddrinfo", "another_symbol"),
                    ELF_BIONIC.replace("libc.so", "libc.so.6"),
                    ELF_BIONIC.replace("/system/bin/linker64", "/lib/ld-linux.so"), ""):
            with self.subTest(elf=elf[:30]), \
                    patch.object(android.subprocess, "run", side_effect=[result(metadata()), result(elf)]), \
                    self.assertRaises(SystemExit):
                android.verify_go_binary("go", artifact(self.root / "libwb2gateway.so"), self.clang,
                                         "arm64-v8a", TAG, PUBLIC_KEY)

    def test_skip_go_missing_helper_is_rejected(self):
        root = self.jnilibs / "arm64-v8a"
        root.mkdir(parents=True)
        (root / "libwb2gateway.so").write_bytes(b"fixture")
        with patch.object(android, "verify_go_binary"), self.assertRaises(SystemExit):
            android.verify_jnilibs("go", self.clang, "arm64-v8a", TAG)

    def test_version_code_never_falls_back_or_overflows_segments(self):
        self.assertEqual(android.version_code(TAG), 3_016_001)
        self.assertGreater(android.version_code("v3.17.0"), android.version_code(TAG))
        for tag in ("dev", "v3.16", "v3.16.1-rc1", "v3.016.1", "v3.16.-1", "v3.16.0",
                    "v2.1000.0", "v3.15.1001", "v9999.0.0", "v3.16.1 extra"):
            with self.subTest(tag=tag), self.assertRaises(SystemExit):
                android.version_code(tag)

    def test_missing_version_file_does_not_become_dev(self):
        (self.gateway / "VERSION").unlink()
        with self.assertRaises(SystemExit):
            android.read_version()

    def test_apk_verification_preserves_signer_identity(self):
        tools = self.root / "sdk" / "build-tools" / "33.0.2"
        tools.mkdir(parents=True)
        (tools / ("apksigner.bat" if sys.platform == "win32" else "apksigner")).write_text("fixture")
        good = "Signer #1 certificate SHA-256 digest: " + android.LEGACY_SIGNER_SHA256 + "\n"
        with patch.object(android.subprocess, "run", return_value=result(good)):
            android.verify_apk_signature(self.root / "test.apk", self.root / "sdk")
        for raw, code in ((good, 1), (good.replace(android.LEGACY_SIGNER_SHA256, "22" * 32), 0),
                          (good + good.replace("#1", "#2"), 0), ("", 0)):
            with self.subTest(code=code, length=len(raw)), \
                    patch.object(android.subprocess, "run", return_value=result(raw, code)), self.assertRaises(SystemExit):
                android.verify_apk_signature(self.root / "test.apk", self.root / "sdk")

    def test_skip_go_packaging_still_validates_artifacts_before_gradle(self):
        with patch.object(android, "verify_jnilibs", side_effect=SystemExit("unverified")) as verify, \
                patch.object(android, "run") as run, self.assertRaises(SystemExit):
            android.build_apk("gradle", "release", "arm64-v8a", TAG, "go", self.clang)
        verify.assert_called_once()
        run.assert_not_called()

    def test_apk_cannot_downgrade_current_source_version(self):
        (self.gateway / "VERSION").write_text("v3.17.0")
        with patch.object(android, "run") as run, self.assertRaises(SystemExit):
            android.build_apk("gradle", "release", "arm64-v8a", TAG, "go", self.clang)
        run.assert_not_called()

    def test_apk_is_verified_before_copying_to_external_distribution(self):
        gradle_output = self.root / "gradle-output"
        dist = self.root / "distribution"
        apk = gradle_output / "outputs" / "apk" / "release" / "app-release.apk"
        commands = []
        def fake_gradle(command):
            commands.append(command)
            apk.parent.mkdir(parents=True)
            apk.write_bytes(b"synthetic signed APK")
        def verify_before_copy(path, sdk):
            self.assertEqual(path, apk)
            self.assertFalse(dist.exists())
        with patch.object(android, "GRADLE_OUTPUT", gradle_output), patch.object(android, "DIST", dist), \
                patch.object(android, "verify_jnilibs"), patch.object(android, "run", side_effect=fake_gradle), \
                patch.object(android, "find_sdk", return_value=None), \
                patch.object(android, "verify_apk_signature", side_effect=verify_before_copy) as verify:
            output = android.build_apk("gradle", "release", "arm64-v8a", TAG, "go", self.clang)
        verify.assert_called_once()
        self.assertEqual(output.parent, dist)
        self.assertEqual(output.read_bytes(), apk.read_bytes())
        self.assertIn("-PversionCode=3016001", commands[0])
        self.assertIn("-PjniLibsDir=" + str(self.jnilibs.resolve()), commands[0])
        self.assertIn("-PbuildOutputDir=" + str(gradle_output.resolve()), commands[0])

    def test_apk_with_wrong_signer_never_reaches_distribution(self):
        gradle_output = self.root / "gradle-output"
        dist = self.root / "distribution"
        apk = gradle_output / "outputs" / "apk" / "release" / "app-release.apk"
        apk.parent.mkdir(parents=True)
        apk.write_bytes(b"synthetic untrusted APK")
        with patch.object(android, "GRADLE_OUTPUT", gradle_output), patch.object(android, "DIST", dist), \
                patch.object(android, "verify_jnilibs"), patch.object(android, "run"), \
                patch.object(android, "find_sdk", return_value=None), \
                patch.object(android, "verify_apk_signature", side_effect=SystemExit("signer mismatch")), \
                self.assertRaises(SystemExit):
            android.build_apk("gradle", "release", "arm64-v8a", TAG, "go", self.clang)
        self.assertFalse(dist.exists())

    def test_explicit_invalid_tools_do_not_fall_back(self):
        with patch.dict(os.environ, {"GO": str(self.root / "missing-go")}), self.assertRaises(SystemExit):
            android.find_go()
        with patch.dict(os.environ, {"ANDROID_NDK_HOME": str(self.root / "missing-ndk")}), self.assertRaises(SystemExit):
            android.find_ndk()


if __name__ == "__main__":
    unittest.main()
