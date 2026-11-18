#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-25：账本不可读时密钥列表保留未知值，禁止把丢失的用量伪装成零。
# 2026-09-23：补日志窗口回归：60/120/300/600 是「请求条数」而不是原始日志行数，
#             取窗口时要按噪声比例放大 tail，并按请求行裁到目标条数。
# 2026-09-22：补密钥管理默认启用的口径回归：api_keys_file 未配置时按默认路径解析
#             （与网关同口径），显式空串仍关闭，且 api_key 也为空时不能显示成可用。
# 2026-09-20：覆盖有效期字段的透传、显式 null 与非法值拒绝，以及密钥列表合并累计用量。
# 2026-09-20：复制接口和逐密钥保护开关必须经过管理员会话、来源和字段校验。
# 2026-09-16：验证密钥管理的管理员登录、请求来源、大小和字段限制，使用本地服务与合成会话。
# 2026-09-17：覆盖模型绑定字段的透传与非法输入的本地拒绝。
# 2026-09-17：覆盖用量统计页的数据通道：登录保护 + 经本机管理 socket 读 /usage。
# 2026-09-17：覆盖请求日志解析：带 key= 列的新行、旧格式行与警告行分流。

import http.client
import json
import os
import tempfile
import threading
import unittest
from unittest.mock import patch
from http.server import ThreadingHTTPServer
import app
import key_management


class Handler(app.Handler):
    def _session(self):
        return {"u": "test-admin"} if self.headers.get("Cookie") == "test-session" else None

    def log_message(self, *args):
        pass


class KeyManagementTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def request(self, path="/api/keys", body='{"name":"client"}', headers=None, method="POST"):
        opts = {"Cookie": "test-session", "Content-Type": "application/json", "X-Admin-Request": "1"}
        if headers:
            opts.update(headers)
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=2)
        try:
            connection.request(method, path, body=body, headers=opts)
            response = connection.getresponse()
            return response.status, json.loads(response.read())
        finally:
            connection.close()

    def test_requires_admin_session_not_api_key(self):
        with patch.object(app.key_management, "request") as upstream:
            for method in ("GET", "POST"):
                status, _ = self.request(headers={"Cookie": "", "Authorization": "Bearer ordinary-api-key"}, method=method)
                self.assertEqual(status, 401)
            upstream.assert_not_called()

    def test_rejects_untrusted_origin_and_missing_custom_header(self):
        for headers in ({"Origin": "https://unrelated.invalid"}, {"X-Admin-Request": ""}):
            with patch.object(app.key_management, "request") as upstream:
                self.assertEqual(self.request(headers=headers)[0], 403)
                upstream.assert_not_called()

    def test_validates_body_size_type_and_ids(self):
        # 上限是 64 KiB（密钥库允许 64 项 × 256 字节的模型绑定，合法体可超过 8 KiB）；
        # 8193 字节的纯空白不再是「超限」，而是「不是完整 JSON」。
        cases = [("/api/keys", "[]", 400), ("/api/keys", "null", 400), ("/api/keys", "broken", 400), ("/api/keys/delete", '{"id":"../../config"}', 400), ("/api/keys/update", '{"id":"legacy","sha256":"x"}', 400)]
        for path, body, expected in cases:
            with self.subTest(path=path, body=body[:40]), patch.object(app.key_management, "request") as upstream:
                self.assertEqual(self.request(path, body)[0], expected)
                upstream.assert_not_called()
        # 超限只看 Content-Length，不读 body：用声明的长度造超限请求，避免
        # 客户端真写 64 KiB 时服务端已关闭连接、Windows 上变成 ConnectionReset。
        with patch.object(app.key_management, "request") as upstream:
            self.assertEqual(self.request(headers={"Content-Length": "65537"})[0], 413)
            upstream.assert_not_called()
        self.assertEqual(self.request(headers={"Content-Type": "text/plain"})[0], 415)

    def test_accepts_maximum_legal_model_binding_body(self):
        """合法最大绑定（64 项 × 每项 200+ 字节）必须放行。

        旧上限 8192 会把这种请求 413 掉，而密钥库本身允许同一份配置；
        用户看到的是「请求体超过大小限制」，却找不到任何超限字段。
        """
        models = ["doubao:" + "m" * 190 + chr(97 + i % 26) + chr(97 + i // 26) for i in range(64)]
        body = json.dumps({"name": "many", "models": models})
        self.assertGreater(len(body), 8192)
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"),                 patch.object(app.key_management, "request", return_value=(201, {"ok": True, "key": "synthetic"})) as upstream:
            code, result = self.request(body=body)
            self.assertEqual(code, 201, result)
            upstream.assert_called_once()

    def test_forwards_only_authenticated_management_requests(self):
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), patch.object(app.key_management, "request", return_value=(201, {"ok": True, "key": "synthetic-once"})) as upstream:
            code, result = self.request()
            self.assertEqual(code, 201)
            self.assertEqual(result["key"], "synthetic-once")
            upstream.assert_called_once_with("/tmp/test.sock", "POST", "/keys", {"name": "client"})

    def test_forwards_model_binding_and_rejects_bad_lists(self):
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), patch.object(app.key_management, "request", return_value=(201, {"ok": True, "key": "synthetic"})) as upstream:
            code, _ = self.request(body=json.dumps({"name": "c", "models": ["cn:deepseek-v4.1-flash"]}))
            self.assertEqual(code, 201)
            upstream.assert_called_once_with("/tmp/test.sock", "POST", "/keys",
                                             {"name": "c", "models": ["cn:deepseek-v4.1-flash"]})
        bad_update = [
            '{"id":"legacy","models":"cn:deepseek-v4.1-flash"}',
            '{"id":"legacy","models":["a b"]}',
            '{"id":"legacy","models":["dup","dup"]}',
            '{"id":"legacy","models":["' + "x" * 65 + '"]}',
            # 裸名绑定按完整名逐字比对时一条都匹配不上，面板这一层就要拦住，
            # 不能保存成功却让调用方全撞 403。
            '{"id":"legacy","models":["deepseek-v4.1-flash"]}',
            '{"id":"legacy","models":["cn:deepseek-v4.1-flash","glm-5.2"]}',
            # 前缀大小写非法（命名空间必须小写开头）。
            '{"id":"legacy","models":["US:deepseek-v4.1-flash"]}',
            '{"id":"legacy","models":[":deepseek-v4.1-flash"]}',
        ]
        # 未知但语法合法的命名空间必须放行到网关：前缀是动态命名空间，
        # 面板不维护平台表；该模型是否存在由网关按完整名比对。
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"),                 patch.object(app.key_management, "request", return_value=(200, {"ok": True})) as upstream:
            self.assertEqual(self.request("/api/keys/update",
                                         '{"id":"legacy","models":["us:deepseek-v4.1-flash"]}')[0], 200)
            upstream.assert_called_once()
        for body in bad_update:
            with self.subTest(body=body[:40]), patch.object(app.key_management, "request") as upstream:
                self.assertEqual(self.request("/api/keys/update", body)[0], 400)
                upstream.assert_not_called()

    def test_rejects_bare_model_binding_on_create(self):
        """创建路径同样拦裸名：这是手填最容易踩的写法。"""
        for models in (["deepseek-v4.1-flash"], ["cn:hy3", "glm-5.2"], ["global:"]):
            with self.subTest(models=models), patch.object(app.key_management, "request") as upstream:
                code, result = self.request(body=json.dumps({"name": "c", "models": models}))
                self.assertEqual(code, 400, result)
                upstream.assert_not_called()

    def test_copy_requires_admin_and_valid_origin(self):
        for headers in ({"Cookie": "", "Authorization": "Bearer ordinary-key"},
                        {"Origin": "https://unrelated.invalid"}, {"X-Admin-Request": ""}):
            with self.subTest(headers=headers), patch.object(app.key_management, "request") as upstream:
                code, _ = self.request("/api/keys/copy", '{"id":"legacy"}', headers=headers)
                self.assertEqual(code, 401 if not headers.get("Cookie", "test-session") else 403)
                upstream.assert_not_called()

    def test_copy_forwards_only_id_to_private_endpoint(self):
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                patch.object(app.key_management, "request", return_value=(200, {"ok": True, "key": "synthetic-copy"})) as upstream:
            code, result = self.request("/api/keys/copy", '{"id":"legacy"}')
            self.assertEqual(code, 200)
            self.assertEqual(result["key"], "synthetic-copy")
            upstream.assert_called_once_with("/tmp/test.sock", "POST", "/keys/legacy/copy", {})
        for body in ('{"id":"../../config"}', '{"id":"legacy","enabled":true}'):
            with patch.object(app.key_management, "request") as upstream:
                self.assertEqual(self.request("/api/keys/copy", body)[0], 400)
                upstream.assert_not_called()

    def test_reasoning_guard_boolean_passes_create_and_update(self):
        for path, body, method, endpoint, forwarded in (
            ("/api/keys", {"name": "client", "reasoning_loop_guard": False}, "POST", "/keys", {"name": "client", "reasoning_loop_guard": False}),
            ("/api/keys/update", {"id": "legacy", "reasoning_loop_guard": True}, "PATCH", "/keys/legacy", {"reasoning_loop_guard": True}),
        ):
            with self.subTest(path=path), patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                    patch.object(app.key_management, "request", return_value=(200, {"ok": True})) as upstream:
                self.assertEqual(self.request(path, json.dumps(body))[0], 200)
                upstream.assert_called_once_with("/tmp/test.sock", method, endpoint, forwarded)
        for value in ("false", 0, None, []):
            with self.subTest(value=value), patch.object(app.key_management, "request") as upstream:
                self.assertEqual(self.request("/api/keys/update", json.dumps({"id": "legacy", "reasoning_loop_guard": value}))[0], 400)
                upstream.assert_not_called()

    def test_usage_requires_admin_session(self):
        with patch.object(app.key_management, "request") as upstream:
            status, _ = self.request("/api/usage", body=None,
                                     headers={"Cookie": "", "Authorization": "Bearer ordinary-api-key"},
                                     method="GET")
            self.assertEqual(status, 401)
            upstream.assert_not_called()

    def test_expiry_passes_create_and_update_and_rejects_bad_values(self):
        future = "2030-01-02T03:04:05Z"
        for path, body, method, endpoint, forwarded in (
            ("/api/keys", {"name": "c", "expires_at": future}, "POST", "/keys",
             {"name": "c", "expires_at": future}),
            ("/api/keys", {"name": "c", "expires_at": None}, "POST", "/keys",
             {"name": "c", "expires_at": None}),
            ("/api/keys/update", {"id": "legacy", "expires_at": future}, "PATCH", "/keys/legacy",
             {"expires_at": future}),
            ("/api/keys/update", {"id": "legacy", "expires_at": None}, "PATCH", "/keys/legacy",
             {"expires_at": None}),
        ):
            with self.subTest(body=body), patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                    patch.object(app.key_management, "request", return_value=(200, {"ok": True})) as upstream:
                self.assertEqual(self.request(path, json.dumps(body))[0], 200)
                upstream.assert_called_once_with("/tmp/test.sock", method, endpoint, forwarded)
        for value in (123, [], {"at": future}, "x" * 41):
            with self.subTest(value=value), patch.object(app.key_management, "request") as upstream:
                body = {"id": "legacy", "expires_at": value}
                self.assertEqual(self.request("/api/keys/update", json.dumps(body))[0], 400)
                upstream.assert_not_called()

    def test_key_list_merges_usage_totals(self):
        keys_payload = {"ok": True, "keys": [
            {"id": "key_a", "name": "A", "masked_key": "wbk_a…a"},
            {"id": "key_b", "name": "B", "masked_key": "wbk_b…b"},
        ], "max_keys": 256}
        usage_payload = {"ok": True, "totals": {"requests": 5},
                         "keys": [{"key_id": "key_a", "totals": {"requests": 4, "total_tokens": 4096}}]}
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                patch.object(app.key_management, "request", side_effect=[(200, keys_payload), (200, usage_payload)]) as upstream:
            code, result = self.request("/api/keys", body=None, method="GET")
            self.assertEqual(code, 200)
            self.assertEqual([k["total_tokens"] for k in result["keys"]], [4096, 0])
            self.assertTrue(result["usage_available"])
            self.assertEqual(upstream.call_count, 2)

    def test_key_list_survives_disabled_ledger(self):
        keys_payload = {"ok": True, "keys": [{"id": "key_a", "name": "A"}], "max_keys": 256}
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                patch.object(app.key_management, "request", side_effect=[(200, keys_payload), (200, {"ok": False, "message": "未启用"})]):
            code, result = self.request("/api/keys", body=None, method="GET")
            self.assertEqual(code, 200)
            self.assertIsNone(result["keys"][0]["total_tokens"])
            self.assertFalse(result["usage_available"])

    def test_usage_proxies_management_socket(self):
        payload = {"ok": True, "totals": {"requests": 3, "total_tokens": 120},
                   "keys": [{"key_id": "k1", "name": "团队 A", "masked_key": "wb2a_ab…cd",
                             "totals": {"requests": 3, "total_tokens": 120},
                             "models": [{"model": "cn:deepseek-v4.1-flash",
                                         "totals": {"requests": 3, "total_tokens": 120}}]}],
                   "since": "2026-09-17T00:00:00Z", "updated_at": "2026-09-17T01:00:00Z",
                   "file": "/opt/workbuddy2api/data/usage.json"}
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                patch.object(app.key_management, "request", return_value=(200, payload)) as upstream:
            code, result = self.request("/api/usage", body=None, method="GET")
            self.assertEqual(code, 200)
            self.assertTrue(result["ok"])
            self.assertEqual(result["keys"][0]["name"], "团队 A")
            upstream.assert_called_once_with("/tmp/test.sock", "GET", "/usage", None)

    def test_usage_reports_disabled_ledger(self):
        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
                patch.object(app.key_management, "request", return_value=(200, {"ok": False, "message": "用量账本未启用"})):
            code, result = self.request("/api/usage", body=None, method="GET")
            self.assertEqual(code, 200)
            self.assertFalse(result["ok"])
            self.assertIn("未启用", result["message"])

    def test_parse_request_log_keeps_key_column_and_warnings(self):
        text = "\n".join([
            "2026/09/17 22:04:21 INFO boot",
            "| #098 | 22:04:21 | global:deep | stream | 200 | key=团队 A | uid=1e04e34d | TTFB=3414ms | tok=110 | 34.3tok/s | total=3.4s |",
            "| #099 | 22:04:35 | cn:deepseek | stream | 200 | key=- | uid=fbede7cd | TTFB=2749ms | tok=161 | 49.5tok/s | total=3.3s |",
            "2026/09/17 22:05:00 WARN: [upstream] chat_stream uid=1e04e34d: upstream 400 channel_rejected",
            "| #100 | 22:05:01 | cn:deepseek | sync | 502 | key=脚本机 | uid=4e183777 | TTFB=- | tok=- | -tok/s | total=0.2s |",
        ])
        rows, other = app.parse_request_log(text)
        self.assertEqual(len(rows), 3)
        self.assertEqual(rows[0]["seq"], "098")
        self.assertEqual(rows[0]["key"], "团队 A")
        self.assertEqual(rows[0]["model"], "global:deep")
        self.assertEqual(rows[0]["status"], "200")
        self.assertEqual(rows[0]["uid"], "1e04e34d")
        self.assertEqual(rows[0]["tok"], "110")
        self.assertEqual(rows[0]["total"], "3.4s")
        self.assertEqual(rows[1]["key"], "-")
        self.assertEqual(rows[2]["status"], "502")
        self.assertEqual(len(other), 2)
        self.assertIn("channel_rejected", other[-1])

    def test_parse_request_log_accepts_legacy_rows_without_key(self):
        rows, other = app.parse_request_log(
            "| #001 | 10:00:00 | deepseek | stream | 200 | uid=00e26541 | TTFB=100ms | tok=5 | 50.0tok/s | total=0.1s |")
        self.assertEqual(len(rows), 1)
        # 旧格式没有 key= 列：归一化成 "-"，前端按同一列渲染。
        self.assertEqual(rows[0]["key"], "-")
        self.assertEqual(other, [])

    def test_parse_request_log_reads_cache_column(self):
        """v2.6.19 在 hit= 与 tok= 之间插了 cache= 列。

        回归背景（2026-10-10 真踩过）：正则原先把 hit= 与 tok= 写成紧邻，
        插入 cache= 后整行匹配失败，请求日志页直接显示「还没有请求记录」——
        网关日志本身是好的，是面板解析挂了。cache= 必须是可选取，且旧行
        （无该列）仍要能解析。
        """
        rows, _ = app.parse_request_log("\n".join([
            # 新格式：cache=prev=N
            "| #012 | 22:04:21 | global:deep | stream | 200 | key=团队 A | uid=1e04e34d | "
            "TTFB=3414ms | in=306401 | hit=298112 | cache=prev=305900 | tok=110 | 34.3tok/s | total=3.4s | rid=req_ABC |",
            # 新格式：cache=cold
            "| #013 | 22:04:31 | global:deep | stream | 200 | key=团队 A | uid=1e04e34d | "
            "TTFB=8734ms | in=399973 | hit=1408 | cache=cold | tok=365 | 36.5tok/s | total=10.0s |",
            # 旧格式：无 cache= 列
            "| #098 | 22:04:21 | global:deep | stream | 200 | key=团队 A | uid=1e04e34d | "
            "TTFB=3414ms | in=306401 | hit=298112 | tok=110 | 34.3tok/s | total=3.4s |",
        ]))
        self.assertEqual(len(rows), 3, "cache= 列不应让任何一行解析失败")
        self.assertEqual(rows[0]["cache"], "prev=305900")
        self.assertEqual(rows[0]["hit"], "298112")
        self.assertEqual(rows[0]["tok"], "110")
        self.assertEqual(rows[1]["cache"], "cold")
        # 旧行没有该列 → 归一化成 "-"（与 key= 同一约定），而不是丢掉整行。
        self.assertEqual(rows[2]["cache"], "-")
        self.assertEqual(rows[2]["tok"], "110")


class LogWindowTests(unittest.TestCase):
    """日志页的 60/120/300/600 是「请求条数」语义，不是 docker --tail 的原始行数。

    日志里请求行与 WARN/ERR 行混排，噪声重时请求约占一半——直接拿按钮数字去 tail，
    按钮写 60 就只能出 ~30 条请求。这里锁住「按比例放大再裁到目标条数」的口径。
    """

    def test_window_is_larger_than_the_request_count(self):
        """取窗口必须大于目标请求数，否则噪声会把请求行挤掉。"""
        for want in (60, 120, 300, 600):
            with self.subTest(want=want):
                self.assertGreater(app.request_window_size(want), want)

    def test_window_is_capped(self):
        """放大后要封顶，避免一次拉出过长的 docker logs。"""
        self.assertLessEqual(app.request_window_size(1000), app.LOG_TAIL_MAX)
        self.assertLessEqual(app.request_window_size(10_000), app.LOG_TAIL_MAX)

    def test_window_survives_heavy_noise(self):
        """噪声占 3/4 时，放大后的窗口仍要够拿到目标条数请求。"""
        want = 120
        tail = app.request_window_size(want)
        # 极端情况：每 4 行里只有 1 行是请求。
        request_lines = tail // 4
        self.assertGreaterEqual(request_lines, want)


class KeyManagementDefaultTests(unittest.TestCase):
    """api_keys_file 的默认值必须与网关 cmd/server 的 applyAPIKeysDefault 同口径。

    这一层是「密钥管理尚未启用」这条提示的真正来源：面板读的是原始 config.json，
    网关读的是归一化后的配置。两边默认值一旦分叉，用户就会看到网关明明能用、
    面板却说未启用。
    """

    def write_config(self, payload):
        handle = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8")
        self.addCleanup(os.unlink, handle.name)
        json.dump(payload, handle)
        handle.close()
        return handle.name

    def test_missing_field_defaults_to_enabled(self):
        """核心回归：没写 api_keys_file 也要能管理密钥（此前直接报未启用）。"""
        for payload in ({"api_key": "k"}, {"listen": ":9999"}, {}):
            with self.subTest(payload=payload):
                path = self.write_config(payload)
                self.assertEqual(key_management.socket_path(path, "/srv"),
                                 os.path.normpath("/srv/data/api_keys.sock"))

    def test_explicit_path_wins(self):
        path = self.write_config({"api_keys_file": "./custom/keys.json"})
        self.assertEqual(key_management.socket_path(path, "/srv"),
                         os.path.normpath("/srv/custom/api_keys.sock"))

    def test_explicit_empty_still_enables(self):
        """空串按「没配」处理：历史 config.example.json 里就是空串，照抄它的部署
        很多，把它当成关闭信号正是「密钥管理尚未启用」这条提示的来源。"""
        for payload in ({"api_keys_file": "", "api_key": "k"},
                        {"api_keys_file": ""},
                        {"api_keys_file": "   "}):
            with self.subTest(payload=payload):
                path = self.write_config(payload)
                self.assertEqual(key_management.socket_path(path, "/srv"),
                                 os.path.normpath("/srv/data/api_keys.sock"))

    def test_enabled_false_disables(self):
        """显式关闭是唯一能让密钥库不启用的写法。"""
        path = self.write_config({"api_keys_enabled": False, "api_key": "k"})
        self.assertIsNone(key_management.socket_path(path, "/srv"))

    def test_enabled_true_keeps_default_path(self):
        path = self.write_config({"api_keys_enabled": True})
        self.assertEqual(key_management.socket_path(path, "/srv"),
                         os.path.normpath("/srv/data/api_keys.sock"))

    def test_null_treated_as_unset(self):
        """显式 null 与「没写」同义，走默认启用。"""
        path = self.write_config({"api_keys_file": None, "api_key": "k"})
        self.assertEqual(key_management.socket_path(path, "/srv"),
                         os.path.normpath("/srv/data/api_keys.sock"))

    def test_absolute_path_kept(self):
        path = self.write_config({"api_keys_file": "/var/lib/wb2api/keys.json"})
        # 期望值同样过一遍 normpath：面板跑在 Linux 容器里，但测试也会在 Windows
        # 开发机上执行，分隔符由平台决定，不该让测试把这条算成失败。
        self.assertEqual(key_management.socket_path(path, "/srv"),
                         os.path.normpath("/var/lib/wb2api/api_keys.sock"))

    def test_container_prefixed_path_remapped_to_base(self):
        """容器内写成 /app/... 的路径，在宿主机/面板侧要换到 BASE 下解析。"""
        path = self.write_config({"api_keys_file": "/app/data/api_keys.json"})
        self.assertEqual(key_management.socket_path(path, "/opt/workbuddy2api"),
                         os.path.normpath("/opt/workbuddy2api/data/api_keys.sock"))


class ModelsEndpointTests(unittest.TestCase):
    """GET /api/models 必须把展示字段（倍率/名称/供应商/能力）透传给前端。

    2026-10-08：该端点此前只回一串 id，密钥绑定表单因此看不到扣费倍率。改为一并回
    items 后，这里锁定「透传什么、省略什么」——缺字段要省略而不是编造，同时旧的
    models 纯 id 列表必须保留，否则既有前端会拿不到值。
    """

    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def get_models(self, payload):
        with patch.object(app, "gateway_get", return_value=payload):
            connection = http.client.HTTPConnection(*self.server.server_address, timeout=2)
            try:
                connection.request("GET", "/api/models",
                                   headers={"Cookie": "test-session", "X-Admin-Request": "1"})
                response = connection.getresponse()
                return response.status, json.loads(response.read())
            finally:
                connection.close()

    def test_forwards_display_fields_and_keeps_id_list(self):
        status, body = self.get_models({"data": [
            {"id": "cn:glm-5.3", "credits": "x0.79", "name": "GLM-5.3", "vendor": "e",
             "tags": ["craft"], "supports_reasoning": True, "supports_tool_call": True,
             "supports_images": False},
            {"id": "cn:hy3", "credits": "x0.00", "name": "Hy3", "vendor": "j"},
        ]})
        self.assertEqual(status, 200)
        self.assertTrue(body["ok"])
        # 旧的纯 id 列表必须保留（既有前端读的是它）。
        self.assertEqual(body["models"], ["cn:glm-5.3", "cn:hy3"])
        by_id = {i["id"]: i for i in body["items"]}
        self.assertEqual(by_id["cn:glm-5.3"]["credits"], "x0.79")
        self.assertEqual(by_id["cn:glm-5.3"]["vendor"], "e")
        self.assertTrue(by_id["cn:glm-5.3"]["supports_reasoning"])
        self.assertTrue(by_id["cn:glm-5.3"]["supports_tool_call"])
        # 上游没给的字段要省略，不能编造一个 false。
        self.assertNotIn("supports_images", by_id["cn:glm-5.3"])
        self.assertNotIn("supports_reasoning", by_id["cn:hy3"])

    def test_skips_entries_without_id_and_tolerates_missing_payload(self):
        status, body = self.get_models({"data": [
            {"credits": "x1.0"},          # 没有 id：跳过
            {"id": ""},                    # 空 id：跳过
            {"id": "cn:ok"},
        ]})
        self.assertEqual(status, 200)
        self.assertEqual(body["models"], ["cn:ok"])
        self.assertEqual(len(body["items"]), 1)
        # 网关读不到时（None）不能炸，回空列表而不是 500。
        status, body = self.get_models(None)
        self.assertEqual(status, 200)
        self.assertEqual(body["models"], [])
        self.assertEqual(body["items"], [])

    def test_provider_catalog_failures_reach_the_picker(self):
        """某个 provider 的目录拉取失败必须传到面板，否则用户以为「模型就是少」。

        Go 面板的 modelsPayload 已透传 /v1/models 的 provider_errors（P1-07，只含
        {provider, code}），新控制台据此提示「部分渠道目录暂不可用」。Python 面板
        （WB2API_PANEL_GO=0 兼容出口 / 容器默认）此前整体丢弃该字段：豆包目录超时
        时模型选择器只剩 WorkBuddy 的模型，用户无法区分「没有这个模型」与「这个渠道
        没读到」，可能误删已有绑定。这里锁住透传与字段白名单。
        """
        status, body = self.get_models({
            "data": [{"id": "cn:only"}],
            "provider_errors": [{"provider": "doubao", "code": "catalog_unavailable"},
                                {"provider": "trae", "code": "catalog_timeout", "message": "upstream detail"}],
        })
        self.assertEqual(status, 200)
        self.assertEqual(body["provider_errors"], [
            {"provider": "doubao", "code": "catalog_unavailable"},
            {"provider": "trae", "code": "catalog_timeout"},
        ], "只透传 provider/code，不得把上游原文 message 带进面板")
        # 没有错误时字段整体省略，前端据此判断「全部渠道正常」。
        status, body = self.get_models({"data": [{"id": "cn:only"}]})
        self.assertNotIn("provider_errors", body)

    def post_models_refresh(self, gateway_result, models_payload):
        """gateway_result 为 (code, payload) 时按该码返回，否则视为 (200, payload)。"""
        """触发面板的「刷新模型」，返回 (面板响应, 是否真的调了网关刷新)。"""
        called = {"refresh": 0}

        def fake_request(socket, method, endpoint, body=None, **kw):
            if endpoint == "/models/refresh":
                called["refresh"] += 1
                if isinstance(gateway_result, tuple):
                    return gateway_result
                return 200, gateway_result
            return 200, models_payload

        with patch.object(app.key_management, "socket_path", return_value="/tmp/test.sock"), \
             patch.object(app.key_management, "request", side_effect=fake_request):
            connection = http.client.HTTPConnection(*self.server.server_address, timeout=2)
            try:
                connection.request("POST", "/api/models/refresh", body="{}",
                                   headers={"Cookie": "test-session", "Content-Type": "application/json",
                                            "X-Admin-Request": "1"})
                response = connection.getresponse()
                return response.status, json.loads(response.read()), called
            finally:
                connection.close()

    def test_refresh_returns_the_fresh_list(self):
        """刷新成功时要把新列表一并回给前端（省一次往返），并带上 refreshed 标记。"""
        status, body, called = self.post_models_refresh(
            {"ok": True, "before": 2, "after": 3},
            {"data": [{"id": "cn:new", "credits": "x0.42"}]})
        self.assertEqual(status, 200)
        self.assertEqual(called["refresh"], 1, "必须真的调用网关的强制刷新")
        self.assertTrue(body["ok"])
        self.assertTrue(body["refreshed"])
        self.assertEqual(body["models"], ["cn:new"])
        self.assertEqual(body["items"][0]["credits"], "x0.42")

    def test_refresh_failure_is_reported_not_hidden(self):
        """刷新失败必须如实回错，不能让「没刷到」看起来像「已刷新」。"""
        status, body, called = self.post_models_refresh(
            (503, {"error": {"code": "models_unavailable", "message": "upstream refused"}}), {"data": []})
        self.assertEqual(status, 200)
        self.assertEqual(called["refresh"], 1)
        self.assertFalse(body["ok"], "失败时 ok 必须为 false")
        self.assertNotIn("refreshed", body, "失败不得带 refreshed 标记")
        self.assertIn("upstream refused", body["message"])

    def test_refresh_requires_admin_session(self):
        """刷新走管理写路径，未登录必须被挡。"""
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=2)
        try:
            connection.request("POST", "/api/models/refresh", body="{}",
                               headers={"Cookie": "", "Content-Type": "application/json",
                                        "X-Admin-Request": "1"})
            response = connection.getresponse()
            self.assertEqual(response.status, 401)
            response.read()
        finally:
            connection.close()


if __name__ == "__main__":
    unittest.main()


class TraeModelBindingTest(unittest.TestCase):
    """执行源码及打包前端，验证目录绑定、历史绑定和任意 provider 前缀搜索。"""

    def setUp(self):
        here = os.path.dirname(os.path.abspath(__file__))
        self.sources = {}
        for name in ("app.py", "keys.js", "index.html"):
            with open(os.path.join(here, name), "r", encoding="utf-8") as fh:
                self.sources[name] = fh.read()

    def test_backend_accepts_dynamic_provider_prefix(self):
        # 前缀是动态命名空间（cn/global/trae/doubao/…），与 Go 面板的
        # modelBindingError 和密钥库的 splitRealm 同一口径；写死 cn|global|trae
        # 会把豆包模型绑定的合法请求拒成 400。
        source = self.sources["app.py"]
        self.assertIn("PREFIXED_MODEL_RE = re.compile(", source)
        self.assertIn("^[a-z][a-z0-9_-]{0,31}:.+$", source)
        self.assertIn("PREFIXED_MODEL_RE.fullmatch(item)", source)
        self.assertNotIn(r're.fullmatch(r"(cn|global|trae):.+"', self.sources["app.py"])

    def _run_frontend_behavior(self, source_name, expression):
        from html.parser import HTMLParser
        from pathlib import Path
        import test_frontend_behavior as frontend

        if not frontend.NODE:
            self.skipTest("Node.js is required for frontend behavior checks")
        if source_name == "keys.js":
            return frontend.FrontendHarness.run_frontend(self, expression, extra_sources=("keys.js",))

        class InlineScripts(HTMLParser):
            def __init__(self):
                super().__init__()
                self.active = False
                self.parts = []

            def handle_starttag(self, tag, attrs):
                if tag == "script":
                    attributes = dict(attrs)
                    self.active = not attributes.get("src") and attributes.get("type", "") not in ("application/json", "application/ld+json")

            def handle_endtag(self, tag):
                if tag == "script":
                    self.active = False
                    self.parts.append("\n")

            def handle_data(self, data):
                if self.active:
                    self.parts.append(data)

        parser = InlineScripts()
        parser.feed(self.sources[source_name])
        bundled_script = "".join(parser.parts)
        self.assertTrue(bundled_script.strip(), "built page must contain executable inline JavaScript")
        base = Path(os.environ.get("LOOMGATE_WORK_ROOT", "D:/LoomGate-work")) / "panel-behavior-tests"
        base.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="fixture-", dir=base) as directory:
            runtime = Path(directory)
            # Execute the complete built script exactly once using the existing
            # DOM harness. The external QR vendor is irrelevant to key binding.
            (runtime / "app.js").write_text(bundled_script, encoding="utf-8")
            (runtime / "usage.js").write_text("", encoding="utf-8")
            with patch.object(frontend, "PANEL", runtime):
                return frontend.FrontendHarness.run_frontend(self, expression)

    def test_frontend_accepts_trae_prefix(self):
        """真实表单提交按目录接受任意provider；未知模型拒绝，历史绑定可编辑保留。"""
        for name in ("keys.js", "index.html"):
            with self.subTest(name=name):
                result = self._run_frontend_behavior(name, """
var catalog=['cn:alpha','global:beta','trae:gamma','new-provider:delta'];
var writes=[];
api=async function(path,body){
  if(path==='api/models'||path==='api/models/refresh') return {ok:true,models:catalog.slice(),items:catalog.map(function(id){return {id:id};})};
  if(path==='api/keys'||path==='api/keys/update'){
    writes.push({path:path,models:body.models.slice()});
    return {key:'synthetic-created-key',entry:{copy_available:true}};
  }
  throw new Error('unexpected fixture API route');
};
toast=function(){};
loadKeys=async function(){};
['keyName','keyModels','btnCopyKey','keyRPM','keyConcurrency','keyQueueSeconds'].forEach(function(id){$('#'+id).focus=function(){};});
$('#keyDialog').showModal=function(){this.open=true;};
$('#keyDialog').close=function(){this.open=false;if(this.events.close)this.events.close();};
// Real input.value coerces numbers to strings; the minimal DOM harness does not.
['keyRPM','keyConcurrency','keyQueueSeconds'].forEach(function(id){
  var value='';Object.defineProperty($('#'+id),'value',{get:function(){return value;},set:function(v){value=String(v);},configurable:true});
});
await loadKeyModels();
var offered=catalog.map(function(id){return $('#keyModelPanel').innerHTML.indexOf(id)>=0;});
async function submit(models,key){
  openKeyEditor(key||null);
  await Promise.resolve();await Promise.resolve();
  $('#keyName').value='synthetic binding';
  if(models!==null)fillModelInput(models);
  var before=writes.length;
  await $('#keyForm').events.submit({preventDefault:function(){}});
  var result={sent:writes.length-before,error:!!$('#keyFormError').textContent,
              request:writes.length>before?writes[writes.length-1]:null};
  closeKeyEditor();return result;
}
var accepted=[];
for(var id of catalog)accepted.push(await submit([id],null));
var unknown=[];
for(var id of ['cn:missing','new-provider:missing','alpha'])unknown.push(await submit([id],null));
var historic={id:'synthetic-key-id',name:'synthetic historical key',models:['retired-provider:legacy']};
var retained=await submit(null,historic);
var historicRejectedAsNew=await submit(['retired-provider:legacy'],null);
return {offered:offered,accepted:accepted,unknown:unknown,retained:retained,historicRejectedAsNew:historicRejectedAsNew};
""")
                self.assertEqual(result["offered"], [True] * 4, "all directory models must appear in the picker")
                expected = ["cn:alpha", "global:beta", "trae:gamma", "new-provider:delta"]
                self.assertEqual([row["request"]["models"] if row["request"] else None for row in result["accepted"]],
                                 [[model] for model in expected], "selected directory models must reach the save request")
                self.assertTrue(all(row["sent"] == 1 and not row["error"] for row in result["accepted"]))
                self.assertTrue(all(row["sent"] == 0 and row["error"] for row in result["unknown"]),
                                "a plausible prefix alone must not authorize an unknown model")
                self.assertEqual(result["retained"]["request"],
                                 {"path": "api/keys/update", "models": ["retired-provider:legacy"]},
                                 "opening an editor must preserve its saved bindings when the catalog omits them")
                self.assertEqual(result["historicRejectedAsNew"]["sent"], 0,
                                 "a previous editor's historical models must not authorize a newly created key")
                self.assertTrue(result["historicRejectedAsNew"]["error"])

    def test_prefix_stripping_knows_trae(self):
        """搜索按模型本身排序；新增provider和只命中provider名称的结果也应正确处理。"""
        for name in ("keys.js", "index.html"):
            with self.subTest(name=name):
                result = self._run_frontend_behavior(name, """
var ids=['cn:glm-alpha','global:plain','trae:glm-beta','new-provider:glm-gamma','glm-provider:unrelated','cn:before-glm'];
api=async function(){return {models:ids,items:ids.map(function(id){return {id:id};})};};
await loadKeyModels();
fillModelInput(['global:plain']);
KS.modelQuery='gl';
var broad=filteredModelItems().map(function(item){return item.id;});
KS.modelQuery='glm gamma';
var narrow=filteredModelItems().map(function(item){return item.id;});
KS.modelQuery='gl unrelated';
var providerOnly=filteredModelItems().map(function(item){return item.id;});
KS.modelQuery='no-such-synthetic-model';
var empty=filteredModelItems().map(function(item){return item.id;});
return {broad:broad,narrow:narrow,providerOnly:providerOnly,empty:empty};
""")
                self.assertEqual(set(result["broad"][:3]),
                                 {"cn:glm-alpha", "trae:glm-beta", "new-provider:glm-gamma"},
                                 "model-name prefix matches must precede substring/provider-only matches")
                self.assertEqual(len(result["broad"]), 6, "provider-only matches remain discoverable")
                self.assertEqual(result["narrow"], ["new-provider:glm-gamma"])
                self.assertEqual(result["providerOnly"], ["glm-provider:unrelated"])
                self.assertEqual(result["empty"], [])
