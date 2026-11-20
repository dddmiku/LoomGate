package autoclaw

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
)

type sendCodeFixture struct {
	body   string
	err    error
	calls  int
	device string
	wait   bool
}

func (f *sendCodeFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls++
	if r.URL.Path != sendCodePath {
		return nil, errors.New("unexpected synthetic login path")
	}
	var body struct {
		Device string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, errors.New("synthetic request unreadable")
	}
	f.device = body.Device
	if f.wait {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(f.body)), Request: r}, nil
}

func realLoginService(t *testing.T, transport http.RoundTripper, timeout ...time.Duration) (*authflow.Service, *accountvault.Vault) {
	t.Helper()
	vault, err := accountvault.Open(t.TempDir())
	if err != nil {
		t.Fatal("synthetic flow store unavailable")
	}
	t.Cleanup(func() { vault.Close() })
	driver := &LoginDriver{HTTP: &http.Client{Transport: transport}, InviteCode: "-"}
	operationTimeout := time.Duration(0)
	if len(timeout) > 0 {
		operationTimeout = timeout[0]
	}
	svc, err := authflow.Open(authflow.Options{Vault: vault,
		Drivers: map[string]authflow.Driver{"autoclaw": driver},
		Timeout: operationTimeout,
		Now:     func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal("synthetic flow service unavailable")
	}
	return svc, vault
}

func loginRoute(t *testing.T, svc *authflow.Service, path string, body any) authflow.View {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal("synthetic input encoding failed")
	}
	w := httptest.NewRecorder()
	if !svc.Route(w, httptest.NewRequest(http.MethodPost, "/api/connections/"+path, strings.NewReader(string(raw)))) || w.Code != http.StatusOK {
		t.Fatalf("login route %s returned HTTP %d", path, w.Code)
	}
	var response struct {
		Flow authflow.View `json:"flow"`
	}
	if json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal("synthetic flow response unreadable")
	}
	return response.Flow
}

func privateLoginState(t *testing.T, vault *accountvault.Vault, id string) loginState {
	t.Helper()
	_, raw, err := vault.Get("authentication-flows")
	if err != nil {
		t.Fatal("synthetic flow record missing")
	}
	var rows []authflow.Flow
	if json.Unmarshal(raw, &rows) != nil {
		t.Fatal("synthetic flow record unreadable")
	}
	for _, row := range rows {
		if row.ID == id {
			var state loginState
			if json.Unmarshal(row.Secret, &state) != nil {
				t.Fatal("private login state was lost")
			}
			return state
		}
	}
	t.Fatal("synthetic flow not found")
	return loginState{}
}

func TestRealLoginServiceSendCodeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, response, status, code string
		err                          error
	}{
		{name: "success", response: `{"code":0}`, status: "waiting_code"},
		{name: "slider", response: `{"code":630014}`, status: "manual_action", code: "slider_required"},
		{name: "rejected", response: `{"code":42001,"msg":"synthetic rejection"}`, status: "failed", code: "code_send_failed"},
		{name: "unreadable", response: `unreadable`, status: "failed", code: "code_send_failed"},
		{name: "transport_failure", err: errors.New("synthetic transport failure"), status: "failed", code: "code_send_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &sendCodeFixture{body: tc.response, err: tc.err}
			svc, vault := realLoginService(t, transport)
			flow := loginRoute(t, svc, "start", authflow.Request{Provider: "autoclaw", Method: "sms"})
			before := privateLoginState(t, vault, flow.ID)
			if flow.Status != "waiting_phone" || before.DeviceID == "" || transport.calls != 0 {
				t.Fatal("start must persist a private device and wait without sending SMS")
			}
			flow = loginRoute(t, svc, "step", map[string]any{"id": flow.ID, "action": "send_code",
				"input": map[string]any{"phone": "1" + strings.Repeat("0", 10), "invite_code": ""}})
			if flow.Status != tc.status || flow.ErrorCode != tc.code || transport.calls != 1 {
				t.Fatalf("unexpected send outcome: status=%s code=%s calls=%d", flow.Status, flow.ErrorCode, transport.calls)
			}
			if transport.device != before.DeviceID {
				t.Fatal("send request lost the private per-flow device")
			}
			if flow.Status == "waiting_code" {
				after := privateLoginState(t, vault, flow.ID)
				if after.DeviceID != before.DeviceID || after.Stage != "code_requested" {
					t.Fatal("successful send lost the private continuation")
				}
			}
		})
	}
}

func TestRealLoginServiceValidationCanBeCorrectedWithoutCooldown(t *testing.T) {
	for _, tc := range []struct{ name, phone, invite string }{
		{name: "invalid_phone", phone: "invalid"},
		{name: "invalid_invite", phone: "1" + strings.Repeat("0", 10), invite: "bad invite!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &sendCodeFixture{body: `{"code":0}`}
			svc, vault := realLoginService(t, transport)
			flow := loginRoute(t, svc, "start", authflow.Request{Provider: "autoclaw", Method: "sms"})
			before := privateLoginState(t, vault, flow.ID)
			flow = loginRoute(t, svc, "step", map[string]any{"id": flow.ID, "action": "send_code",
				"input": map[string]any{"phone": tc.phone, "invite_code": tc.invite}})
			after := privateLoginState(t, vault, flow.ID)
			if flow.Status != "waiting_phone" || after.DeviceID != before.DeviceID || transport.calls != 0 {
				t.Fatal("invalid input must retain the initial state without sending SMS")
			}
			flow = loginRoute(t, svc, "step", map[string]any{"id": flow.ID, "action": "send_code",
				"input": map[string]any{"phone": "1" + strings.Repeat("0", 10), "invite_code": ""}})
			if flow.Status != "waiting_code" || transport.calls != 1 {
				t.Fatal("corrected input was blocked although no SMS had been sent")
			}
			if _, err := svc.Step(t.Context(), flow.ID, "send_code", authflow.Input{Phone: "1" + strings.Repeat("0", 10)}); err != authflow.ErrBusy || transport.calls != 1 {
				t.Fatal("a real send must still activate the cooldown and block duplicate dispatch")
			}
		})
	}
}

func TestRealLoginServiceCooldownIsScopedPerPhone(t *testing.T) {
	transport := &sendCodeFixture{body: `{"code":42001,"msg":"synthetic rejection"}`}
	svc, _ := realLoginService(t, transport)
	phoneA := "1" + strings.Repeat("0", 10)
	phoneB := "1" + strings.Repeat("1", 10)
	first := loginRoute(t, svc, "start", authflow.Request{Provider: "autoclaw", Method: "sms"})
	first = loginRoute(t, svc, "step", map[string]any{"id": first.ID, "action": "send_code",
		"input": map[string]any{"phone": phoneA, "invite_code": ""}})
	if first.Status != "failed" || transport.calls != 1 {
		t.Fatalf("first send did not record its cooldown: status=%s calls=%d", first.Status, transport.calls)
	}
	second := loginRoute(t, svc, "start", authflow.Request{Provider: "autoclaw", Method: "sms"})
	second = loginRoute(t, svc, "step", map[string]any{"id": second.ID, "action": "send_code",
		"input": map[string]any{"phone": phoneB, "invite_code": ""}})
	if second.Status != "failed" || transport.calls != 2 {
		t.Fatalf("different phone was incorrectly blocked by first cooldown: status=%s calls=%d", second.Status, transport.calls)
	}
	if _, err := svc.Step(t.Context(), second.ID, "cancel", authflow.Input{}); err != nil {
		t.Fatalf("cancel second flow: %v", err)
	}
	third, err := svc.Start(t.Context(), authflow.Request{Provider: "autoclaw", Method: "sms"})
	if err != nil {
		t.Fatalf("start same-phone retry flow: %v", err)
	}
	if _, err = svc.Step(t.Context(), third.ID, "send_code", authflow.Input{Phone: phoneA}); err != authflow.ErrBusy {
		t.Fatalf("same phone bypassed its cooldown: %v", err)
	}
}

func TestRealLoginServiceTimedOutSendIsUnconfirmed(t *testing.T) {
	transport := &sendCodeFixture{wait: true}
	svc, _ := realLoginService(t, transport, 20*time.Millisecond)
	flow := loginRoute(t, svc, "start", authflow.Request{Provider: "autoclaw", Method: "sms"})
	flow = loginRoute(t, svc, "step", map[string]any{"id": flow.ID, "action": "send_code",
		"input": map[string]any{"phone": "1" + strings.Repeat("0", 10), "invite_code": ""}})
	if flow.Status != "uncertain" || flow.ErrorCode != "operation_unconfirmed" || transport.calls != 1 {
		t.Fatalf("timeout must retain an unconfirmed result: status=%s code=%s calls=%d", flow.Status, flow.ErrorCode, transport.calls)
	}
}
