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

# field -> (legacy task name, hours field, enabled field, default hours, provider, once per day)
#
# `task_key` is intentionally not the legacy module name. The new engine filters
# discovered tasks with TaskKeyMatches(pattern, key): a literal key must equal a
# real discovered task key, a pattern ending in "/" covers that module prefix.
# Legacy config only had module-level schedule fields, so:
#   - trae_check ran Trae's check-in: adapter "trae", real task key "checkin";
#   - travel_* ran the Buddy travel pipeline: discovered keys travel/<action>;
#   - growth_* ran the whole growth-center pipeline: discovered keys growth/<...>.
# Writing the legacy names verbatim produced plans that fired on time, discovered
# zero tasks and finished as "skipped" — visible in the console, doing nothing.
LEGACY_TASKS = (
    ("checkin", "checkin_hours", "checkin_enabled", [9, 21], "workbuddy", "checkin", False),
    ("trae_check", "trae_check_hours", "trae_check_enabled", [0], "trae", "checkin", False),
    ("travel", "travel_hours", "travel_enabled", [15], "workbuddy", "travel/", False),
    ("activity", "activity_hours", "activity_enabled", [10], "workbuddy", "activity", False),
    ("keepalive", "keepalive_hours", "keepalive_enabled", [22], "workbuddy", "keepalive", False),
    ("school", "school_hours", "school_enabled", [13], "workbuddy", "school", False),
    ("cat", "cat_hours", "cat_enabled", [1], "workbuddy", "cat", False),
    ("growth", "growth_hours", "growth_enabled", [12], "workbuddy", "growth/", False),
    ("balance", "balance_hours", "balance_enabled", [14], "workbuddy", "balance", False),
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
    for key, hours_field, enabled_field, default_hours, provider, task_key, once in LEGACY_TASKS:
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
            "task_key": task_key,
            "provider_id": provider,
            "enabled": False,
            "hours": hours,
            "minute": 5 if key == "trae_check" else 0,
            "timezone": TIMEZONE,
            "account_ids": [],
            "once_per_day": once,
            # 审计事实：旧部署里这条排程的开关原本是什么。计划本身一律停用
            # （升级不能自动恢复写操作），但控制台要靠这个字段显示「旧版曾启用」
            # 或「旧版未启用」——否则运维无法判断一条停用的计划是迁移有意停的，
            # 还是旧配置本来就没开。应用内启动导入（cmd/server/workbench.go）写
            # 的是同一个字段名，两个入口必须一致。
            "legacy_enabled": enabled,
        }
        if manual_only or not enabled:
            skipped.append({"id": schedule_id, "reason": "manual_only_legacy_event_report" if manual_only else "disabled_in_source"})
    originals = {}
    for key, _, enabled_field, _, _, _, _ in LEGACY_TASKS:
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
        # 离线迁移本身就是「旧计划导入」这一次动作。不写这个标记的话，网关
        # 首次启动会再跑一遍应用内导入：发现计划已存在 → 判定为用户数据 →
        # 报 ErrLegacyScheduleImportBlocked 并保持标记未完成，每次启动都重试；
        # 更严重的是用户删掉迁移来的计划后，下次启动会因为标记仍为 false 而把
        # 它们重新写回（应用内路径已有「删除不得复活」的回归测试）。
        "legacy_schedule_import_done": True,
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
