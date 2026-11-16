"""All data is generated locally; never open real account directories."""
import contextlib
import ctypes
import importlib.util
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("migration", Path(__file__).with_name("migrate.py"))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


def create_closed_socket(path):
    if os.name != "nt":
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
            server.bind(str(path))
        return
    # CPython on Windows may not expose AF_UNIX although Windows/Go support it.
    # Exercise a real Windows AF_UNIX endpoint through Winsock, not a fake tag.
    api = ctypes.WinDLL("Ws2_32", use_last_error=True)
    api.socket.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_int]
    api.socket.restype = ctypes.c_size_t
    api.bind.argtypes = [ctypes.c_size_t, ctypes.c_void_p, ctypes.c_int]
    api.closesocket.argtypes = [ctypes.c_size_t]
    raw = str(path).encode("utf-8")
    if len(raw) >= 108:
        raise RuntimeError("synthetic_socket_path_too_long")
    handle = api.socket(1, socket.SOCK_STREAM, 0)
    if handle == ctypes.c_size_t(-1).value:
        raise RuntimeError("synthetic_socket_creation_failed")
    try:
        address = ctypes.create_string_buffer(b"\x01\x00" + raw + b"\0" * (108 - len(raw)), 110)
        if api.bind(handle, address, len(address)) != 0:
            raise RuntimeError("synthetic_socket_bind_failed")
    finally:
        api.closesocket(handle)


class MigrationTests(unittest.TestCase):
    def setUp(self):
        base = Path(os.environ.get("LOOMGATE_WORK_ROOT", "D:/LoomGate-work")) / "migration-tests"
        base.mkdir(parents=True, exist_ok=True)
        self.root = Path(tempfile.mkdtemp(prefix="fixture-", dir=base))
        self.source, self.dest = self.root / "old", self.root / "new"
        for name in m.CATEGORIES:
            (self.source / name).mkdir(parents=True)
        self.config = {"auth_dir": "./auths", "api_keys_file": "./data/api_keys.json",
                       "state_file": "./data/state.json", "unknown": {"future": 7},
                       "update": {"enabled": True, "repo": "example/unchanged"}}
        self.write("config/config.json", self.config)
        self.write("data/api_keys.json", {"version": 1, "keys": [{"id": "fixture", "name": "fixture",
                   "sha256": hashlib.sha256(b"synthetic-fixture-only").hexdigest(), "encrypted_key": "synthetic-only"}]})
        (self.source / "data/api_keys.json.enc-key").write_bytes(bytes(range(32)))
        self.write("data/state.json", {"saved_at": "fixture", "future": [1, 2]})
        self.write("auths/workbuddy-fixture.json", {"fixture": True, "unknown": "preserve"})
        self.write("panel-data/credentials.json", {"fixture": True})

    def tearDown(self):
        shutil.rmtree(self.root)

    def write(self, path, value):
        target = self.source / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(json.dumps(value), encoding="utf-8")

    def test_offline_management_socket_excluded_without_following_links(self):
        for explicit in [False, True]:
            endpoint = self.source / "data/api_keys.sock"
            if explicit:
                self.config["api_keys_socket"] = str(endpoint)
                self.write("config/config.json", self.config)
            create_closed_socket(endpoint)
            target = self.root / ("explicit" if explicit else "default")
            report = m.migrate(self.source, target)
            self.assertEqual(report["excluded"]["runtime_sockets"], 1)
            self.assertFalse(target.exists())
            m.migrate(self.source, target, apply=True, offline=True)
            self.assertFalse((target / "data/api_keys.sock").exists())
            self.assertEqual((self.source / "data/state.json").read_bytes(), (target / "data/state.json").read_bytes())
            self.assertTrue(endpoint.exists())
            endpoint.unlink()

    def test_other_socket_and_socket_named_regular_file_are_not_silently_dropped(self):
        path = self.source / "data/unknown.sock"
        create_closed_socket(path)
        with self.assertRaises(m.MigrationError):
            m.migrate(self.source, self.dest)
        path.unlink()
        (self.source / "data/api_keys.sock").write_text("synthetic important data")
        with self.assertRaises(m.MigrationError):
            m.migrate(self.source, self.dest)

    def test_management_socket_symlink_cannot_escape_or_hide_data(self):
        endpoint = self.source / "data/api_keys.sock"
        endpoint.symlink_to(self.source / "data/state.json")
        with self.assertRaisesRegex(m.MigrationError, "link_or_junction"):
            m.migrate(self.source, self.dest)

    def test_dry_run_has_no_writes_and_redacts_names(self):
        before = {p.relative_to(self.source): p.read_bytes() for p in self.source.rglob("*") if p.is_file()}
        report = m.migrate(self.source, self.dest)
        self.assertFalse(self.dest.exists())
        self.assertEqual(report["encrypted_key_records"], 1)
        self.assertNotIn("workbuddy-fixture", json.dumps(report))
        self.assertEqual(before, {p.relative_to(self.source): p.read_bytes() for p in self.source.rglob("*") if p.is_file()})

    def test_copy_preserves_bytes_master_unknowns_and_rollback(self):
        m.migrate(self.source, self.dest, apply=True, offline=True)
        for name in ("data/api_keys.json", "data/api_keys.json.enc-key", "auths/workbuddy-fixture.json", "panel-data/credentials.json"):
            self.assertEqual((self.source / name).read_bytes(), (self.dest / name).read_bytes())
        copied = json.loads((self.dest / "config/config.json").read_text())
        self.assertEqual(copied["unknown"], self.config["unknown"])
        self.assertEqual(copied["update"]["repo"], "example/unchanged")
        self.assertFalse(copied["update"]["enabled"])
        self.assertTrue(json.loads((self.source / "config/config.json").read_text())["update"]["enabled"])
        m.rollback(self.dest, offline=True)
        self.assertFalse(self.dest.exists())
        self.assertTrue(self.dest.with_name("new.rolled-back").is_dir())
        self.assertTrue(self.source.is_dir())

    def test_missing_master_fails_before_destination_creation(self):
        (self.source / "data/api_keys.json.enc-key").unlink()
        with self.assertRaisesRegex(m.MigrationError, "original_master_key"):
            m.migrate(self.source, self.dest)
        self.assertFalse(self.dest.exists())

    def test_external_path_refused(self):
        self.config["auth_dir"] = "../outside"
        self.write("config/config.json", self.config)
        with self.assertRaisesRegex(m.MigrationError, "external_config_path"):
            m.migrate(self.source, self.dest)

    def test_overlap_existing_destination_and_missing_offline_refused(self):
        with self.assertRaises(m.MigrationError):
            m.migrate(self.source, self.source / "copy")
        self.dest.mkdir()
        with self.assertRaises(m.MigrationError):
            m.migrate(self.source, self.dest)
        with self.assertRaisesRegex(m.MigrationError, "offline"):
            m.migrate(self.source, self.root / "another", apply=True)

    def test_legacy_config_and_absolute_paths_are_mapped(self):
        (self.source / "config/config.json").rename(self.source / "config.json")
        self.config["auth_dir"] = str(self.source / "auths")
        self.write("config.json", self.config)
        report = m.migrate(self.source, self.dest, apply=True, offline=True)
        self.assertIn("auth_dir", report["mapped_fields"])
        self.assertEqual(json.loads((self.dest / "config/config.json").read_text())["auth_dir"], "./auths")

    def test_runtime_updates_and_locks_are_not_reactivated(self):
        self.write("data/updates/current", {"fixture": "old binary"})
        self.write("data/state.json.lock", {})
        report = m.migrate(self.source, self.dest, apply=True, offline=True)
        self.assertFalse((self.dest / "data/updates").exists())
        self.assertFalse((self.dest / "data/state.json.lock").exists())
        self.assertEqual(report["excluded"]["runtime_updates"], 1)

    def test_cli_errors_do_not_expose_os_paths(self):
        output = io.StringIO()
        with contextlib.redirect_stderr(output):
            rc = m.main(["copy", "--source", str(self.root / "private-name"), "--destination", str(self.dest)])
        self.assertEqual(rc, 2)
        self.assertNotIn("private-name", output.getvalue())

    def test_nested_paths_are_remapped_and_external_input_rejected(self):
        for name in ("prompt.txt", "device-input.txt"):
            (self.source / "config" / name).write_text("synthetic fixture", encoding="utf-8")
        self.config["prompt"] = {"mode": "custom", "file": str(self.source / "config/prompt.txt")}
        self.config["upstream"] = {"device_token_file": str(self.source / "config/device-input.txt"),
                                   "channel_dump_dir": str(self.source / "data/new-capture")}
        self.write("config/config.json", self.config)
        report = m.migrate(self.source, self.dest, apply=True, offline=True)
        copied = json.loads((self.dest / "config/config.json").read_text(encoding="utf-8"))
        self.assertEqual(copied["prompt"]["file"], "./config/prompt.txt")
        self.assertEqual(copied["upstream"]["device_token_file"], "./config/device-input.txt")
        self.assertEqual(copied["upstream"]["channel_dump_dir"], "./data/new-capture")
        self.assertIn("prompt.file", report["mapped_fields"])
        self.config["prompt"]["file"] = str(self.root / "outside.txt")
        self.write("config/config.json", self.config)
        with self.assertRaisesRegex(m.MigrationError, "external_config_path"):
            m.migrate(self.source, self.root / "other")

    def test_root_configuration_symlink_rejected_before_copy(self):
        (self.source / "config/config.json").unlink()
        target = self.root / "outside.json"
        target.write_text(json.dumps(self.config), encoding="utf-8")
        try:
            (self.source / "config.json").symlink_to(target)
        except OSError:
            self.skipTest("symlink creation unavailable")
        with self.assertRaisesRegex(m.MigrationError, "link_or_junction"):
            m.migrate(self.source, self.dest)
        self.assertFalse(self.dest.exists())

    def test_hardlinked_account_file_rejected(self):
        target = self.root / "outside.json"
        target.write_text('{"fixture":true}', encoding="utf-8")
        os.link(target, self.source / "auths/linked-fixture.json")
        with self.assertRaisesRegex(m.MigrationError, "hardlinked_data"):
            m.migrate(self.source, self.dest)

    def test_registry_invalid_version_record_or_duplicate_rejected(self):
        original = json.loads((self.source / "data/api_keys.json").read_text())
        bad_cases = ({"version": 999, "keys": []}, {"version": 1, "keys": [{"id": "fixture"}]},
                     {"version": 1, "keys": original["keys"] * 2})
        for doc in bad_cases:
            with self.subTest(case=len(doc["keys"])):
                self.write("data/api_keys.json", doc)
                with self.assertRaisesRegex(m.MigrationError, "invalid_key_registry"):
                    m.migrate(self.source, self.dest)
        self.assertFalse(self.dest.exists())

    def test_rollback_rejects_unrecognized_or_changed_backup(self):
        fake = self.root / "fake"
        fake.mkdir()
        (fake / m.MARKER).write_text('{"mode":"copied"}', encoding="utf-8")
        with self.assertRaisesRegex(m.MigrationError, "unrecognized"):
            m.rollback(fake, offline=True)
        self.assertTrue(fake.is_dir())
        m.migrate(self.source, self.dest, apply=True, offline=True)
        (self.dest / "config/config.json.pre-migration").write_text('{}', encoding="utf-8")
        with self.assertRaisesRegex(m.MigrationError, "backup_missing_or_changed"):
            m.rollback(self.dest, offline=True)
        self.assertTrue(self.dest.is_dir())

    def test_interrupted_copy_can_be_parked_and_retried(self):
        original = shutil.copyfile
        calls = 0
        def interrupt(src, dst, *args, **kwargs):
            nonlocal calls
            calls += 1
            if calls == 2:
                raise OSError("synthetic interruption")
            return original(src, dst, *args, **kwargs)
        with patch.object(shutil, "copyfile", side_effect=interrupt):
            with self.assertRaises(OSError):
                m.migrate(self.source, self.dest, apply=True, offline=True)
        with self.assertRaisesRegex(m.MigrationError, "offline"):
            m.recover(self.dest)
        result = m.recover(self.dest, offline=True)
        self.assertTrue(result["partial_data_preserved"])
        self.assertTrue(self.dest.with_name("new.migration-failed").is_dir())
        self.assertFalse(self.dest.with_name("new.migration-staging").exists())
        m.migrate(self.source, self.dest, apply=True, offline=True)
        self.assertTrue((self.dest / m.MARKER).is_file())

    def test_custom_update_directory_is_excluded(self):
        self.config["update"]["dir"] = str(self.source / "data/custom-updater")
        self.write("config/config.json", self.config)
        self.write("data/custom-updater/current", {"fixture": "old binary"})
        report = m.migrate(self.source, self.dest, apply=True, offline=True)
        self.assertFalse((self.dest / "data/custom-updater").exists())
        self.assertEqual(report["excluded"]["runtime_updates"], 1)

    def test_missing_output_data_file_remains_valid_migration_input(self):
        self.config["usage_file"] = "./data/not-created.json"
        self.write("config/config.json", self.config)
        m.migrate(self.source, self.dest)

    @unittest.skipUnless(os.name == "nt", "Windows ACL only")
    def test_windows_copied_credentials_have_private_acl(self):
        shell = shutil.which("pwsh") or shutil.which("powershell")
        if not shell:
            self.skipTest("PowerShell unavailable for ACL verification")
        m.migrate(self.source, self.dest, apply=True, offline=True)
        for target in (self.dest, self.dest / "auths/workbuddy-fixture.json", self.dest / "data/api_keys.json.enc-key"):
            literal = str(target).replace("'", "''")
            script = "$ErrorActionPreference='Stop'; $allowed=@('S-1-3-4','S-1-5-18','S-1-5-32-544'); $acl=Get-Acl -LiteralPath '" + literal + "'; $unsafe=@($acl.Access | Where-Object { $_.AccessControlType -eq 'Allow' -and $_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value -notin $allowed }); [pscustomobject]@{unsafe=$unsafe.Count; entries=$acl.Access.Count} | ConvertTo-Json -Compress"
            env = dict(os.environ)
            if Path(shell).name.lower() == "powershell.exe":
                env.pop("PSModulePath", None)
            result = subprocess.run([shell, "-NoProfile", "-Command", script], capture_output=True, text=True, env=env, timeout=30)
            self.assertEqual(result.returncode, 0, "ACL query failed")
            actual = json.loads(result.stdout)
            self.assertGreater(actual["entries"], 0)
            self.assertEqual(actual["unsafe"], 0, "copied credentials grant access outside trusted principals")

    def test_malformed_path_configuration_has_safe_cli_error(self):
        self.config["api_keys_file"] = ["synthetic-private-value"]
        self.write("config/config.json", self.config)
        output = io.StringIO()
        with contextlib.redirect_stderr(output):
            result = m.main(["copy", "--source", str(self.source), "--destination", str(self.dest)])
        self.assertEqual(result, 2)
        self.assertNotIn("synthetic-private-value", output.getvalue())

    def test_configured_data_in_update_directory_is_rejected(self):
        self.config["state_file"] = "./data/updates/state.json"
        self.write("config/config.json", self.config)
        with self.assertRaisesRegex(m.MigrationError, "inside_excluded_updates"):
            m.migrate(self.source, self.dest)

    def test_empty_configured_account_directory_is_preserved(self):
        self.config["auth_dir"] = "./auths/empty"
        (self.source / "auths/empty").mkdir()
        self.write("config/config.json", self.config)
        m.migrate(self.source, self.dest, apply=True, offline=True)
        self.assertTrue((self.dest / "auths/empty").is_dir())

    def test_commit_failure_can_be_recovered_without_deleting_copy(self):
        with patch.object(Path, "rename", side_effect=OSError("synthetic rename failure")):
            with self.assertRaises(OSError):
                m.migrate(self.source, self.dest, apply=True, offline=True)
        result = m.recover(self.dest, offline=True)
        self.assertTrue(result["partial_data_preserved"])
        self.assertTrue((self.dest.with_name("new.migration-failed") / "config/config.json").is_file())

    def test_null_optional_sections_remain_migratable(self):
        self.config["prompt"] = None
        self.config["upstream"] = None
        self.write("config/config.json", self.config)
        m.migrate(self.source, self.dest, apply=True, offline=True)

    def test_invalid_limits_or_models_do_not_receive_success_report(self):
        original = json.loads((self.source / "data/api_keys.json").read_text())
        for field, value in (("models", {}), ("limits", {"queue_timeout_seconds": 1, "max_concurrent": 0})):
            doc = json.loads(json.dumps(original))
            doc["keys"][0][field] = value
            self.write("data/api_keys.json", doc)
            with self.assertRaisesRegex(m.MigrationError, "invalid_key_registry"):
                m.migrate(self.source, self.dest)


if __name__ == "__main__":
    unittest.main()
