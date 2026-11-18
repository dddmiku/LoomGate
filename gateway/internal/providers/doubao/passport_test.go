package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"workbuddy2api/internal/authflow"
)

type passportTransport func(*http.Request) (*http.Response, error)

func (f passportTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func passportReply(r *http.Request, data any) *http.Response {
	b, _ := json.Marshal(map[string]any{"message": "success", "data": data})
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(string(b))),
		Header:     http.Header{"Set-Cookie": {"fixture_session=synthetic; Domain=.doubao.com; Path=/; Secure; HttpOnly"}},
		Request:    r,
	}
}

func TestPassportSMSAuthenticatesBeforeSavingAndResumesCookies(t *testing.T) {
	requests := []string{}
	saved := false
	p := &Passport{Save: func(c context.Context, id string, profile StoredProfile) error {
		if !strings.HasPrefix(id, "doubao-") || profile.Headers.Get("Cookie") == "" {
			t.Error("verified session not carried to encrypted store")
		}
		saved = true
		return nil
	}}
	p.Transport = passportTransport(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.Path)
		// accounts.doubao.com serves the passport calls; www.doubao.com is the
		// chat host used once to discover the account's default bot id.
		if r.URL.Host != "accounts.doubao.com" && r.URL.Host != "www.doubao.com" {
			t.Fatalf("unexpected host %q", r.URL.Host)
		}
		// Only the passport host rejects real_aid (error 22); the chat host needs
		// it, so the assertion is scoped to accounts.doubao.com.
		if r.URL.Host == "accounts.doubao.com" && r.URL.Query().Get("real_aid") != "" {
			t.Fatal("real_aid leaked into a passport request")
		}
		if r.Method == "POST" && r.URL.Host == "accounts.doubao.com" {
			raw, _ := io.ReadAll(r.Body)
			body, _ := url.ParseQuery(string(raw))
			if body.Get("mobile") != "" && body.Get("mobile") != "2e342537353730303035343535" {
				t.Fatalf("official mobile transformation differs: %q", body.Get("mobile"))
			}
			if strings.Contains(string(raw), "2025550100") {
				t.Fatal("plaintext phone in encoded request")
			}
		}
		switch r.URL.Path {
		case "/passport/web/send_code/":
			return passportReply(r, map[string]any{"error_code": 0, "retry_time": 120}), nil
		case "/passport/web/sms_login/":
			if r.Header.Get("Cookie") == "" {
				t.Fatal("flow cookie not resumed")
			}
			return passportReply(r, map[string]any{"error_code": 0}), nil
		case "/passport/account/info/v2/":
			return passportReply(r, map[string]any{"user_id_str": "synthetic-user"}), nil
		case "/alice/slot/action_bar_v3/brief_list":
			// Confirming the client's default assistant id: code 0 means the id is
			// valid for this account.
			return &http.Response{StatusCode: 200, Request: r,
				Body:   io.NopCloser(strings.NewReader(`{"code":0,"data":{"entries":[]}}`)),
				Header: http.Header{"Content-Type": {"application/json"}}}, nil
		case "/im/chain/recent_conv":
			return &http.Response{StatusCode: 200, Request: r,
				Body:   io.NopCloser(strings.NewReader(`{"status_code":0,"status_desc":"ok","downlink_body":{"pull_recent_conv_chain_downlink_body":{"cells":[],"has_more":false,"extra":{"coco_bot_id":"1234567890123456789","coco_bot_name":"fixture"}}}}`)),
				Header: http.Header{"Content-Type": {"application/json"}}}, nil
		}
		return nil, fmt.Errorf("unexpected_endpoint %s", r.URL.Path)
	})
	f, err := p.Start(context.Background(), authflow.Request{Method: "sms"})
	if err != nil || f.Status != "waiting_phone" {
		t.Fatal("sms start failed")
	}
	f, err = p.Step(context.Background(), authflow.Flow{Secret: f.Secret}, "send_code", authflow.Input{Phone: "+1 2025550100"})
	if err != nil || f.Status != "waiting_code" || f.RetryAfter != 120*time.Second {
		t.Fatalf("send cooldown missing: %v %v", f.Status, err)
	}
	f, err = p.Step(context.Background(), authflow.Flow{Secret: f.Secret}, "submit_code", authflow.Input{Code: "000000"})
	if err != nil || f.Status != "verified" || saved {
		t.Fatalf("not verified before save: %v %v", f.Status, err)
	}
	f, err = p.Step(context.Background(), authflow.Flow{Secret: f.Secret}, "commit", authflow.Input{})
	if err != nil || f.Status != "succeeded" || !saved {
		t.Fatalf("commit contract failed: %v %v saved=%v requests=%d", f.Status, err, saved, len(requests))
	}
	// 3 passport calls + 1 brief_list confirmation of the client's default bot id
	// (that call returns code 0, so the fallback feed is not consulted).
	if len(requests) != 4 {
		t.Fatalf("expected 4 requests, got %d: %v", len(requests), requests)
	}
}

func TestPassportQRScanIsNotSuccessAndExpiryDoesNotRefresh(t *testing.T) {
	status := "scanned"
	calls := 0
	p := &Passport{Transport: passportTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		switch r.URL.Path {
		case "/passport/web/get_qrcode/":
			return passportReply(r, map[string]any{"token": "synthetic-qr", "qrcode_index_url": "https://www.doubao.com/fixture-qr"}), nil
		case "/passport/web/check_qrconnect/":
			return passportReply(r, map[string]any{"status": status}), nil
		default:
			t.Fatalf("scan must not verify without confirmation: %s", r.URL.Path)
		}
		return nil, nil
	})}
	f, err := p.Start(context.Background(), authflow.Request{Method: "qr"})
	if err != nil || f.Status != "waiting_scan" {
		t.Fatalf("qr start failed: %v %v", f.Status, err)
	}
	r, err := p.Step(context.Background(), authflow.Flow{View: authflow.View{Request: authflow.Request{Method: "qr"}, QRURL: f.QRURL}, Secret: f.Secret}, "poll", authflow.Input{})
	if err != nil || r.Status != "scanned" {
		t.Fatalf("scanned became success: %v %v", r.Status, err)
	}
	status = "expired"
	r, err = p.Step(context.Background(), authflow.Flow{View: authflow.View{Request: authflow.Request{Method: "qr"}}, Secret: r.Secret}, "poll", authflow.Input{})
	if err != nil || r.Status != "expired" || calls != 3 {
		t.Fatalf("expired QR retried automatically: %v calls=%d", r.Status, calls)
	}
}

func TestPassportRefusesUnverifiedIdentityAndManualChallenge(t *testing.T) {
	p := &Passport{Transport: passportTransport(func(r *http.Request) (*http.Response, error) {
		return passportReply(r, map[string]any{"error_code": 9999, "description": "synthetic-private-message"}), nil
	})}
	r, err := p.Start(context.Background(), authflow.Request{Method: "qr"})
	if err != nil || r.Status != "manual_action" || strings.Contains(r.Notice, "private-message") {
		t.Fatalf("challenge not safely projected: %v %v %q", r.Status, err, r.Notice)
	}
	if r.ErrorCode != "official_request_rejected" || r.UpstreamCode == nil || *r.UpstreamCode != 9999 {
		t.Fatal("business code lost or misclassified as verified risk control")
	}
	p.Transport = passportTransport(func(r *http.Request) (*http.Response, error) { return passportReply(r, map[string]any{}), nil })
	r, err = p.verify(context.Background(), &passportState{Profile: StoredProfile{Headers: http.Header{}}})
	if err != nil || r.Status != "manual_action" {
		t.Fatal("empty account info accepted")
	}
}

// TestPassportMethodsAreAvailableWithoutAClient proves the gateway login no
// longer depends on a captured SDK context or a running desktop client.
func TestPassportMethodsAreAvailableWithoutAClient(t *testing.T) {
	p := &Passport{}
	for _, m := range p.Methods() {
		if !m.Available || m.Reason != "" {
			t.Fatalf("method %s unexpectedly unavailable: %+v", m.ID, m)
		}
	}
}

// TestPassportCommitRejectsAuthOnlyCookie keeps the domain-scope contract: a
// cookie that only applies to the passport host is not a chat session.
func TestPassportCommitRejectsAuthOnlyCookie(t *testing.T) {
	p := &Passport{Save: func(context.Context, string, StoredProfile) error {
		t.Fatal("auth-only cookie imported into work session")
		return nil
	}}
	st := passportState{
		Profile: StoredProfile{Query: deviceQuery(), Headers: http.Header{}},
		UserID:  "synthetic-user",
		Cookies: []*http.Cookie{{Name: "accounts_only", Value: "synthetic", Domain: "accounts.doubao.com", Path: "/", Secure: true}},
	}
	r, err := p.Step(context.Background(), authflow.Flow{Secret: passportSecret(st)}, "commit", authflow.Input{})
	if err != nil || r.Status != "manual_action" || r.ErrorCode != "work_session_not_verified" {
		t.Fatalf("cookie domain scope expanded: %v %v %q", r.Status, err, r.ErrorCode)
	}
}

// TestPassportCommitStoresChatCookie proves a real chat-scoped cookie is sealed
// with the device context and no captured request body.
func TestPassportCommitStoresChatCookie(t *testing.T) {
	var got StoredProfile
	p := &Passport{Transport: passportTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Request: r,
			Body:   io.NopCloser(strings.NewReader(`{"code":0,"data":{"samantha_bot":{"bot_id":"1234567890123456789"}}}`)),
			Header: http.Header{"Content-Type": {"application/json"}}}, nil
	}), Save: func(_ context.Context, id string, profile StoredProfile) error {
		if !strings.HasPrefix(id, "doubao-") {
			t.Fatalf("unexpected account id %q", id)
		}
		got = profile
		return nil
	}}
	st := passportState{
		Profile: StoredProfile{Query: deviceQuery(), Headers: http.Header{}},
		UserID:  "synthetic-user",
		Cookies: []*http.Cookie{{Name: "sessionid", Value: "synthetic", Domain: ".doubao.com", Path: "/", Secure: true}},
	}
	r, err := p.Step(context.Background(), authflow.Flow{Secret: passportSecret(st)}, "commit", authflow.Input{})
	if err != nil || r.Status != "succeeded" {
		t.Fatalf("commit failed: %v %v", r.Status, err)
	}
	if !strings.Contains(got.Headers.Get("Cookie"), "sessionid=") {
		t.Fatalf("chat cookie missing: %q", got.Headers.Get("Cookie"))
	}
	if got.BindingFlow != "gateway-passport" {
		t.Fatalf("binding flow = %q", got.BindingFlow)
	}
	if len(got.CompletionTemplate) != 0 {
		t.Fatal("capture template must not be injected by the login flow")
	}
	if got.Query.Get("device_platform") == "" || got.Query.Get("aid") == "" {
		t.Fatal("device context not stored")
	}
}

// TestPassportRequestsStripRealAid is the regression for the live error 22: the
// passport host rejects requests carrying real_aid, while the chat host needs it.
func TestPassportRequestsStripRealAid(t *testing.T) {
	var seen []url.Values
	p := &Passport{Transport: passportTransport(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.URL.Query())
		switch r.URL.Path {
		case "/passport/web/get_qrcode/":
			return passportReply(r, map[string]any{"token": "t", "qrcode_index_url": "https://www.doubao.com/q"}), nil
		default:
			return passportReply(r, map[string]any{"status": "new"}), nil
		}
	})}
	if _, err := p.Start(context.Background(), authflow.Request{Method: "qr"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Get("real_aid") != "" {
		t.Fatalf("real_aid not stripped: %v", seen)
	}
}

// TestPassportCommitRequiresBotIdentity is the regression for the hard blocker:
// catalog and completion both need the account's own bot id. If the service
// rejects the client's default assistant id and the feed carries no Coco bot,
// the login must NOT be stored as a usable account, because it would fail every
// model request afterwards.
func TestPassportCommitRequiresBotIdentity(t *testing.T) {
	saved := false
	p := &Passport{
		Transport: passportTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/alice/slot/action_bar_v3/brief_list" {
				// A candidate id the account cannot use.
				return &http.Response{StatusCode: 200, Request: r,
					Body:   io.NopCloser(strings.NewReader(`{"code":710010202,"data":{}}`)),
					Header: http.Header{"Content-Type": {"application/json"}}}, nil
			}
			// The feed answers but carries no bot id for this account.
			return &http.Response{StatusCode: 200, Request: r,
				Body:   io.NopCloser(strings.NewReader(`{"code":0,"data":{}}`)),
				Header: http.Header{"Content-Type": {"application/json"}}}, nil
		}),
		Save: func(context.Context, string, StoredProfile) error {
			saved = true
			return nil
		},
	}
	st := passportState{
		Profile: StoredProfile{Query: deviceQuery(), Headers: http.Header{}},
		UserID:  "synthetic-user",
		Cookies: []*http.Cookie{{Name: "sessionid", Value: "synthetic", Domain: ".doubao.com", Path: "/", Secure: true}},
	}
	r, err := p.Step(context.Background(), authflow.Flow{Secret: passportSecret(st)}, "commit", authflow.Input{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != "manual_action" || r.ErrorCode != "bot_identity_unavailable" {
		t.Fatalf("missing bot id was not surfaced: %+v", r)
	}
	if saved {
		t.Fatal("an unusable account was stored")
	}
}

// TestFirstBotID walks nested shapes and rejects non-numeric values.
func TestFirstBotID(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"direct", map[string]any{"bot_id": "1234567890123456789"}, "1234567890123456789"},
		{"nested samantha_bot", map[string]any{"samantha_bot": map[string]any{"bot_id": "9876543210987654321"}}, "9876543210987654321"},
		{"camel", map[string]any{"samanthaBotInfo": map[string]any{"botId": "1111111111111111111"}}, "1111111111111111111"},
		{"in list", map[string]any{"entry_list": []any{map[string]any{"bot_id": "2222222222222222222"}}}, "2222222222222222222"},
		{"too short", map[string]any{"bot_id": "12345"}, ""},
		{"non numeric", map[string]any{"bot_id": "abcdefghijklmnop"}, ""},
		{"absent", map[string]any{"other": "x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstBotID(tc.value); got != tc.want {
				t.Fatalf("firstBotID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPassportStoresMaskedMobileForDisplay proves the account list can identify a
// Doubao login without exposing a full phone number: the service masks the
// number before returning it, and that masked value is what gets stored.
func TestPassportStoresMaskedMobileForDisplay(t *testing.T) {
	const masked = "138****8000"
	var saved StoredProfile
	p := &Passport{
		Transport: passportTransport(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/passport/account/info/v2/":
				return passportReply(r, map[string]any{"user_id_str": "synthetic-user", "mobile": masked, "name": "合成昵称"}), nil
			case "/alice/slot/action_bar_v3/brief_list":
				return &http.Response{StatusCode: 200, Request: r,
					Body:   io.NopCloser(strings.NewReader(`{"code":0,"data":{}}`)),
					Header: http.Header{"Content-Type": {"application/json"}}}, nil
			}
			return nil, fmt.Errorf("unexpected_endpoint %s", r.URL.Path)
		}),
		Save: func(_ context.Context, _ string, profile StoredProfile) error {
			saved = profile
			return nil
		},
	}
	st := passportState{
		Profile: StoredProfile{Query: deviceQuery(), Headers: http.Header{}},
		UserID:  "synthetic-user",
		Cookies: []*http.Cookie{{Name: "sessionid", Value: "synthetic", Domain: ".doubao.com", Path: "/", Secure: true}},
	}
	// verify reads account-info and captures the display facts; commit then sees
	// them through the encrypted flow secret, exactly as in a real login.
	verified, err := p.Step(context.Background(), authflow.Flow{Secret: passportSecret(st)}, "resume", authflow.Input{})
	if err != nil || verified.Status != "verified" {
		t.Fatalf("verify: %v %v", verified.Status, err)
	}
	r, err := p.Step(context.Background(), authflow.Flow{Secret: verified.Secret}, "commit", authflow.Input{})
	if err != nil || r.Status != "succeeded" {
		t.Fatalf("commit: %v %v", r.Status, err)
	}
	if saved.Mobile != masked {
		t.Fatalf("mobile = %q, want the upstream-masked value %q", saved.Mobile, masked)
	}
	if saved.DisplayName != "合成昵称" {
		t.Fatalf("display name = %q", saved.DisplayName)
	}
}
