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
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b))), Header: http.Header{"Set-Cookie": {"fixture_session=synthetic; Path=/; Secure; HttpOnly"}}, Request: r}
}
func bootstrapFixture(context.Context) (StoredProfile, error) {
	return StoredProfile{Query: url.Values{"aid": {"synthetic-aid"}, "device_platform": {"web"}}, Headers: http.Header{"User-Agent": {"synthetic-agent"}}}, nil
}
func TestPassportSMSAuthenticatesBeforeSavingAndResumesCookies(t *testing.T) {
	requests := []string{}
	saved := false
	p := &Passport{Bootstrap: bootstrapFixture, Save: func(c context.Context, id string, profile StoredProfile) error {
		if !strings.HasPrefix(id, "doubao-") || profile.Headers.Get("Cookie") == "" {
			t.Error("verified session not carried to encrypted store")
		}
		saved = true
		return nil
	}}
	p.Transport = passportTransport(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.Path)
		if r.URL.Host != "www.doubao.com" {
			t.Fatal("unexpected host")
		}
		if r.Method == "POST" {
			raw, _ := io.ReadAll(r.Body)
			body, _ := url.ParseQuery(string(raw))
			if body.Get("mobile") != "2e342537353730303035343535" {
				t.Fatal("official mobile transformation differs")
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
		}
		return nil, fmt.Errorf("unexpected_endpoint")
	})
	f, err := p.Start(context.Background(), authflow.Request{Method: "sms"})
	if err != nil || f.Status != "waiting_phone" {
		t.Fatal("sms start failed")
	}
	f, err = p.Step(context.Background(), authflow.Flow{Secret: f.Secret}, "send_code", authflow.Input{Phone: "+1 2025550100"})
	if err != nil || f.Status != "waiting_code" || f.RetryAfter != 120*time.Second {
		t.Fatal("send cooldown missing")
	}
	f, err = p.Step(context.Background(), authflow.Flow{Secret: f.Secret}, "submit_code", authflow.Input{Code: "000000"})
	if err != nil || f.Status != "verified" || saved {
		t.Fatal("not verified before save")
	}
	f, err = p.Step(context.Background(), authflow.Flow{Secret: f.Secret}, "commit", authflow.Input{})
	if err != nil || f.Status != "succeeded" || !saved || len(requests) != 3 {
		t.Fatal("commit contract failed")
	}
}
func TestPassportQRScanIsNotSuccessAndExpiryDoesNotRefresh(t *testing.T) {
	status := "scanned"
	calls := 0
	p := &Passport{Bootstrap: bootstrapFixture, Transport: passportTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		switch r.URL.Path {
		case "/passport/web/get_qrcode/":
			return passportReply(r, map[string]any{"token": "synthetic-qr", "qrcode_index_url": "https://www.doubao.com/fixture-qr"}), nil
		case "/passport/web/check_qrconnect/":
			return passportReply(r, map[string]any{"status": status}), nil
		default:
			t.Fatal("scan must not verify without confirmation")
		}
		return nil, nil
	})}
	f, err := p.Start(context.Background(), authflow.Request{Method: "qr"})
	if err != nil || f.Status != "waiting_scan" {
		t.Fatal("qr start failed")
	}
	r, err := p.Step(context.Background(), authflow.Flow{View: authflow.View{Request: authflow.Request{Method: "qr"}, QRURL: f.QRURL}, Secret: f.Secret}, "poll", authflow.Input{})
	if err != nil || r.Status != "scanned" {
		t.Fatal("scanned became success")
	}
	status = "expired"
	r, err = p.Step(context.Background(), authflow.Flow{View: authflow.View{Request: authflow.Request{Method: "qr"}}, Secret: r.Secret}, "poll", authflow.Input{})
	if err != nil || r.Status != "expired" || calls != 3 {
		t.Fatal("expired QR retried automatically")
	}
}
func TestPassportRefusesUnverifiedIdentityAndManualChallenge(t *testing.T) {
	p := &Passport{Bootstrap: bootstrapFixture, Transport: passportTransport(func(r *http.Request) (*http.Response, error) {
		return passportReply(r, map[string]any{"error_code": 9999, "description": "synthetic-private-message"}), nil
	})}
	r, err := p.Start(context.Background(), authflow.Request{Method: "qr"})
	if err != nil || r.Status != "manual_action" || strings.Contains(r.Notice, "private-message") {
		t.Fatal("challenge not safely projected")
	}
	p.Transport = passportTransport(func(r *http.Request) (*http.Response, error) { return passportReply(r, map[string]any{}), nil })
	r, err = p.verify(context.Background(), &passportState{Profile: StoredProfile{Headers: http.Header{}}})
	if err != nil || r.Status != "manual_action" {
		t.Fatal("empty account info accepted")
	}
}
