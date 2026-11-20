#!/usr/bin/env python3
"""Bounded, credential-safe model/protocol health checks against a running gateway.

Run inside the deployment container or against an isolated copy. Existing
calling keys are copied through the authenticated management API into process
memory only. Reports contain capability metadata, status, timing and numeric
usage; they never contain credentials, prompts, replies, tool arguments or raw
upstream errors. The default is inventory only; --run explicitly enables tiny
inference calls. No login, SMS, reward or task endpoint is called.
"""
import argparse
import base64
import collections
import concurrent.futures
import hashlib
import hmac
import http.client
import json
from pathlib import Path
import re
import secrets
import struct
import threading
import time
import zlib
from urllib.parse import quote, urlsplit

MODEL = re.compile(r"[A-Za-z0-9_:./-]{1,180}\Z")
CODE = re.compile(r"[A-Za-z0-9_.:-]{1,100}\Z")
PROTOCOLS = ("chat", "responses", "messages", "gemini")
MODEL_FIELDS = ("id", "owned_by", "supports_tool_call", "supports_images",
                "supports_reasoning", "streaming_mode", "supports_multi_turn",
                "context_length", "max_output_tokens", "reasoning_supported_efforts",
                "reasoning_default_effort", "credits")


def admin_cookie(path):
    record = json.loads(Path(path).read_text())
    enc = lambda value: base64.urlsafe_b64encode(value).rstrip(b"=")
    body = enc(json.dumps(dict(u=record["username"], e=int(time.time())+7200,
                               n=secrets.token_hex(12)), separators=(",", ":")).encode())
    signature = enc(hmac.new(bytes.fromhex(record["sessionKey"]), body, hashlib.sha256).digest())
    return "wb2a_admin="+(body+b"."+signature).decode()


class Gateway:
    def __init__(self, base, config, credentials, timeout=75):
        self.address = urlsplit(base)
        if self.address.hostname not in ("127.0.0.1", "localhost", "::1"):
            raise ValueError("health probes require a loopback gateway")
        self.cookie = admin_cookie(credentials) if Path(credentials).is_file() else None
        self.config = json.loads(Path(config).read_text())
        self.timeout = timeout
        self.keys = []
        self.by_model = {}

    def connection(self):
        constructor = http.client.HTTPSConnection if self.address.scheme == "https" else http.client.HTTPConnection
        return constructor(self.address.hostname, self.address.port, timeout=self.timeout)

    def json_request(self, path, body=None, key=None, admin=False):
        headers = {"Content-Type": "application/json"}
        if key:
            headers["Authorization"] = "Bearer "+key
        if admin and self.cookie:
            headers.update(Cookie=self.cookie, **{"X-Admin-Request": "1"})
        connection = self.connection()
        try:
            connection.request("GET" if body is None else "POST", path,
                               None if body is None else json.dumps(body), headers)
            response = connection.getresponse()
            raw = response.read(16 << 20)
            try:
                data = json.loads(raw)
            except ValueError:
                data = {}
            return response.status, data
        finally:
            connection.close()

    def inventory(self):
        status, providers = self.json_request("/admin/api/workbench/providers", admin=True)
        provider_rows = [{field: row.get(field) for field in ("id", "inference_available", "accounts")}
                         for row in providers.get("providers", [])]
        status, records = self.json_request("/admin/api/keys", admin=True)
        key_records = records.get("keys") or [] if status == 200 else []
        configured = self.config.get("api_key")
        candidates = [configured] if isinstance(configured, str) and configured else []
        for record in key_records:
            if not record.get("enabled") or not record.get("copy_available"):
                continue
            expiry = record.get("expires_at")
            if expiry:
                # The live gateway's discovery auth makes the final decision.
                pass
            status, result = self.json_request("/admin/api/keys/copy", {"id": record["id"]}, admin=True)
            key = result.get("key")
            if status == 200 and isinstance(key, str) and key:
                candidates.append(key)
        models = {}
        permissions = []
        seen = set()
        for key in candidates:
            fingerprint = hashlib.sha256(key.encode()).hexdigest()[:12]
            if fingerprint in seen:
                continue
            seen.add(fingerprint)
            status, result = self.json_request("/v1/models", key=key)
            allowed = []
            for row in result.get("data") or []:
                identifier = row.get("id", "")
                if not MODEL.fullmatch(identifier):
                    continue
                allowed.append(identifier)
                self.by_model.setdefault(identifier, key)
                models[identifier] = {field: row[field] for field in MODEL_FIELDS if field in row}
            permissions.append(dict(key_sha256=fingerprint, http_status=status, visible_models=len(allowed)))
            self.keys.append(key)
        return dict(providers=provider_rows, models=sorted(models.values(), key=lambda row: row["id"]),
                    key_permissions=permissions, protocols=list(PROTOCOLS),
                    model_count=len(models), created_keys=0, persisted_credentials=False)


def selected_models(models):
    groups = collections.defaultdict(list)
    for row in models:
        groups[row["id"].split(":", 1)[0]].append(row)
    preferred = dict(cn="cn:deepseek-v4.1-flash", global_="global:deepseek-v4.1-flash",
                     trae="trae:deepseek-v4.1-flash", autoclaw="autoclaw:zai_glm-5.3-flash",
                     doubao="doubao:deepseek-v4.1-flash")
    preferred["global"] = preferred.pop("global_")
    return {provider: next((row for row in rows if row["id"] == preferred.get(provider)), rows[0])
            for provider, rows in groups.items()}


def plan(inventory):
    representatives = selected_models(inventory["models"])
    cases = []
    for model in inventory["models"]:
        for protocol in PROTOCOLS:
            cases.append(dict(model=model["id"], protocol=protocol, mode="plain"))
    for provider, row in representatives.items():
        for protocol in PROTOCOLS:
            cases.append(dict(model=row["id"], protocol=protocol, mode="stream"))
            if row.get("supports_multi_turn") is not False:
                cases.append(dict(model=row["id"], protocol=protocol, mode="multiturn"))
            if row.get("supports_tool_call") is True or provider == "autoclaw":
                # AutoClaw currently implements ToolProvider but advertises false:
                # exercise the implementation claim and record the discrepancy.
                cases.append(dict(model=row["id"], protocol=protocol, mode="tools"))
                cases.append(dict(model=row["id"], protocol=protocol, mode="tool_loop"))
    return dict(model_count=inventory["model_count"], route_count=len(representatives),
                protocol_count=len(PROTOCOLS), request_count=len(cases),
                representatives={key: value["id"] for key, value in representatives.items()}, cases=cases)


def payload(case):
    protocol, mode, model = case["protocol"], case["mode"], case["model"]
    stream, tools = mode == "stream", mode in ("tools", "tool_loop")
    marker = case.get("_expected", "HC_fixture_12").rsplit("_", 1)[0]
    remembered = case.get("_expected", "MEM_fixture")
    remember_instruction = "Remember the word "+remembered+". Reply briefly."
    prompt = "Call health_echo with value OK. Do not answer in prose." if tools else "Compute 7 + 5. Reply exactly as "+marker+"_<answer>, replacing <answer> with the numeric sum. No explanation or punctuation."
    schema = dict(type="object", properties=dict(value=dict(type="string")), required=["value"], additionalProperties=False)
    function = dict(name="health_echo", description="A synthetic local health test.", parameters=schema)
    if protocol == "chat":
        messages = [dict(role="user", content=prompt)]
        if mode == "multiturn":
            messages = [dict(role="user", content=remember_instruction),
                        dict(role="assistant", content="OK"),
                        dict(role="user", content="What word did I ask you to remember? Reply with just that word.")]
        body = dict(model=model, messages=messages, stream=stream, max_tokens=4096)
        if tools:
            body.update(tools=[dict(type="function", function=function)],
                        tool_choice=dict(type="function", function=dict(name="health_echo")))
        return "/v1/chat/completions", image_payload(case, body, prompt)
    if protocol == "responses":
        body = dict(model=model, input=prompt, stream=stream, max_output_tokens=4096, store=False)
        if mode == "multiturn":
            body["input"] = [dict(role="user", content=remember_instruction),
                             dict(role="assistant", content="OK"),
                             dict(role="user", content="What word did I ask you to remember? Reply with just that word.")]
        if tools:
            body.update(tools=[dict(type="function", **function)], tool_choice=dict(type="function", name="health_echo"))
        return "/v1/responses", image_payload(case, body, prompt)
    if protocol == "messages":
        messages = [dict(role="user", content=prompt)]
        if mode == "multiturn":
            messages = [dict(role="user", content=remember_instruction),
                        dict(role="assistant", content="OK"),
                        dict(role="user", content="What word did I ask you to remember? Reply with just that word.")]
        body = dict(model=model, messages=messages, stream=stream, max_tokens=4096)
        if tools:
            body.update(tools=[dict(name="health_echo", description=function["description"], input_schema=schema)],
                        tool_choice=dict(type="tool", name="health_echo"))
        return "/v1/messages", image_payload(case, body, prompt)
    body = dict(contents=[dict(role="user", parts=[dict(text=prompt)])], generationConfig=dict(maxOutputTokens=4096))
    if mode == "multiturn":
        body["contents"] = [dict(role="user", parts=[dict(text=remember_instruction)]),
                            dict(role="model", parts=[dict(text="OK")]),
                            dict(role="user", parts=[dict(text="What word did I ask you to remember? Reply with just that word.")])]
    if tools:
        body.update(tools=[dict(functionDeclarations=[function])],
                    toolConfig=dict(functionCallingConfig=dict(mode="ANY", allowedFunctionNames=["health_echo"])))
    action = "streamGenerateContent?alt=sse" if stream else "generateContent"
    return "/v1beta/models/"+quote(model, safe=":")+":"+action, image_payload(case, body, prompt)


def image_payload(case, body, prompt):
    if not case["mode"].startswith("image"):
        return body
    # A plain red pixel block contains no word or textual hint about its color.
    # The question asks for visual color identification instead of embedding
    # the answer in accompanying text.
    def chunk(kind, raw):
        return struct.pack("!I", len(raw))+kind+raw+struct.pack("!I", zlib.crc32(kind+raw)&0xffffffff)
    png = b"\x89PNG\r\n\x1a\n"+chunk(b"IHDR", struct.pack("!IIBBBBB", 16, 16, 8, 2, 0, 0, 0))
    color = b"\x00\x00\xff" if case["mode"] == "image-blue" else b"\xff\x00\x00"
    png += chunk(b"IDAT", zlib.compress((b"\x00"+color*16)*16))+chunk(b"IEND", b"")
    encoded = base64.b64encode(png).decode()
    question = "Identify the solid color in the image. Reply with exactly one English color word, no explanation."
    protocol = case["protocol"]
    if protocol == "chat":
        body["messages"] = [dict(role="user", content=[dict(type="text", text=question),
                            dict(type="image_url", image_url=dict(url="data:image/png;base64,"+encoded))])]
    elif protocol == "responses":
        body["input"] = [dict(role="user", content=[dict(type="input_text", text=question),
                         dict(type="input_image", image_url="data:image/png;base64,"+encoded)])]
    elif protocol == "messages":
        body["messages"] = [dict(role="user", content=[dict(type="image", source=dict(type="base64", media_type="image/png", data=encoded)),
                            dict(type="text", text=question)])]
    else:
        body["contents"] = [dict(role="user", parts=[dict(inlineData=dict(mimeType="image/png", data=encoded)), dict(text=question)])]
    return body


def valid_tool_arguments(value):
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except ValueError:
            return False
    return isinstance(value, dict) and set(value) == {"value"} and value["value"] == "OK"


def safe_error(data):
    error = data.get("error") or {}
    value = next((error.get(field) for field in ("code", "type", "status") if isinstance(error.get(field), str)), None) if isinstance(error, dict) else error
    return value if isinstance(value, str) and CODE.fullmatch(value) else ("upstream_error" if error else None)


def inspect_event(protocol, event, totals):
    if not isinstance(event, dict):
        return
    def text(value):
        if not isinstance(value, str):
            return
        totals["text_chars"] += len(value)
        totals.setdefault("_answer", []).append(value)
        totals["expected_word_seen"] = totals.get("expected_word_seen", False) or "ORCHID" in value.upper()
    if error := safe_error(event):
        totals["error_code"] = error
    usage = event.get("usage") or event.get("usageMetadata") or (event.get("response") or {}).get("usage")
    if isinstance(usage, dict):
        for field in ("input_tokens", "output_tokens", "prompt_tokens", "completion_tokens",
                      "total_tokens", "promptTokenCount", "candidatesTokenCount", "totalTokenCount"):
            if isinstance(usage.get(field), (int, float)):
                totals.setdefault("usage", {})[field] = usage[field]
    if protocol == "chat":
        for choice in event.get("choices") or []:
            message = choice.get("delta") or choice.get("message") or {}
            if isinstance(message.get("content"), str) and message["content"]:
                text(message["content"])
            for tool in message.get("tool_calls") or []:
                fn = tool.get("function") or {}
                if fn.get("name") == "health_echo":
                    totals["tool_seen"] = True
                    totals["tool_arguments_valid"] = valid_tool_arguments(fn.get("arguments"))
                    totals["_tool_record"] = dict(tool)
                    totals["_assistant_message"] = dict(message)
            if choice.get("finish_reason"):
                totals["finished"] = True
                totals["finish_reason"] = choice["finish_reason"] if CODE.fullmatch(str(choice["finish_reason"])) else "other"
                if choice["finish_reason"] == "length":
                    totals["error_code"] = "output_truncated"
                elif choice["finish_reason"] not in ("stop", "tool_calls"):
                    totals["error_code"] = "abnormal_finish_reason"
    elif protocol == "responses":
        kind = event.get("type")
        if kind == "response.output_text.delta":
            text(event.get("delta") or "")
        response = event.get("response") or event
        if response.get("status") in ("failed", "incomplete"):
            totals["error_code"] = safe_error(response) or "response_incomplete"
        for item in response.get("output") or []:
            if item.get("type") == "function_call" and item.get("name") == "health_echo":
                totals["tool_seen"] = True
                totals["tool_arguments_valid"] = valid_tool_arguments(item.get("arguments"))
                totals["_tool_record"] = dict(item)
                totals["_assistant_output"] = list(response.get("output") or [])
            for content in item.get("content") or []:
                if content.get("type") == "output_text":
                    if totals.get("_answer") is None:
                        text(content.get("text") or "")
        if kind == "response.completed" or response.get("status") == "completed":
            totals["finished"] = True
    elif protocol == "messages":
        if (event.get("delta") or {}).get("type") == "text_delta":
            text(event["delta"].get("text") or "")
        block = event.get("content_block") or {}
        blocks = event.get("content") or [block]
        for item in blocks:
            if item.get("type") == "tool_use" and item.get("name") == "health_echo":
                totals["tool_seen"] = True
                totals["tool_arguments_valid"] = valid_tool_arguments(item.get("input"))
                totals["_tool_record"] = dict(item)
                totals["_assistant_content"] = list(event.get("content") or [item])
            if item.get("type") == "text":
                text(item.get("text") or "")
        if event.get("type") == "message_stop" or event.get("stop_reason"):
            totals["finished"] = True
        if event.get("stop_reason") == "max_tokens" or (event.get("delta") or {}).get("stop_reason") == "max_tokens":
            totals["error_code"] = "output_truncated"
        stop = event.get("stop_reason") or (event.get("delta") or {}).get("stop_reason")
        if stop and stop not in ("end_turn", "tool_use", "max_tokens"):
            totals["error_code"] = "abnormal_finish_reason"
    else:
        for candidate in event.get("candidates") or []:
            for part in (candidate.get("content") or {}).get("parts") or []:
                if isinstance(part.get("text"), str) and not part.get("thought"):
                    text(part["text"])
                if (part.get("functionCall") or {}).get("name") == "health_echo":
                    totals["tool_seen"] = True
                    totals["tool_arguments_valid"] = valid_tool_arguments(part["functionCall"].get("args"))
                    totals["_tool_record"] = dict(part["functionCall"])
                    totals["_candidate_content"] = dict(candidate.get("content") or {})
            if candidate.get("finishReason"):
                totals["finished"] = True
                if candidate["finishReason"] == "MAX_TOKENS":
                    totals["error_code"] = "output_truncated"
                elif candidate["finishReason"] != "STOP":
                    totals["error_code"] = "abnormal_finish_reason"


def continuation_payload(case, first, marker):
    path, body = payload(dict(case, mode="tools"))
    record = first.get("_tool_record") or {}
    content = json.dumps(dict(result=marker), separators=(",", ":"))
    instruction = "Reply with exactly the result field from the tool response. No explanation or punctuation."
    protocol = case["protocol"]
    if protocol == "chat":
        assistant = first.get("_assistant_message") or {}
        body["messages"] += [dict(assistant, role="assistant"),
                             dict(role="tool", tool_call_id=record.get("id", ""), content=content),
                             dict(role="user", content=instruction)]
        body["tool_choice"] = "auto"
    elif protocol == "responses":
        body["input"] = [dict(role="user", content=body["input"]),
                         *first.get("_assistant_output", []),
                         dict(type="function_call_output", call_id=record.get("call_id") or record.get("id", ""), output=content),
                         dict(role="user", content=instruction)]
        body["tool_choice"] = "auto"
    elif protocol == "messages":
        body["messages"] += [dict(role="assistant", content=first.get("_assistant_content", [])),
                             dict(role="user", content=[dict(type="tool_result", tool_use_id=record.get("id", ""), content=content),
                                                         dict(type="text", text=instruction)])]
        body["tool_choice"] = dict(type="auto")
    else:
        body["contents"] += [dict(first.get("_candidate_content") or {}, role="model"),
                             dict(role="user", parts=[dict(functionResponse=dict(name="health_echo", response=dict(result=marker))),
                                                        dict(text=instruction)])]
        body["toolConfig"] = dict(functionCallingConfig=dict(mode="AUTO"))
    return path, body


def probe(gateway, case, override_payload=None):
    marker = "HC_"+hashlib.sha256((case["model"]+"/"+case["protocol"]).encode()).hexdigest()[:8]+"_12"
    path, body = override_payload or payload(dict(case, _expected=marker))
    result = dict(case, http_status=0, ttfb_ms=None, first_event_ms=None, total_ms=None,
                  first_text_ms=None, first_tool_ms=None, events=0, text_chars=0, tool_seen=False, finished=False, passed=False)
    connection = gateway.connection()
    start = time.monotonic()
    try:
        headers = {"Authorization": "Bearer "+gateway.by_model[case["model"]],
                   "Content-Type": "application/json", "Anthropic-Version": "2023-06-01"}
        connection.request("POST", path, json.dumps(body), headers)
        response = connection.getresponse()
        result.update(http_status=response.status, ttfb_ms=round((time.monotonic()-start)*1000, 2))
        if response.status == 200 and case["mode"] == "stream":
            consumed = 0
            while True:
                line = response.readline(256 << 10)
                if not line:
                    break
                consumed += len(line)
                if consumed > 2 << 20 or time.monotonic()-start > gateway.timeout:
                    result["error_code"] = "probe_budget_exceeded"
                    break
                if line.startswith(b"data: "):
                    event_data = line[6:].strip()
                    if event_data == b"[DONE]":
                        continue
                    try:
                        event = json.loads(event_data)
                    except ValueError:
                        result["error_code"] = "invalid_sse_json"
                        continue
                    result["events"] += 1
                    if result["first_event_ms"] is None:
                        result["first_event_ms"] = round((time.monotonic()-start)*1000, 2)
                    inspect_event(case["protocol"], event, result)
                    if result["text_chars"] and result["first_text_ms"] is None:
                        result["first_text_ms"] = round((time.monotonic()-start)*1000, 2)
                    if result["tool_seen"] and result["first_tool_ms"] is None:
                        result["first_tool_ms"] = round((time.monotonic()-start)*1000, 2)
        else:
            raw = response.read(2 << 20)
            try:
                inspect_event(case["protocol"], json.loads(raw), result)
            except ValueError:
                result["error_code"] = "invalid_json_response"
        result["passed"] = (response.status == 200 and not result.get("error_code") and result["finished"]
                              and (result["tool_seen"] and result.get("tool_arguments_valid", False) if case["mode"] in ("tools", "tool_loop") else result["text_chars"] > 0))
        if response.status == 200 and case["mode"] not in ("tools", "tool_loop") and result["text_chars"] == 0 and not result.get("error_code"):
            result["error_code"] = "empty_visible_answer"
        if case["mode"] == "multiturn":
            result["memory_exact"] = "".join(result.get("_answer", [])).strip() == marker
            result["passed"] = result["passed"] and result["memory_exact"]
            if not result["memory_exact"] and not result.get("error_code"):
                result["error_code"] = "multiturn_memory_mismatch"
        elif case["mode"].startswith("image"):
            expected_color = "blue" if case["mode"] == "image-blue" else "red"
            result["color_correct"] = "".join(result.get("_answer", [])).strip().lower() == expected_color
            result["passed"] = result["passed"] and result["color_correct"]
            if not result["color_correct"] and not result.get("error_code"):
                result["error_code"] = "image_color_incorrect"
        elif case["mode"] not in ("tools", "tool_loop"):
            result["output_matches_expected"] = "".join(result.get("_answer", [])).strip() == marker
            result["passed"] = result["passed"] and result["output_matches_expected"]
            if result["text_chars"] > 0 and not result["output_matches_expected"] and not result.get("error_code"):
                result["error_code"] = "unexpected_canary_or_arithmetic_answer"
        if case["mode"] == "tool_loop" and result["passed"]:
            followup = continuation_payload(case, result, marker)
            # The initial response is consumed; close its connection before
            # beginning the caller-side tool-result turn. No hidden retry.
            connection.close()
            continued = probe(gateway, dict(case, mode="tool_result"), override_payload=followup)
            result.update(tool_first_call_passed=True, tool_result_continuation_passed=continued["passed"],
                          continuation_http_status=continued["http_status"],
                          continuation_total_ms=continued["total_ms"], passed=continued["passed"],
                          continuation_usage=continued.get("usage"),
                          generation_calls=2)
            if continued.get("error_code"):
                result["error_code"] = continued["error_code"]
    except Exception as error:
        result["error_code"] = type(error).__name__
    finally:
        result["total_ms"] = round((time.monotonic()-start)*1000, 2)
        for key in list(result):
            if key.startswith("_"):
                result.pop(key, None)
        connection.close()
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default="http://127.0.0.1:7863")
    parser.add_argument("--config", default="/app/config/config.json")
    parser.add_argument("--credentials", default="/app/panel-data/credentials.json")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--timeout", type=int, default=75)
    parser.add_argument("--label", default="baseline")
    parser.add_argument("--base-secondary", default="")
    parser.add_argument("--config-secondary", default="")
    parser.add_argument("--credentials-secondary", default="")
    parser.add_argument("--label-secondary", default="candidate")
    parser.add_argument("--run", action="store_true")
    parser.add_argument("--stage", choices=["all-models", "protocols", "all"], default="all")
    parser.add_argument("--max-calls", type=int, default=12)
    parser.add_argument("--provider", default="")
    parser.add_argument("--exclude-provider", action="append", default=[])
    parser.add_argument("--mode", choices=["plain", "stream", "tools", "tool_loop", "multiturn", "image-red", "image-blue"])
    args = parser.parse_args()
    gateway = Gateway(args.base, args.config, args.credentials, args.timeout)
    inventory = gateway.inventory()
    secondary = None
    if args.base_secondary:
        if not args.config_secondary:
            parser.error("secondary config path is required; do not transmit a raw key on the command line")
        secondary = Gateway(args.base_secondary, args.config_secondary,
                            args.credentials_secondary or args.credentials, args.timeout)
        secondary_inventory = secondary.inventory()
    matrix = plan(inventory)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    (output / "inventory.json").write_text(json.dumps(inventory, indent=2), encoding="utf-8")
    (output / "plan.json").write_text(json.dumps(matrix, indent=2), encoding="utf-8")
    if secondary:
        (output / "inventory-secondary.json").write_text(json.dumps(secondary_inventory, indent=2), encoding="utf-8")
    if not args.run:
        print(json.dumps({key: value for key, value in matrix.items() if key != "cases"}))
        return 0
    cases = matrix["cases"]
    if args.stage == "all-models":
        cases = [case for case in cases if case["mode"] == "plain"]
    elif args.stage == "protocols":
        representatives = set(matrix["representatives"].values())
        cases = [case for case in cases if case["model"] in representatives]
    if args.provider:
        cases = [case for case in cases if case["model"].split(":", 1)[0] == args.provider]
    if args.exclude_provider:
        cases = [case for case in cases if case["model"].split(":", 1)[0] not in args.exclude_provider]
    if args.mode:
        cases = [case for case in cases if case["mode"] == args.mode]
    report = output / "results.jsonl"
    previous = set()
    if report.exists():
        for line in report.read_text().splitlines():
            row = json.loads(line)
            previous.add((row.get("trial", args.label), row["model"], row["protocol"], row["mode"]))
    trials = [(args.label, gateway)]
    if secondary:
        trials.append((args.label_secondary, secondary))
    remaining = [(label, client, case) for case in cases for label, client in trials
                 if (label, case["model"], case["protocol"], case["mode"]) not in previous]
    results = []
    for label, client, case in remaining[:max(0, args.max_calls)]:
        if case["model"] not in client.by_model:
            result = dict(case, http_status=0, error_code="model_not_authorized", passed=False, trial=label)
        else:
            result = probe(client, case)
            result["trial"] = label
        results.append(result)
        with report.open("a", encoding="utf-8") as handle:
            handle.write(json.dumps(result, separators=(",", ":"))+"\n")
        print(json.dumps(result), flush=True)
        time.sleep(.15)
    return 0 if all(row["passed"] for row in results) else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(json.dumps(dict(error_type=type(error).__name__, safe_failure=True)))
        raise SystemExit(1)
