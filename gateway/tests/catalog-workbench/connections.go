package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"workbuddy2api/internal/authflow"
	"workbuddy2api/internal/providers/doubao"
)

func syntheticPassportBootstrap(context.Context) (doubao.StoredProfile, error) {
	return doubao.StoredProfile{Query: url.Values{"aid": {"synthetic-aid"}, "device_platform": {"web"}}, Headers: http.Header{"User-Agent": {"synthetic-client"}}}, nil
}

type syntheticPassportTransport struct{ fixture *fixtures }

func (s syntheticPassportTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "accounts.doubao.com" {
		return nil, errors.New("fixture_external_request_blocked")
	}
	data := map[string]any{"error_code": 0}
	switch r.URL.Path {
	case "/passport/web/get_qrcode/":
		s.fixture.qrCalls.Add(1)
		s.fixture.qrPolls.Store(0)
		data["token"] = "synthetic-qr-value"
		data["qrcode_index_url"] = "https://www.doubao.com/synthetic-qr"
	case "/passport/web/check_qrconnect/":
		if s.fixture.qrPolls.Add(1) == 1 {
			data["status"] = "scanned"
		} else {
			data["status"] = "confirmed"
		}
		if s.fixture.mode.Load() == 5 {
			data["status"] = "expired"
		}
	case "/passport/web/send_code/":
		s.fixture.smsCalls.Add(1)
		data["retry_time"] = 90
	case "/passport/web/sms_login/":
		data["ok"] = true
	case "/passport/account/info/v2/":
		data["user_id_str"] = "synthetic-browser-user"
	default:
		return nil, errors.New("fixture_unexpected_passport_endpoint")
	}
	if s.fixture.mode.Load() == 6 {
		data = map[string]any{"error_code": 9999, "description": "synthetic-private-rejection"}
	}
	b, _ := json.Marshal(map[string]any{"message": "success", "data": data})
	return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": {"fixture_session=synthetic; Domain=.doubao.com; Path=/; Secure; HttpOnly"}}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
}

type syntheticConnection struct{}

type syntheticDesktop struct{ fixture *fixtures }

type syntheticManaged struct{ fixture *fixtures }

func (d *syntheticManaged) Endpoint(context.Context, bool) (int, error) { return 19227, nil }
func (d *syntheticManaged) Login(_ context.Context, action, nonce, method string) (string, string, error) {
	if action == "login_start" {
		d.fixture.qrCalls.Add(1)
	}
	if action == "login_poll" {
		d.fixture.qrPolls.Add(1)
	}
	if action == "login_cancel" {
		return "canceled", "", nil
	}
	switch d.fixture.mode.Load() {
	case 5:
		return "expired", "", nil
	case 6:
		return "manual_action", "official_verification_required", nil
	case 7:
		return "", "", errors.New("synthetic_connection_unavailable")
	case 9:
		return "waiting_scan", "", nil
	}
	return "verified", "", nil
}

func (d *syntheticDesktop) Bind(context.Context) (doubao.RuntimeBinding, error) {
	switch d.fixture.mode.Load() {
	case 7:
		return doubao.RuntimeBinding{}, &doubao.APIError{Kind: "desktop_unavailable"}
	case 9:
		return doubao.RuntimeBinding{}, &doubao.APIError{Kind: "desktop_login_required"}
	case 8:
		return doubao.RuntimeBinding{Identity: strings.Repeat("b", 64)}, nil
	}
	return doubao.RuntimeBinding{Identity: strings.Repeat("a", 64)}, nil
}
func (d *syntheticDesktop) Connection(ctx context.Context, expected doubao.RuntimeBinding) (doubao.ConnectionProof, error) {
	b, err := d.Bind(ctx)
	if err != nil {
		return doubao.ConnectionProof{}, err
	}
	if expected.Identity != "" && expected.Identity != b.Identity {
		return doubao.ConnectionProof{}, &doubao.APIError{Kind: "desktop_identity_mismatch"}
	}
	return doubao.ConnectionProof{Binding: b, BotID: "synthetic-current-bot", Models: 2}, nil
}

func (*syntheticConnection) Methods() []authflow.Method {
	return []authflow.Method{{ID: "qr", Name: "合成官方授权", Available: true}, {ID: "browser", Name: "合成网页授权", Available: true}}
}
func (*syntheticConnection) Start(ctx context.Context, r authflow.Request) (authflow.Result, error) {
	status := "waiting_scan"
	if r.Provider == "trae" {
		status = "waiting_authorization"
	}
	return authflow.Result{Status: status, ActionURL: "https://www.workbuddy.ai/synthetic", Secret: json.RawMessage(`{"synthetic":true}`)}, nil
}
func (*syntheticConnection) Step(ctx context.Context, f authflow.Flow, a string, i authflow.Input) (authflow.Result, error) {
	if a == "commit" {
		return authflow.Result{Status: "succeeded", AccountID: "synthetic-legacy-account"}, nil
	}
	if a == "callback" || a == "poll" {
		return authflow.Result{Status: "verified", Secret: f.Secret}, nil
	}
	return authflow.Result{}, authflow.ErrInvalid
}
