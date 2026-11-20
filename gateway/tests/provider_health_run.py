#!/usr/bin/env python3
"""Run the baseline/candidate study with one request per route and <=3 total.

Only safe outcome journal rows leave memory. Existing failed cases are never
silently retried; use a separate explicitly named output directory to retest
after a fix. Three consecutive authentication/quota failures pause that route
while other providers continue.
"""
import argparse
import collections
import concurrent.futures
import importlib.util
import json
import os
from pathlib import Path
import threading
import time
import signal

spec = importlib.util.spec_from_file_location("healthcheck", Path(__file__).with_name("provider_healthcheck.py"))
health = importlib.util.module_from_spec(spec)
spec.loader.exec_module(health)


def study_cases(inventory, smoke=False):
    representatives = health.selected_models(inventory["models"])
    models = sorted(inventory["models"], key=lambda row: (row["id"].split(":", 1)[0], row["id"]))
    plan = []
    for route, row in representatives.items():
        for protocol in health.PROTOCOLS:
            case = dict(model=row["id"], protocol=protocol, mode="plain")
            plan.append(("candidate", case))
            if not smoke:
                plan.append(("baseline", case))
    if not smoke:
        for row in models:
            for protocol in health.PROTOCOLS:
                case = dict(model=row["id"], protocol=protocol, mode="plain")
                if protocol == "chat":
                    plan.append(("baseline", case))
                plan.append(("candidate", case))
        for route, row in representatives.items():
            for protocol in health.PROTOCOLS:
                plan.append(("candidate", dict(model=row["id"], protocol=protocol, mode="stream")))
                if row.get("supports_tool_call") is True or route == "autoclaw":
                    plan.append(("candidate", dict(model=row["id"], protocol=protocol, mode="tools")))
                if row.get("supports_multi_turn") is not False:
                    plan.append(("candidate", dict(model=row["id"], protocol=protocol, mode="multiturn")))
                if row.get("supports_images") is True:
                    for color in ("red", "blue"):
                        plan.append(("candidate", dict(model=row["id"], protocol=protocol, mode="image-"+color)))
    seen, result = set(), []
    for label, case in plan:
        identity = (label, case["model"], case["protocol"], case["mode"])
        if identity not in seen:
            seen.add(identity)
            result.append((label, case))
    return result


def blocked_error(row):
    code = (row.get("error_code") or "").lower()
    return row.get("http_status") in (401, 402) or any(token in code for token in ("quota", "credential", "unauthoriz", "session_invalid", "access_token_expired"))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--baseline-port", type=int, default=17873)
    parser.add_argument("--candidate-port", type=int, default=17874)
    parser.add_argument("--smoke", action="store_true")
    parser.add_argument("--run", action="store_true", help="explicitly enable inference calls; otherwise inventory and plan only")
    parser.add_argument("--inventory-only", action="store_true")
    parser.add_argument("--max-calls", type=int, default=1000)
    parser.add_argument("--exclude-provider", action="append", default=[])
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    # Hold a real process lock across inventory and calls. Two maintenance
    # shells must not dispatch the same case before either writes its result.
    lock_handle = (output / "runner.lock").open("a")
    try:
        import fcntl
        fcntl.flock(lock_handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except ImportError:
        raise ValueError("the study runner requires an isolated POSIX host")
    stop_dispatch = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop_dispatch.set())
    signal.signal(signal.SIGINT, lambda *_: stop_dispatch.set())
    clients, inventories = {}, {}
    for label, port in [("baseline", args.baseline_port), ("candidate", args.candidate_port)]:
        root = args.root / label
        client = health.Gateway("http://127.0.0.1:"+str(port), root / "config.json", root / "panel-data/credentials.json", timeout=90)
        clients[label] = client
        inventories[label] = client.inventory()
        (output / ("inventory-"+label+".json")).write_text(json.dumps(inventories[label], indent=2), encoding="utf-8")
    planned = study_cases(inventories["candidate"], args.smoke)
    planned = [(label, case) for label, case in planned if case["model"].split(":", 1)[0] not in args.exclude_provider]
    journal = output / "results.jsonl"
    previous, paused, streaks = set(), set(), collections.Counter()
    if journal.exists():
        for line in journal.read_text().splitlines():
            row = json.loads(line)
            previous.add((row["trial"], row["model"], row["protocol"], row["mode"]))
            if row.get("error_code") == "provider_paused_after_auth_or_quota_failures":
                paused.add(row["model"].split(":", 1)[0])
    # A process can disappear after the network side effect but before its
    # response is journaled. Preserve that case as unknown instead of sending
    # it a second time on resume. A fix/retest uses a separate output journal.
    attempts = output / "attempts.jsonl"
    if attempts.exists():
        for line in attempts.read_text().splitlines():
            row = json.loads(line)
            identity = (row["trial"], row["model"], row["protocol"], row["mode"])
            if identity not in previous:
                unknown = {key: row[key] for key in ("trial", "model", "protocol", "mode")}
                unknown.update(http_status=0, passed=False, error_code="interrupted_request_result_unknown")
                with journal.open("a", encoding="utf-8") as handle:
                    handle.write(json.dumps(unknown, separators=(",", ":"))+"\n")
                previous.add(identity)
    plan_summary = dict(total_calls=len(planned), by_trial=dict(collections.Counter(label for label, _ in planned)),
                        by_mode=dict(collections.Counter(case["mode"] for _, case in planned)),
                        baseline_models=inventories["baseline"]["model_count"], candidate_models=inventories["candidate"]["model_count"],
                        excluded_providers=args.exclude_provider,
                        concurrency=3, per_route_concurrency=1, max_request_seconds=90, automatic_retries=0)
    (output / "study-plan.json").write_text(json.dumps(plan_summary, indent=2), encoding="utf-8")
    print(json.dumps(plan_summary), flush=True)
    if args.inventory_only or not args.run:
        return 0
    # Group by routing prefix. A worker owns one route and alternates baseline
    # and candidate cases within it; pool concurrency is capped at three.
    routes = collections.defaultdict(list)
    budget = 0
    for label, case in planned:
        identity = (label, case["model"], case["protocol"], case["mode"])
        if identity not in previous and budget < args.max_calls:
            routes[case["model"].split(":", 1)[0]].append((label, case))
            budget += 1
    lock = threading.Lock()
    completed, failed, last_update = 0, 0, time.monotonic()
    def save(row):
        nonlocal completed, failed, last_update
        with lock:
            with journal.open("a", encoding="utf-8") as handle:
                handle.write(json.dumps(row, separators=(",", ":"))+"\n")
                handle.flush()
            completed += 1
            failed += not row["passed"]
            if time.monotonic()-last_update > 45 or not row["passed"]:
                print(json.dumps(dict(completed=completed, failed=failed, route=row["model"].split(":", 1)[0],
                                      protocol=row["protocol"], mode=row["mode"], trial=row["trial"],
                                      http_status=row.get("http_status"), error_code=row.get("error_code"),
                                      paused_routes=sorted(paused))), flush=True)
                last_update = time.monotonic()
    def run_route(route, cases):
        for label, case in cases:
            control_path = output / "control.json"
            try:
                control = json.loads(control_path.read_text()) if control_path.exists() else {}
            except ValueError:
                stop_dispatch.set()
                return
            if stop_dispatch.is_set() or control.get("stop_dispatch"):
                return
            if route in set(control.get("paused_routes") or []):
                row = dict(case, trial=label, http_status=0, passed=False, error_code="provider_paused_for_investigation")
                save(row)
                continue
            if route in paused:
                row = dict(case, trial=label, http_status=0, passed=False, error_code="provider_paused_after_auth_or_quota_failures")
            elif case["model"] not in clients[label].by_model:
                row = dict(case, trial=label, http_status=0, passed=False, error_code="model_not_authorized")
            else:
                with lock:
                    with (output / "attempts.jsonl").open("a", encoding="utf-8") as handle:
                        handle.write(json.dumps(dict(case, trial=label, event="started"), separators=(",", ":"))+"\n")
                        handle.flush()
                        os.fsync(handle.fileno())
                row = health.probe(clients[label], case)
                row["trial"] = label
                if blocked_error(row):
                    streaks[route] += 1
                elif row["passed"]:
                    streaks[route] = 0
                if streaks[route] >= 3:
                    paused.add(route)
            save(row)
            time.sleep(.2)
    with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
        futures = [pool.submit(run_route, route, cases) for route, cases in routes.items()]
        for future in futures:
            future.result()
    result = dict(completed=completed, failed=failed, passed=completed-failed, paused_routes=sorted(paused), production_changed=False)
    (output / "summary.json").write_text(json.dumps(result, indent=2), encoding="utf-8")
    print(json.dumps(result), flush=True)
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(json.dumps(dict(error_type=type(error).__name__, safe_failure=True)), flush=True)
        raise SystemExit(1)
