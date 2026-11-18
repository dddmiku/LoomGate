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

type connectionTransport func(*http.Request) (*http.Response, error)

func (f connectionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestConnectionsRequireSessionOriginAndStrictInput(t *testing.T) {
	h := newHarness(t)
	h.server.opts.Console = true
	defer h.server.Close()
	doc := h.writeCredentials()
	for _, path := range []string{"/api/connections/providers", "/api/connections/flows"} {
		if h.do(http.MethodGet, path, "", nil).Code != 401 {
			t.Fatal("authentication state readable without session")
		}
	}
	for _, headers := range []map[string]string{{"Cookie": CookieName + "=" + h.token(doc)}, {"Cookie": CookieName + "=" + h.token(doc), "X-Admin-Request": "1", "Origin": "https://foreign.invalid"}} {
		if h.do(http.MethodPost, "/api/connections/start", `{"provider":"trae","method":"browser"}`, headers).Code != 403 {
			t.Fatal("unauthorized authentication mutation")
		}
	}
	w := h.postJSON(doc, "/api/connections/start", map[string]any{"provider": "trae", "method": "browser", "unexpected": true})
	if w.Code != 400 {
		t.Fatal("unknown auth field accepted")
	}
	w = h.postJSON(doc, "/api/connections/start", map[string]any{"provider": "trae", "method": "browser"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "waiting_authorization") {
		t.Fatal("existing Trae authorization entry lost")
	}
	if h.do(http.MethodGet, "/api/connections/flows", "", map[string]string{"Cookie": CookieName + "=" + h.token(doc)}).Code != 200 {
		t.Fatal("flow resume endpoint failed")
	}
}
func TestSafeLoginURLAllowsOnlyOfficialWorkBuddyAuthorizationHosts(t *testing.T) {
	for _, raw := range []string{
		"https://copilot.tencent.com/authorize",
		"https://www.workbuddy.ai/authorize",
	} {
		if !safeLoginURL(raw) {
			t.Errorf("official authorization URL rejected: %s", raw)
		}
	}

	for _, raw := range []string{
		"https://copilot.tencent.com.evil.invalid/authorize",
		"https://evil.copilot.tencent.com/authorize",
		"https://user@copilot.tencent.com/authorize",
		"https://copilot.tencent.com@evil.invalid/authorize",
		"http://copilot.tencent.com/authorize",
	} {
		if safeLoginURL(raw) {
			t.Errorf("untrusted authorization URL accepted: %s", raw)
		}
	}
}

func TestConnectionWorkBuddyProtocolStoresVerifiedResultWithoutHelperFiles(t *testing.T) {
	h := newHarness(t)
	defer h.server.Close()
	h.transport.replies["POST /runtime/reload"] = stubReply{Code: http.StatusOK, Body: `{"ok":true}`}
	requests := []string{}
	client := &http.Client{Transport: connectionTransport(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.Host+r.URL.Path)
		var data any
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			authURL := ""
			switch r.URL.Host {
			case "copilot.tencent.com":
				authURL = "https://copilot.tencent.com/synthetic-auth"
			case "www.workbuddy.ai":
				authURL = "https://www.workbuddy.ai/synthetic-auth"
			default:
				t.Fatalf("unexpected realm host: %s", r.URL.Host)
			}
			data = map[string]any{"state": "synthetic-state", "authUrl": authURL}
		case "/v2/plugin/auth/token":
			if r.URL.Host != "copilot.tencent.com" {
				t.Fatalf("CN token request used unexpected host: %s", r.URL.Host)
			}
			data = map[string]any{"accessToken": "synthetic-access", "refreshToken": "synthetic-refresh", "expiresIn": 3600, "domain": "copilot.tencent.com"}
		case "/v2/plugin/login/account":
			if r.URL.Host != "copilot.tencent.com" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
				t.Fatal("account identity request was not sent to authenticated CN endpoint")
			}
			data = map[string]any{"uid": "123456", "nickname": "Synthetic account"}
		default:
			t.Fatalf("unexpected authorization request: %s %s", r.Method, r.URL.Host+r.URL.Path)
		}
		b, _ := json.Marshal(map[string]any{"code": 0, "data": data})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
	})}
	d := &legacyConnection{s: h.server, provider: "workbuddy", client: client}
	start, err := d.Start(context.Background(), authflow.Request{Provider: "workbuddy", Method: "qr", Realm: "cn"})
	if err != nil || start.Status != "waiting_scan" || start.ActionURL != "https://copilot.tencent.com/synthetic-auth" {
		t.Fatal("CN WorkBuddy authorization start failed")
	}
	global, err := d.Start(context.Background(), authflow.Request{Provider: "workbuddy", Method: "qr", Realm: "global"})
	if err != nil || global.Status != "waiting_scan" || global.ActionURL != "https://www.workbuddy.ai/synthetic-auth" {
		t.Fatal("Global WorkBuddy authorization start failed")
	}
	request := authflow.Request{Provider: "workbuddy", Method: "qr", Realm: "cn"}
	result, err := d.Step(context.Background(), authflow.Flow{View: authflow.View{Request: request}, Secret: start.Secret}, "poll", authflow.Input{})
	if err != nil || result.Status != "verified" || len(requests) != 4 {
		t.Fatal("official identity verification incomplete")
	}
	committed, err := d.Step(context.Background(), authflow.Flow{View: authflow.View{Request: request}, Secret: result.Secret}, "commit", authflow.Input{})
	if err != nil || committed.Status != "succeeded" || committed.AccountID != "123456" {
		t.Fatal("verified WorkBuddy account was not committed")
	}
	if h.transport.count("POST /runtime/reload") != 1 {
		t.Fatal("committed WorkBuddy account did not trigger exactly one runtime reload")
	}
	raw, err := os.ReadFile(filepath.Join(h.server.opts.AuthDir, "workbuddy-123456.json"))
	if err != nil {
		t.Fatal("committed WorkBuddy auth document missing")
	}
	var saved struct {
		Account map[string]any `json:"account"`
		Auth    map[string]any `json:"auth"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil || jsonString(saved.Account["uid"]) != "123456" || jsonString(saved.Account["nickname"]) != "Synthetic account" || jsonString(saved.Auth["realm"]) != "cn" || jsonString(saved.Auth["domain"]) != "copilot.tencent.com" || jsonInt64(saved.Auth["expiresAt"]) <= time.Now().Unix() {
		t.Fatal("committed WorkBuddy auth document does not preserve verified identity and session")
	}
}
func TestConnectionTraeRejectsForeignCallbackBeforeExchange(t *testing.T) {
	for _, raw := range []string{"https://foreign.invalid/authorize?refreshToken=synthetic", "http://127.0.0.1:8765/wrong?refreshToken=synthetic", "http://foreign@127.0.0.1:8765/authorize?refreshToken=synthetic"} {
		if _, err := connectionTraeCallback(context.Background(), raw); err == nil {
			t.Fatal("foreign callback accepted")
		}
	}
}
