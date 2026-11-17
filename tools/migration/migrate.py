#!/usr/bin/env python3
"""Offline copy migration. Never starts the gateway or prints private file names."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys

# The migration is always run with `python -I`, which omits the script
# directory from sys.path. Load the sibling converter by explicit path so the
# offline tool does not depend on the caller's working directory.
import importlib.util as _importlib_util

_legacy_spec = _importlib_util.spec_from_file_location(
    "loomgate_legacy_schedule", Path(__file__).with_name("legacy_schedule.py"))
legacy_schedule = _importlib_util.module_from_spec(_legacy_spec)
_legacy_spec.loader.exec_module(legacy_schedule)

FORMAT = 2
MARKER = ".loomgate-migration.json"
CATEGORIES = ("config", "auths", "auths-trash", "data", "panel-data")
PATH_FIELDS = ("auth_dir", "api_keys_file", "api_keys_socket", "usage_file", "state_file")
NESTED_PATH_FIELDS = (("prompt", "file", True), ("upstream", "device_token_file", True),
                      ("upstream", "channel_dump_dir", False))
MAX_CONFIG_BYTES = 8 * 1024 * 1024
MAX_KEYS = 256
SHA256 = re.compile(r"^[0-9a-fA-F]{64}$")


class MigrationError(Exception):
    """Only constant, non-sensitive messages may cross the CLI boundary."""


def reject_links(path: Path) -> None:
    # is_junction is absent before Python 3.12. The Windows attribute also catches
    # other reparse points, rather than resolving an untrusted ancestor first.
    for part in (path.absolute(), *path.absolute().parents):
        try:
            info = part.lstat()
        except FileNotFoundError:
            continue
        if (stat.S_ISLNK(info.st_mode) or
                getattr(info, "st_file_attributes", 0) & 0x400):
            raise MigrationError("link_or_junction_rejected")


def regular_file(path: Path) -> None:
    reject_links(path)
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode):
        raise MigrationError("non_regular_data_rejected")
    if info.st_nlink > 1:
        raise MigrationError("hardlinked_data_requires_export")


def runtime_socket(path: Path) -> bool:
    """Recognize Unix endpoints without following arbitrary reparse points."""
    try:
        info = path.lstat()
    except FileNotFoundError:
        return False
    return stat.S_ISSOCK(info.st_mode) or getattr(info, "st_reparse_tag", 0) == 0x80000023


def socket_path(value: str, source: Path) -> Path:
    path = Path(value)
    candidate = path if path.is_absolute() else source / path
    reject_links(candidate.parent)
    # AF_UNIX is a Windows reparse tag, but it has no link target to follow.
    # Canonicalize only its verified parent; never resolve the endpoint.
    candidate = candidate.parent.resolve() / candidate.name
    try:
        relative = candidate.relative_to(source)
    except ValueError:
        raise MigrationError("external_config_path_requires_export") from None
    if not relative.parts or relative.parts[0] not in CATEGORIES:
        raise MigrationError("config_path_outside_migration_categories")
    return candidate


def private_directory(path: Path) -> None:
    path.mkdir(mode=0o700)
    if os.name == "nt":
        # Numeric SIDs require * in icacls. No localized account names and no
        # command output: an error could otherwise expose a private path.
        try:
            result = subprocess.run(
                ["icacls", str(path), "/inheritance:r", "/grant:r",
                 "*S-1-3-4:(OI)(CI)F", "*S-1-5-18:(OI)(CI)F",
                 "*S-1-5-32-544:(OI)(CI)F", "/Q"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False, timeout=30)
        except (OSError, subprocess.SubprocessError):
            raise MigrationError("private_directory_acl_failed") from None
        if result.returncode:
            raise MigrationError("private_directory_acl_failed")
    else:
        path.chmod(0o700)


def object_file(path: Path) -> dict:
    regular_file(path)
    if path.stat().st_size > MAX_CONFIG_BYTES:
        raise MigrationError("configuration_or_registry_too_large")
    try:
        doc = json.loads(path.read_text(encoding="utf-8-sig"))
    except (ValueError, UnicodeError):
        raise MigrationError("invalid_json_object") from None
    if not isinstance(doc, dict):
        raise MigrationError("invalid_json_object")
    return doc


def digest(path: Path) -> str:
    regular_file(path)
    h = hashlib.sha256()
    try:
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                h.update(chunk)
    except PermissionError:
        # A live official client holds Chromium's profile lock and other files.
        # Migration is an offline operation; say so instead of a generic OS
        # failure that hides the required action ("close the client").
        raise MigrationError("source_in_use_close_the_gateway_and_client") from None
    return h.hexdigest()


def guarded_root(path: Path, exists: bool) -> Path:
    original = path.absolute()
    reject_links(original)
    resolved = original.resolve()
    if resolved == Path(resolved.anchor):
        raise MigrationError("filesystem_root_rejected")
    if exists and not resolved.is_dir():
        raise MigrationError("source_directory_missing")
    return resolved


def mapped_path(value: str, source: Path, *, required: bool = False) -> str:
    """Only paths inside the selected root can become portable relative paths."""
    path = Path(value)
    # Foreign absolute paths require an explicit source-side export first.
    if (len(value) > 1 and value[1] == ":") and os.name != "nt":
        raise MigrationError("foreign_absolute_path_requires_export")
    candidate = path if path.is_absolute() else source / path
    reject_links(candidate)
    resolved = candidate.resolve()
    try:
        relative = resolved.relative_to(source)
    except ValueError:
        raise MigrationError("external_config_path_requires_export") from None
    if not relative.parts or relative.parts[0] not in CATEGORIES:
        raise MigrationError("config_path_outside_migration_categories")
    if required and not resolved.is_file():
        raise MigrationError("configured_data_path_missing")
    return "./" + relative.as_posix()


def validate_registry(doc: dict) -> list[dict]:
    records = doc.get("keys")
    if type(doc.get("version")) is not int or doc["version"] != 1 or not isinstance(records, list) or len(records) > MAX_KEYS:
        raise MigrationError("invalid_key_registry_schema")
    seen_ids, seen_digests = set(), set()
    for row in records:
        if not isinstance(row, dict):
            raise MigrationError("invalid_key_registry_record")
        identity, checksum = row.get("id"), row.get("sha256")
        name, note = row.get("name", ""), row.get("note", "")
        if (not isinstance(identity, str) or not identity or "/" in identity or "\\" in identity or
                identity in seen_ids or not isinstance(checksum, str) or not SHA256.fullmatch(checksum) or checksum in seen_digests or
                not isinstance(name, str) or name != name.strip() or not 1 <= len(name) <= 64 or
                not isinstance(note, str) or len(note) > 256 or
                any(ord(c) < 32 or 127 <= ord(c) <= 159 or c in "\u2028\u2029" for c in name + note)):
            raise MigrationError("invalid_key_registry_record")
        models = row.get("models")
        if models is None:
            models = []
        if not isinstance(models, list) or len(models) > 64 or any(
                not isinstance(model, str) or not 1 <= len(model) <= 64 or
                any(c.isspace() or ord(c) < 32 or 127 <= ord(c) <= 159 for c in model) for model in models):
            raise MigrationError("invalid_key_registry_models")
        if len(set(models)) != len(models):
            raise MigrationError("invalid_key_registry_models")
        limits = row.get("limits")
        if limits is not None:
            if not isinstance(limits, dict):
                raise MigrationError("invalid_key_registry_limits")
            for field, maximum in (("requests_per_minute", 60000), ("max_concurrent", 256), ("queue_timeout_seconds", 30)):
                value = limits.get(field, 0)
                if type(value) is not int or not 0 <= value <= maximum:
                    raise MigrationError("invalid_key_registry_limits")
            if limits.get("queue_timeout_seconds", 0) and not limits.get("max_concurrent", 0):
                raise MigrationError("invalid_key_registry_limits")
        seen_ids.add(identity)
        seen_digests.add(checksum)
    return records


def path_binding(path: Path) -> str:
    return hashlib.sha256(os.path.normcase(str(path.resolve())).encode("utf-8")).hexdigest()


def write_marker(stage: Path, report: dict, destination: Path, mode: str) -> None:
    marker = stage / MARKER
    document = {**report, "mode": mode, "destination_binding": path_binding(destination)}
    marker.write_text(json.dumps(document, indent=2) + "\n", encoding="utf-8")
    marker.chmod(0o600)


def recognized_marker(root: Path, destination: Path, mode: str) -> dict:
    marker = root / MARKER
    if not marker.is_file():
        raise MigrationError("unrecognized_migration_destination")
    document = object_file(marker)
    if (document.get("format") != FORMAT or document.get("mode") != mode or
            document.get("destination_binding") != path_binding(destination) or
            not isinstance(document.get("inventory_sha256"), str) or not SHA256.fullmatch(document["inventory_sha256"]) or
            type(document.get("file_count")) is not int or document["file_count"] < 1 or
            not isinstance(document.get("source_config_sha256"), str) or not SHA256.fullmatch(document["source_config_sha256"])):
        raise MigrationError("unrecognized_migration_destination")
    if mode == "copied":
        backup = root / "config/config.json.pre-migration"
        if not backup.is_file() or digest(backup) != document["source_config_sha256"]:
            raise MigrationError("migration_backup_missing_or_changed")
    return document


def inspect(source: Path, destination: Path) -> tuple[dict, list[tuple[Path, Path, str]], dict | None]:
    source = guarded_root(source, True)
    destination = guarded_root(destination, False)
    if source == destination or source in destination.parents or destination in source.parents:
        raise MigrationError("source_destination_overlap")
    if destination.exists():
        raise MigrationError("destination_must_not_exist")
    files: list[tuple[Path, Path, str]] = []
    counts = {name: {"files": 0, "bytes": 0} for name in CATEGORIES}
    excluded = {"runtime_updates": 0, "transient_files": 0, "runtime_sockets": 0}
    config_candidates = (source / "config/config.json", source / "config.json")
    for candidate in config_candidates:
        reject_links(candidate)
    configs = [p for p in config_candidates if p.is_file()]
    if len(configs) != 1:
        raise MigrationError("exactly_one_configuration_required")
    config_path = configs[0]
    config = object_file(config_path)
    registry_value = config.get("api_keys_file") or "./data/api_keys.json"
    if not isinstance(registry_value, str):
        raise MigrationError("invalid_configuration_path")
    endpoint_value = config.get("api_keys_socket") or str(Path(registry_value).parent / "api_keys.sock")
    if not isinstance(endpoint_value, str):
        raise MigrationError("invalid_configuration_path")
    endpoint = socket_path(endpoint_value, source)
    mapped = dict(config)
    remapped = []
    for field in PATH_FIELDS:
        value = config.get(field)
        if value is not None and not isinstance(value, str):
            raise MigrationError("invalid_configuration_path")
        if isinstance(value, str) and value.strip():
            next_value = ("./" + endpoint.relative_to(source).as_posix()
                          if field == "api_keys_socket" and runtime_socket(endpoint)
                          else mapped_path(value, source))
            if next_value != value:
                remapped.append(field)
            mapped[field] = next_value
    for section, field, required in NESTED_PATH_FIELDS:
        parent = config.get(section)
        if parent is not None and not isinstance(parent, dict):
            raise MigrationError("invalid_configuration_section")
        if isinstance(parent, dict) and parent.get(field) is not None and not isinstance(parent[field], str):
            raise MigrationError("invalid_configuration_path")
        if isinstance(parent, dict) and isinstance(parent.get(field), str) and parent[field].strip():
            value = mapped_path(parent[field], source, required=required)
            mapped[section] = {**mapped.get(section, {}), field: value}
            if value != parent[field]:
                remapped.append(section + "." + field)
    update = config.get("update")
    update_roots = {source / "data/updates"}
    if isinstance(update, dict):
        mapped["update"] = dict(update)
        # A copied runtime must not start an inherited updater on first launch.
        mapped["update"]["enabled"] = False
        if update.get("dir") is not None and not isinstance(update["dir"], str):
            raise MigrationError("invalid_update_directory")
        if update.get("dir"):
            original_update = Path(update["dir"])
            original_update = original_update if original_update.is_absolute() else source / original_update
            reject_links(original_update)
            original_update = original_update.resolve()
            if original_update == source or original_update in {source / cat for cat in CATEGORIES}:
                raise MigrationError("update_directory_overlaps_data_category")
            update_roots.add(original_update)
            mapped["update"]["dir"] = "./data/updates"
            remapped.append("update.dir")
    else:
        if update is not None:
            raise MigrationError("invalid_configuration_section")
        mapped["update"] = {"enabled": False}
    for path_text in [mapped.get(field) for field in PATH_FIELDS] + [
            (mapped.get(section) or {}).get(field) for section, field, _ in NESTED_PATH_FIELDS]:
        if not isinstance(path_text, str) or not path_text:
            continue
        resolved = (source / path_text).resolve()
        if any(resolved == root or root in resolved.parents for root in update_roots):
            raise MigrationError("configured_data_inside_excluded_updates")
    # Preserve unknown fields, endpoints, model names, schedules and credentials.
    # Only known local paths and first-start automatic update state are changed.
    for category in CATEGORIES:
        base = source / category
        if not base.exists():
            continue
        reject_links(base)
        if not base.is_dir():
            raise MigrationError("data_category_is_not_directory")
        for current, dirs, names in os.walk(base, followlinks=False):
            current_path = Path(current)
            for name in list(dirs):
                entry = current_path / name
                reject_links(entry)
                if entry in update_roots:
                    dirs.remove(name)
                    excluded["runtime_updates"] += 1
            for name in names:
                entry = current_path / name
                if entry == endpoint and runtime_socket(entry):
                    # Only the configured/default local management endpoint is
                    # transient. Apply still requires --offline-confirmed.
                    excluded["runtime_sockets"] += 1
                    continue
                reject_links(entry)
                mode = entry.lstat().st_mode
                if stat.S_ISSOCK(mode) or name.endswith(".sock"):
                    raise MigrationError("socket_present_stop_and_export_first")
                if not stat.S_ISREG(mode):
                    raise MigrationError("non_regular_data_rejected")
                if name.endswith(".lock") or name.endswith(".pid"):
                    excluded["transient_files"] += 1
                    continue
                relative = entry.relative_to(source)
                files.append((entry, relative, digest(entry)))
                counts[category]["files"] += 1
                counts[category]["bytes"] += entry.stat().st_size
    if config_path.parent == source:
        files.append((config_path, Path("config/config.json"), digest(config_path)))
        counts["config"]["files"] += 1
        counts["config"]["bytes"] += config_path.stat().st_size
    key_path = config.get("api_keys_file") or "./data/api_keys.json"
    registry = source / key_path if not Path(key_path).is_absolute() else Path(key_path)
    encrypted_count = 0
    if registry.exists():
        doc = object_file(registry)
        records = validate_registry(doc)
        encrypted_count = sum(1 for row in records if isinstance(row, dict) and row.get("encrypted_key"))
        master = Path(str(registry) + ".enc-key")
        reject_links(master)
        if encrypted_count and (not master.is_file() or master.stat().st_size != 32):
            raise MigrationError("encrypted_registry_requires_original_master_key")
        if encrypted_count and not any(p == master for p, _, _ in files):
            raise MigrationError("master_key_not_in_copy_set")
    inventory = hashlib.sha256()
    for _, relative, checksum in sorted(files, key=lambda item: item[1].as_posix()):
        inventory.update(relative.as_posix().encode("utf-8") + b"\0" + checksum.encode("ascii") + b"\n")
    try:
        legacy_plan = legacy_schedule.legacy_schedule_document(config)
    except legacy_schedule.LegacyScheduleError as error:
        raise MigrationError(str(error)) from None
    report = {"format": FORMAT, "mode": "dry-run", "source_unchanged": True,
              "categories": counts, "excluded": excluded, "mapped_fields": sorted(remapped),
              "automatic_update_disabled": True, "encrypted_key_records": encrypted_count,
              "inventory_sha256": inventory.hexdigest(), "file_count": len(files),
              "source_config_sha256": digest(config_path),
              "legacy_plan": {
                  "schedules": len(legacy_plan["schedules"]),
                  "enabled": sum(1 for row in legacy_plan["schedules"].values() if row["enabled"]),
                  "disabled_manual_placeholders": [row["id"] for row in legacy_plan["import"]["skipped"]],
                  "dispatch_disabled": True,
                  "timezone": legacy_plan["import"]["timezone"],
              },
              "target_task_dispatch": "disabled_until_explicit_opt_in",
              "runtime_validation": "NOT RUN", "secret_decryption_validation": "NOT RUN"}
    return report, files, mapped, legacy_plan


def migrate(source: Path, destination: Path, *, apply: bool = False, offline: bool = False) -> dict:
    if apply and not offline:
        raise MigrationError("apply_requires_offline_confirmed")
    source, destination = guarded_root(source, True), guarded_root(destination, False)
    report, files, config, legacy_plan = inspect(source, destination)
    if not apply:
        return report
    stage = destination.with_name(destination.name + ".migration-staging")
    if stage.exists() or stage.is_symlink():
        raise MigrationError("staging_slot_already_exists")
    private_directory(stage)
    write_marker(stage, report, destination, "staging")
    try:
        for category in CATEGORIES:
            (stage / category).mkdir(mode=0o700)
        # An empty configured account directory is still meaningful to the loader.
        auth_dir = config.get("auth_dir")
        if isinstance(auth_dir, str) and auth_dir:
            (stage / auth_dir).mkdir(mode=0o700, parents=True, exist_ok=True)
        for origin, relative, checksum in files:
            target = stage / relative
            target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            shutil.copyfile(origin, target)
            target.chmod(0o600)
            if digest(target) != checksum or digest(origin) != checksum:
                raise MigrationError("source_changed_or_copy_verification_failed")
        # Verify the entire source again before committing, not only each file's copy window.
        refreshed, _, _, _ = inspect(source, destination)
        if refreshed["inventory_sha256"] != report["inventory_sha256"]:
            raise MigrationError("source_changed_during_copy")
        target_config = stage / "config/config.json"
        # Fixed single backup inside the new copy. Source is never written.
        shutil.copyfile(target_config, target_config.with_suffix(".json.pre-migration"))
        target_config.with_suffix(".json.pre-migration").chmod(0o600)
        target_config.write_text(json.dumps(config, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        target_config.chmod(0o600)
        # The imported schedules start disabled and contain no runs, completed
        # markers or uncertain operations. Automatic dispatch stays off until
        # an operator explicitly enables a plan in the new workspace.
        engine_path = stage / "data/task-engine.json"
        engine_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        engine_path.write_text(json.dumps(legacy_plan, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        engine_path.chmod(0o600)
        report["mode"] = "copied"
        write_marker(stage, report, destination, "copied")
        # Windows os.rename refuses overwriting; Unix guard plus empty target requirement.
        if destination.exists():
            raise MigrationError("destination_appeared_during_copy")
        stage.rename(destination)
    except BaseException:
        # A failed staging copy may contain credentials. Retain it privately for explicit
        # recovery; never delete a path selected by the user or print its contents.
        raise
    return report


def rollback(destination: Path, *, offline: bool = False) -> dict:
    if not offline:
        raise MigrationError("rollback_requires_offline_confirmed")
    destination = guarded_root(destination, True)
    recognized_marker(destination, destination, "copied")
    parked = destination.with_name(destination.name + ".rolled-back")
    if parked.exists() or parked.is_symlink():
        raise MigrationError("rollback_slot_already_exists")
    destination.rename(parked)
    return {"mode": "rolled-back", "source_unchanged": True,
            "new_data_preserved": True, "next_action": "restart_original_source"}


def recover(destination: Path, *, offline: bool = False) -> dict:
    """Park an interrupted copy in one fixed private slot; never discard data."""
    if not offline:
        raise MigrationError("recovery_requires_offline_confirmed")
    destination = guarded_root(destination, False)
    if destination.exists():
        raise MigrationError("destination_already_exists")
    stage = guarded_root(destination.with_name(destination.name + ".migration-staging"), True)
    stage_marker = object_file(stage / MARKER)
    mode = stage_marker.get("mode")
    if mode not in ("staging", "copied"):
        raise MigrationError("unrecognized_migration_destination")
    recognized_marker(stage, destination, mode)
    parked = guarded_root(destination.with_name(destination.name + ".migration-failed"), False)
    if parked.exists():
        raise MigrationError("recovery_slot_already_exists")
    stage.rename(parked)
    return {"mode": "recovered", "partial_data_preserved": True,
            "source_unchanged": True, "next_action": "retry_copy"}


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    copy = commands.add_parser("copy")
    copy.add_argument("--source", required=True, type=Path)
    copy.add_argument("--destination", required=True, type=Path)
    copy.add_argument("--apply", action="store_true")
    copy.add_argument("--offline-confirmed", action="store_true")
    undo = commands.add_parser("rollback")
    undo.add_argument("--destination", required=True, type=Path)
    undo.add_argument("--offline-confirmed", action="store_true")
    repair = commands.add_parser("recover")
    repair.add_argument("--destination", required=True, type=Path)
    repair.add_argument("--offline-confirmed", action="store_true")
    args = parser.parse_args(argv)
    try:
        if args.command == "copy":
            result = migrate(args.source, args.destination, apply=args.apply, offline=args.offline_confirmed)
        elif args.command == "rollback":
            result = rollback(args.destination, offline=args.offline_confirmed)
        else:
            result = recover(args.destination, offline=args.offline_confirmed)
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return 0
    except MigrationError as error:
        print(json.dumps({"error": str(error)}, ensure_ascii=False), file=sys.stderr)
        return 2
    except (OSError, ValueError, TypeError):
        # OS error paths may include phone numbers or account names.
        print('{"error":"filesystem_operation_failed"}', file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
