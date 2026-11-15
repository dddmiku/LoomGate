package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
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
func TestConnectionWorkBuddyProtocolStoresVerifiedResultWithoutHelperFiles(t *testing.T) {
	requests := []string{}
	client := &http.Client{Transport: connectionTransport(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.Path)
		var data any
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			data = map[string]any{"state": "synthetic-state", "authUrl": "https://www.codebuddy.cn/synthetic-auth"}
		case "/v2/plugin/auth/token":
			data = map[string]any{"accessToken": "synthetic-access", "refreshToken": "synthetic-refresh", "expiresIn": 3600, "domain": "copilot.tencent.com"}
		case "/v2/plugin/login/account":
			if r.Header.Get("Authorization") != "Bearer synthetic-access" {
				t.Fatal("account identity was not authenticated")
			}
			data = map[string]any{"uid": "123456", "nickname": "Synthetic account"}
		default:
			t.Fatal("unexpected authorization request")
		}
		b, _ := json.Marshal(map[string]any{"code": 0, "data": data})
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
	})}
	d := &legacyConnection{provider: "workbuddy", client: client}
	start, err := d.Start(context.Background(), authflow.Request{Provider: "workbuddy", Method: "qr", Realm: "cn"})
	if err != nil || start.Status != "waiting_scan" {
		t.Fatal("WorkBuddy authorization start failed")
	}
	result, err := d.Step(context.Background(), authflow.Flow{View: authflow.View{Request: authflow.Request{Provider: "workbuddy", Realm: "cn"}}, Secret: start.Secret}, "poll", authflow.Input{})
	if err != nil || result.Status != "verified" || len(requests) != 3 {
		t.Fatal("official identity verification incomplete")
	}
}
func TestConnectionTraeRejectsForeignCallbackBeforeExchange(t *testing.T) {
	for _, raw := range []string{"https://foreign.invalid/authorize?refreshToken=synthetic", "http://127.0.0.1:8765/wrong?refreshToken=synthetic", "http://foreign@127.0.0.1:8765/authorize?refreshToken=synthetic"} {
		if _, err := connectionTraeCallback(context.Background(), raw); err == nil {
			t.Fatal("foreign callback accepted")
		}
	}
}
