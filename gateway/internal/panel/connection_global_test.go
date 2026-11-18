package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"workbuddy2api/internal/authflow"
)

// TestConnectionWorkBuddyGlobalFullProtocolPersistsRealmAndDomain 覆盖 P1-05：
// Global 版必须走 www.workbuddy.ai 的 state→token→account→commit 全链路，
// 落盘文档保留 realm=global 与官方 domain，并触发恰好一次 runtime reload。
// 全部使用注入 RoundTripper 的合成响应，不重放真实授权。
func TestConnectionWorkBuddyGlobalFullProtocolPersistsRealmAndDomain(t *testing.T) {
	h := newHarness(t)
	defer h.server.Close()
	h.transport.replies["POST /runtime/reload"] = stubReply{Code: http.StatusOK, Body: `{"ok":true}`}
	var hosts []string
	client := &http.Client{Transport: connectionTransport(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host+r.URL.Path)
		if r.URL.Host != "www.workbuddy.ai" {
			t.Fatalf("global realm hit unexpected host: %s", r.URL.Host)
		}
		var data any
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			data = map[string]any{"state": "global-state", "authUrl": "https://www.workbuddy.ai/synthetic-global-auth"}
		case "/v2/plugin/auth/token":
			data = map[string]any{"accessToken": "synthetic-global-access", "refreshToken": "synthetic-global-refresh", "expiresIn": 7200, "domain": "www.workbuddy.ai"}
		case "/v2/plugin/login/account":
			if r.Header.Get("Authorization") != "Bearer synthetic-global-access" {
				t.Fatal("global account identity request not bearer authenticated")
			}
			data = map[string]any{"uid": "987654", "nickname": "Synthetic Global", "enterpriseId": "ent-global"}
		default:
			t.Fatalf("unexpected global authorization path: %s", r.URL.Path)
		}
		b, _ := json.Marshal(map[string]any{"code": 0, "data": data})
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
	})}
	d := &legacyConnection{s: h.server, provider: "workbuddy", client: client}
	request := authflow.Request{Provider: "workbuddy", Method: "qr", Realm: "global"}
	start, err := d.Start(context.Background(), request)
	if err != nil || start.Status != "waiting_scan" || start.ActionURL != "https://www.workbuddy.ai/synthetic-global-auth" {
		t.Fatalf("global start failed: %+v %v", start, err)
	}
	verified, err := d.Step(context.Background(), authflow.Flow{View: authflow.View{Request: request}, Secret: start.Secret}, "poll", authflow.Input{})
	if err != nil || verified.Status != "verified" {
		t.Fatalf("global poll/verify failed: %+v %v", verified, err)
	}
	committed, err := d.Step(context.Background(), authflow.Flow{View: authflow.View{Request: request}, Secret: verified.Secret}, "commit", authflow.Input{})
	if err != nil || committed.Status != "succeeded" || committed.AccountID != "987654" {
		t.Fatalf("global commit failed: %+v %v", committed, err)
	}
	if len(hosts) != 3 {
		t.Fatalf("expected exactly 3 official calls (state/token/account), got %v", hosts)
	}
	if h.transport.count("POST /runtime/reload") != 1 {
		t.Fatal("global account commit must reload runtime exactly once")
	}
	raw, err := os.ReadFile(filepath.Join(h.server.opts.AuthDir, "workbuddy-987654.json"))
	if err != nil {
		t.Fatal("global auth document missing")
	}
	var saved struct {
		Account map[string]any `json:"account"`
		Auth    map[string]any `json:"auth"`
	}
	if json.Unmarshal(raw, &saved) != nil {
		t.Fatal("global auth document unparsable")
	}
	if jsonString(saved.Account["nickname"]) != "Synthetic Global" || jsonString(saved.Auth["realm"]) != "global" || jsonString(saved.Auth["domain"]) != "www.workbuddy.ai" || jsonInt64(saved.Auth["expiresAt"]) <= time.Now().Unix() {
		t.Fatalf("global document lost identity/realm/domain/expiry: %+v", saved)
	}
}

// TestConnectionWorkBuddyGlobalPendingAndIdentityFailure 覆盖 Global 的另外两条
// 合成路径：token 返回 login ing 时保持 waiting_scan 且不进入账号核验；账号接口
// 返回非法 uid 时给出 manual_action，且绝不落盘、不 reload。
func TestConnectionWorkBuddyGlobalPendingAndIdentityFailure(t *testing.T) {
	h := newHarness(t)
	defer h.server.Close()
	mode := "pending"
	client := &http.Client{Transport: connectionTransport(func(r *http.Request) (*http.Response, error) {
		var code int
		var msg string
		var data any
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			code, data = 0, map[string]any{"state": "global-state", "authUrl": "https://www.workbuddy.ai/synthetic-global-auth"}
		case "/v2/plugin/auth/token":
			if mode == "pending" {
				code, msg = 7, "login ing"
			} else {
				code, data = 0, map[string]any{"accessToken": "synthetic-global-access", "refreshToken": "r", "expiresIn": 3600, "domain": "www.workbuddy.ai"}
			}
		case "/v2/plugin/login/account":
			code, data = 0, map[string]any{"uid": "bad identity!", "nickname": "Bad"}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		b, _ := json.Marshal(map[string]any{"code": code, "msg": msg, "data": data})
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
	})}
	d := &legacyConnection{s: h.server, provider: "workbuddy", client: client}
	request := authflow.Request{Provider: "workbuddy", Method: "qr", Realm: "global"}
	start, err := d.Start(context.Background(), request)
	if err != nil || start.Status != "waiting_scan" {
		t.Fatalf("global start failed: %v", err)
	}
	pending, err := d.Step(context.Background(), authflow.Flow{View: authflow.View{Request: request}, Secret: start.Secret}, "poll", authflow.Input{})
	if err != nil || pending.Status != "waiting_scan" {
		t.Fatalf("login ing must stay waiting_scan: %+v %v", pending, err)
	}
	mode = "verified"
	bad, err := d.Step(context.Background(), authflow.Flow{View: authflow.View{Request: request}, Secret: start.Secret}, "poll", authflow.Input{})
	if err != nil || bad.Status != "manual_action" || bad.ErrorCode != "account_identity_not_verified" {
		t.Fatalf("invalid global identity must be manual_action: %+v %v", bad, err)
	}
	if h.transport.count("POST /runtime/reload") != 0 {
		t.Fatal("failed identity verification must not reload runtime")
	}
	entries, err := os.ReadDir(h.server.opts.AuthDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "987654") || strings.HasPrefix(entry.Name(), "workbuddy-") {
			t.Fatalf("failed global identity wrote an auth document: %s", entry.Name())
		}
	}
}
