"""Prevent false-positive live probe verdicts without any external service."""
import importlib.util
import json
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("healthcheck", Path(__file__).with_name("provider_healthcheck.py"))
health = importlib.util.module_from_spec(spec)
spec.loader.exec_module(health)


class Response:
    status = 200

    def __init__(self, events=None, data=None):
        self.lines = iter([b"data: "+json.dumps(event).encode()+b"\n" for event in events or []])
        self.data = json.dumps(data or {}).encode()

    def read(self, _):
        return self.data

    def readline(self, _):
        return next(self.lines, b"")


class Connection:
    def __init__(self, response):
        self.response = response

    def request(self, *args):
        pass

    def getresponse(self):
        return self.response

    def close(self):
        pass


def execute(protocol, mode, events=None, data=None):
    marker = "HC_"+health.hashlib.sha256(("cn:fixture/"+protocol).encode()).hexdigest()[:8]+"_12"
    def replace(value):
        if isinstance(value, dict):
            return {key: replace(item) for key, item in value.items()}
        if isinstance(value, list):
            return [replace(item) for item in value]
        return marker if value == "OK" else value
    events, data = replace(events), replace(data)
    gateway = type("Fixture", (), dict(by_model={"cn:fixture": "synthetic"}, timeout=5,
                   connection=lambda _: Connection(Response(events, data))))()
    return health.probe(gateway, dict(model="cn:fixture", protocol=protocol, mode=mode))


class ProviderProbeTests(unittest.TestCase):
    def test_tool_result_loop_uses_real_call_identity_and_requires_exact_followup(self):
        for protocol in health.PROTOCOLS:
            with self.subTest(protocol=protocol):
                marker = "HC_"+health.hashlib.sha256(("cn:fixture/"+protocol).encode()).hexdigest()[:8]+"_12"
                first = {
                    "chat": dict(choices=[dict(message=dict(role="assistant", content=None, tool_calls=[dict(id="fixture-call", type="function", function=dict(name="health_echo", arguments='{"value":"OK"}'))]), finish_reason="tool_calls")]),
                    "responses": dict(status="completed", output=[dict(type="function_call", call_id="fixture-call", name="health_echo", arguments='{"value":"OK"}')]),
                    "messages": dict(stop_reason="tool_use", content=[dict(type="tool_use", id="fixture-call", name="health_echo", input=dict(value="OK"))]),
                    "gemini": dict(candidates=[dict(finishReason="STOP", content=dict(role="model", parts=[dict(functionCall=dict(name="health_echo", args=dict(value="OK")))]))]),
                }[protocol]
                final = {
                    "chat": dict(choices=[dict(message=dict(content=marker), finish_reason="stop")]),
                    "responses": dict(status="completed", output=[dict(type="message", content=[dict(type="output_text", text=marker)])]),
                    "messages": dict(stop_reason="end_turn", content=[dict(type="text", text=marker)]),
                    "gemini": dict(candidates=[dict(finishReason="STOP", content=dict(parts=[dict(text=marker)]))]),
                }[protocol]
                responses = iter([Response(data=first), Response(data=final)])
                requests = []
                class LoopConnection(Connection):
                    def request(self, method, path, body, headers):
                        requests.append(json.loads(body))
                gateway = type("Fixture", (), dict(by_model={"cn:fixture": "synthetic"}, timeout=5,
                               connection=lambda _: LoopConnection(next(responses))))()
                result = health.probe(gateway, dict(model="cn:fixture", protocol=protocol, mode="tool_loop"))
                self.assertTrue(result["passed"])
                self.assertTrue(result["tool_result_continuation_passed"])
                self.assertEqual(len(requests), 2)
                serialized = json.dumps(requests[1])
                self.assertIn(marker, serialized)
                if protocol != "gemini":
                    self.assertIn("fixture-call", serialized)
                self.assertNotIn(marker, json.dumps(result))
                self.assertFalse(any(key.startswith("_") for key in result))

    def test_actual_chat_sse_completion_and_latency_are_observed(self):
        result = execute("chat", "stream", [dict(choices=[dict(delta=dict(content="OK"), finish_reason=None)]),
                                               dict(choices=[dict(delta={}, finish_reason="stop")])])
        self.assertTrue(result["passed"])
        self.assertGreater(result["text_chars"], 2)
        self.assertIsNotNone(result["first_text_ms"])
        self.assertFalse(any(key in result for key in ("key", "prompt", "messages", "response")))

    def test_text_followed_by_truncation_is_not_a_pass(self):
        result = execute("chat", "plain", data=dict(choices=[dict(message=dict(content="partial"), finish_reason="length")]))
        self.assertFalse(result["passed"])
        self.assertEqual(result["error_code"], "output_truncated")

    def test_an_empty_200_response_is_not_a_pass(self):
        for protocol in health.PROTOCOLS:
            with self.subTest(protocol=protocol):
                self.assertFalse(execute(protocol, "plain")["passed"])

    def test_missing_requested_tool_is_not_a_pass(self):
        result = execute("messages", "tools", data=dict(content=[dict(type="text", text="OK")], stop_reason="end_turn"))
        self.assertFalse(result["passed"])

    def test_actual_tool_call_is_detected_without_recording_arguments(self):
        result = execute("responses", "tools", data=dict(status="completed", output=[dict(type="function_call", name="health_echo", arguments='{"value":"OK"}')]))
        self.assertTrue(result["passed"])
        self.assertNotIn("arguments", result)

    def test_tool_name_without_valid_requested_arguments_does_not_pass(self):
        for arguments in ("not JSON", '{"value":"wrong"}', "{}", None):
            result = execute("responses", "tools", data=dict(status="completed", output=[dict(type="function_call", name="health_echo", arguments=arguments)]))
            self.assertFalse(result["passed"])

    def test_actual_visual_color_must_be_correct(self):
        for color, accepted in [("red", True), ("blue", False)]:
            result = execute("chat", "image", data=dict(choices=[dict(message=dict(content=color), finish_reason="stop")]))
            self.assertEqual(result["passed"], accepted)

    def test_gemini_truncation_is_not_a_pass(self):
        result = execute("gemini", "plain", data=dict(candidates=[dict(content=dict(parts=[dict(text="partial")]), finishReason="MAX_TOKENS")]))
        self.assertFalse(result["passed"])

    def test_messages_stream_max_tokens_is_not_a_pass(self):
        result = execute("messages", "stream", [dict(type="content_block_delta", delta=dict(type="text_delta", text="partial")),
                                                   dict(type="message_delta", delta=dict(stop_reason="max_tokens")), dict(type="message_stop")])
        self.assertFalse(result["passed"])

    def test_wrong_short_answer_does_not_satisfy_the_canary(self):
        result = execute("chat", "plain", data=dict(choices=[dict(message=dict(content="wrong"), finish_reason="stop")]))
        self.assertFalse(result["passed"])
        self.assertEqual(result["error_code"], "unexpected_canary_or_arithmetic_answer")

    def test_non_success_terminal_does_not_pass_even_with_correct_answer(self):
        result = execute("chat", "plain", data=dict(choices=[dict(message=dict(content="OK"), finish_reason="content_filter")]))
        self.assertFalse(result["passed"])
        result = execute("gemini", "plain", data=dict(candidates=[dict(content=dict(parts=[dict(text="OK")]), finishReason="SAFETY")]))
        self.assertFalse(result["passed"])

    def test_multiturn_is_an_exact_match_not_a_substring(self):
        result = execute("chat", "multiturn", data=dict(choices=[dict(message=dict(content="not ORCHID"), finish_reason="stop")]))
        self.assertFalse(result["passed"])

    def test_extra_tool_arguments_are_rejected(self):
        self.assertFalse(health.valid_tool_arguments(dict(value="OK", extra="ignored")))

    def test_blue_image_does_not_accept_a_red_guess(self):
        result = execute("chat", "image-blue", data=dict(choices=[dict(message=dict(content="red"), finish_reason="stop")]))
        self.assertFalse(result["passed"])


if __name__ == "__main__":
    unittest.main()
