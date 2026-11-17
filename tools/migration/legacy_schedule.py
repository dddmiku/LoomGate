"""Legacy config schedule -> unified LoomGate schedule conversion.

This module is deliberately pure: it reads only objects already loaded by the
migration inspector and returns schedules for the *copy*. It never starts the
new runtime, never dispatches work, and never changes the source tree.

Legacy semantics recovered from the read-only v3.16.1 baseline:
- `schedule.<task>_hours` are whole hours in the process-local timezone; the
  published deployment set TZ=Asia/Shanghai.
- explicit `schedule.<task>_enabled=false` disables that task; an absent key
  defaults to true.
- an empty or null hour array falls back to the built-in default hours.
- Trae check-in is the only task whose minute is not 0 (00:05 CST).
- legacy scheduling had no per-account scope: every eligible account ran.
"""
from __future__ import annotations

import re


class LegacyScheduleError(Exception):
    """Constant, non-sensitive conversion error."""

TIMEZONE = "Asia/Shanghai"

# field -> (task key, default hours, provider, once per day)
LEGACY_TASKS = (
    ("checkin", "checkin_hours", "checkin_enabled", [9, 21], "workbuddy", False),
    ("trae_check", "trae_check_hours", "trae_check_enabled", [0], "trae", False),
    ("travel", "travel_hours", "travel_enabled", [15], "workbuddy", False),
    ("activity", "activity_hours", "activity_enabled", [10], "workbuddy", False),
    ("keepalive", "keepalive_hours", "keepalive_enabled", [22], "workbuddy", False),
    ("school", "school_hours", "school_enabled", [13], "workbuddy", False),
    ("cat", "cat_hours", "cat_enabled", [1], "workbuddy", False),
    ("growth", "growth_hours", "growth_enabled", [12], "workbuddy", False),
    ("balance", "balance_hours", "balance_enabled", [14], "workbuddy", False),
)

# Tasks whose old implementation reported fabricated usage events. They are
# preserved as disabled manual placeholders so the operator can see what was
# in the source deployment, but the new runtime must never auto-run them.
FAKE_EVENT_TASKS = {"school", "cat", "activity"}

# Trae's legacy refresh loop ran through keepalive with the Trae provider.
TRAE_KEEPALIVE = "keepalive"

ID_SAFE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")


def _hours(value, default, label):
    if value is None:
        return list(default)
    if not isinstance(value, list):
        raise LegacyScheduleError("invalid_legacy_schedule_" + label)
    if not value:
        return list(default)
    out = []
    for hour in value:
        if type(hour) is not int or not 0 <= hour <= 23:
            raise LegacyScheduleError("invalid_legacy_schedule_" + label)
        if hour in out:
            raise LegacyScheduleError("invalid_legacy_schedule_" + label)
        out.append(hour)
    return sorted(out)


def legacy_schedule_document(config: dict) -> dict:
    """Return the unified task-engine schedule document for a copy.

    The result is a complete `task-engine.json` v4 document with schedules but
    no runs, no completions and no uncertain operations. New copies start with
    dispatch disabled; callers must not auto-run the imported schedules.
    """
    if not isinstance(config, dict):
        raise LegacyScheduleError("invalid_configuration_object")
    legacy = config.get("schedule")
    if legacy is None:
        legacy = {}
    if not isinstance(legacy, dict):
        raise LegacyScheduleError("invalid_legacy_schedule_section")

    schedules = {}
    skipped = []
    for key, hours_field, enabled_field, default_hours, provider, once in LEGACY_TASKS:
        enabled = legacy.get(enabled_field, True)
        if type(enabled) is not bool:
            raise LegacyScheduleError("invalid_legacy_schedule_" + key)
        hours = _hours(legacy.get(hours_field), default_hours, key)
        schedule_id = "legacy-" + key.replace("_", "-")
        if not ID_SAFE.fullmatch(schedule_id):
            raise LegacyScheduleError("invalid_legacy_schedule_id")
        manual_only = key in FAKE_EVENT_TASKS
        # A copied workspace must never dispatch the downloaded schedules on
        # first launch. Eligibility is recorded, not enabled; the operator
        # turns a plan on explicitly after reviewing the migration report.
        schedules[schedule_id] = {
            "id": schedule_id,
            "task_key": key,
            "provider_id": provider,
            "enabled": False,
            "hours": hours,
            "minute": 5 if key == "trae_check" else 0,
            "timezone": TIMEZONE,
            "account_ids": [],
            "once_per_day": once,
        }
        if manual_only or not enabled:
            skipped.append({"id": schedule_id, "reason": "manual_only_legacy_event_report" if manual_only else "disabled_in_source"})
    originals = {}
    for key, _, enabled_field, _, _, _ in LEGACY_TASKS:
        originals["legacy-" + key.replace("_", "-")] = bool(legacy.get(enabled_field, True))
    return {
        "format": 4,
        "version": 4,
        "schedules": schedules,
        "runs": [],
        "completed": {},
        "uncertain": {},
        "pending_tasks": {},
        "receipts": {},
        "attempts": {},
        "work": {},
        "import": {
            "source": "legacy-config-schedule",
            "timezone": TIMEZONE,
            "dispatch_disabled": True,
            "originally_enabled": originals,
            "skipped": skipped,
        },
    }


def legacy_account_filter(config: dict, accounts: list[dict]) -> list[str]:
    """Accounts eligible for CN task dispatch in the legacy deployment.

    Legacy scheduling had no per-task account scope; every non-disabled
    WorkBuddy account was visited, with global/Trae skipped inside each task.
    The new copy is still created with empty account_ids (all enabled), but the
    importer records the identities it observed for a later explicit opt-in.
    """
    eligible = []
    for row in accounts:
        if not isinstance(row, dict):
            continue
        uid = row.get("uid")
        realm = row.get("realm") or ""
        if isinstance(uid, str) and uid and realm == "cn":
            eligible.append(uid)
    return eligible
