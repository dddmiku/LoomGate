package autoclaw

// Login must (a) survive the panel reusing the inference vault's exclusive
// lock, and (b) bind the default referral code after a successful login
// without letting a bind failure fail the login.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/authflow"
)

func authflowFlowFor(phone string) authflow.Flow {
	secret, _ := json.Marshal(loginState{Phone: phone, DeviceID: newDeviceID(), Stage: "code_requested"})
	return authflow.Flow{Secret: secret}
}

func authflowInputFor(code string) authflow.Input {
	return authflow.Input{Code: code}
}

func flowWithSecret(secret json.RawMessage) authflow.Flow {
	return authflow.Flow{Secret: secret}
}

// inviteTransport answers the login endpoints and records the fission bind.
type inviteTransport struct {
	bindPath string
	bindBody map[string]any
	bindCode int
}

func (s *inviteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"code":0,"msg":"success"}`
	switch {
	case strings.Contains(r.URL.Path, "agent-login"):
		body = `{"code":0,"data":{"access_token":"synthetic.access.token","refresh_token":"synthetic-refresh","user_id":"u1"}}`
	case strings.Contains(r.URL.Path, "refresh"):
		body = `{"code":0,"data":{"access_token":"swapped.access.token","refresh_token":"swapped-refresh"}}`
	case strings.Contains(r.URL.Path, "fission-v2/bind"):
		s.bindPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &s.bindBody)
		code := s.bindCode
		body = `{"code":` + itoa(code) + `,"msg":"x"}`
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}

// The panel and inference layer share one vault handle. A save must write
// through it, not fail because the directory is already locked.
func TestSaveUsesTheSharedVaultHandle(t *testing.T) {
	tr := &inviteTransport{}
	d := &LoginDriver{
		HTTP:       &http.Client{Transport: tr},
		InviteCode: "-", // disable the referral call for this case
		Save: func(_ context.Context, profile []byte) (string, error) {
			var s stored
			if err := json.Unmarshal(profile, &s); err != nil {
				return "", err
			}
			if s.AccessToken == "" || s.RefreshToken == "" || s.Phone == "" {
				return "", errors.New("profile incomplete")
			}
			return "autoclaw-13800000000", nil
		},
	}
	flow := authflowFlowFor("13800000000")
	verified, err := d.Step(context.Background(), flow, "submit_code", authflowInputFor("123456"))
	if err != nil {
		t.Fatalf("verify step failed: %v", err)
	}
	if verified.Status != "verified" {
		t.Fatalf("status=%q want verified before commit", verified.Status)
	}
	if verified.AccountID != "" {
		t.Fatal("submit_code must not claim an account before commit")
	}
	got, err := d.Step(context.Background(), flowWithSecret(verified.Secret), "commit", authflow.Input{})
	if err != nil {
		t.Fatalf("commit through shared vault failed: %v", err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status=%q want succeeded", got.Status)
	}
	if got.AccountID != "autoclaw-13800000000" {
		t.Fatalf("account id=%q", got.AccountID)
	}
}

// A freshly created account binds the configured referral code by default.
func TestLoginBindsDefaultInviteCode(t *testing.T) {
	tr := &inviteTransport{}
	d := &LoginDriver{
		HTTP: &http.Client{Transport: tr},
		Save: func(context.Context, []byte) (string, error) { return "autoclaw-1", nil },
	}
	verified, err := d.Step(context.Background(), authflowFlowFor("13800000000"), "submit_code", authflowInputFor("123456"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Step(context.Background(), flowWithSecret(verified.Secret), "commit", authflow.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status=%q", got.Status)
	}
	if tr.bindBody == nil {
		t.Fatal("referral bind was not attempted")
	}
	if tr.bindBody["invite_code"] != DefaultInviteCode {
		t.Fatalf("invite_code=%v want %s", tr.bindBody["invite_code"], DefaultInviteCode)
	}
	if tr.bindBody["activity_id"] != fissionActivity {
		t.Fatalf("activity_id=%v", tr.bindBody["activity_id"])
	}
	if !strings.Contains(tr.bindPath, "/agent-assetmgr/api/v1/fission-v2/bind") {
		t.Fatalf("bind hit %q; the referral API lives on the web host", tr.bindPath)
	}
	if !strings.Contains(got.Notice, DefaultInviteCode) {
		t.Fatalf("success notice does not mention the bound code: %q", got.Notice)
	}
}

// A rejected referral code is reported but never loses the account.
func TestInviteBindFailureKeepsTheAccount(t *testing.T) {
	tr := &inviteTransport{bindCode: 42001}
	d := &LoginDriver{
		HTTP: &http.Client{Transport: tr},
		Save: func(context.Context, []byte) (string, error) { return "autoclaw-1", nil },
	}
	verified, err := d.Step(context.Background(), authflowFlowFor("13800000000"), "submit_code", authflowInputFor("123456"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Step(context.Background(), flowWithSecret(verified.Secret), "commit", authflow.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.AccountID != "autoclaw-1" {
		t.Fatalf("a failed bind dropped the account: status=%q id=%q", got.Status, got.AccountID)
	}
	if !strings.Contains(got.Notice, "绑定失败") {
		t.Fatalf("notice hides the bind failure: %q", got.Notice)
	}
}

// "-" opts out of referral binding entirely.
func TestInviteCodeCanBeDisabled(t *testing.T) {
	tr := &inviteTransport{}
	d := &LoginDriver{
		HTTP:       &http.Client{Transport: tr},
		InviteCode: "-",
		Save:       func(context.Context, []byte) (string, error) { return "autoclaw-1", nil },
	}
	verified, err := d.Step(context.Background(), authflowFlowFor("13800000000"), "submit_code", authflowInputFor("123456"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Step(context.Background(), flowWithSecret(verified.Secret), "commit", authflow.Input{}); err != nil {
		t.Fatal(err)
	}
	if tr.bindBody != nil {
		t.Fatal("referral bind ran despite being disabled")
	}
}

// A failed save must still surface as an error so authflow reports
// "结果未确认" instead of pretending the account exists.
func TestSaveFailureIsReported(t *testing.T) {
	tr := &inviteTransport{}
	d := &LoginDriver{
		HTTP:       &http.Client{Transport: tr},
		InviteCode: "-",
		Save:       func(context.Context, []byte) (string, error) { return "", errors.New("vault_busy") },
	}
	verified, err := d.Step(context.Background(), authflowFlowFor("13800000000"), "submit_code", authflowInputFor("123456"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Step(context.Background(), flowWithSecret(verified.Secret), "commit", authflow.Input{}); err == nil {
		t.Fatal("a save failure was swallowed")
	}
}

// The referral code is optional at login. A code the operator types wins; a
// blank field falls back to the provider default, so the built-in code is a
// default rather than the only choice.
func TestOperatorInviteCodeOverridesTheDefault(t *testing.T) {
	tr := &inviteTransport{}
	d := &LoginDriver{HTTP: &http.Client{Transport: tr},
		Save: func(context.Context, []byte) (string, error) { return "autoclaw-1", nil }}

	// The operator types a code with the phone number.
	flow := authflowFlowFor("13800000000")
	verified, err := d.Step(context.Background(), flow, "send_code",
		authflow.Input{Phone: "13800000000", InviteCode: "MYCODE99"})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Status != "waiting_code" {
		t.Fatalf("status=%q", verified.Status)
	}
	// Continue with the secret the driver produced (it carries the code).
	verified, err = d.Step(context.Background(), flowWithSecret(verified.Secret), "submit_code", authflowInputFor("123456"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Step(context.Background(), flowWithSecret(verified.Secret), "commit", authflow.Input{}); err != nil {
		t.Fatal(err)
	}
	if tr.bindBody == nil || tr.bindBody["invite_code"] != "MYCODE99" {
		t.Fatalf("operator code not used: %v", tr.bindBody)
	}
}

// A blank field keeps the provider default.
func TestBlankInviteCodeUsesTheDefault(t *testing.T) {
	tr := &inviteTransport{}
	d := &LoginDriver{HTTP: &http.Client{Transport: tr},
		Save: func(context.Context, []byte) (string, error) { return "autoclaw-1", nil }}
	flow := authflowFlowFor("13800000000")
	verified, err := d.Step(context.Background(), flow, "send_code",
		authflow.Input{Phone: "13800000000"})
	if err != nil {
		t.Fatal(err)
	}
	verified, err = d.Step(context.Background(), flowWithSecret(verified.Secret), "submit_code", authflowInputFor("123456"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Step(context.Background(), flowWithSecret(verified.Secret), "commit", authflow.Input{}); err != nil {
		t.Fatal(err)
	}
	if tr.bindBody == nil || tr.bindBody["invite_code"] != DefaultInviteCode {
		t.Fatalf("default not applied: %v", tr.bindBody)
	}
}

// A malformed code must not silently fall back to the default: the operator
// would believe their code was used.
func TestMalformedInviteCodeIsRejectedNotIgnored(t *testing.T) {
	d := &LoginDriver{HTTP: stubHTTP(t, nil)}
	got, err := d.Step(context.Background(), authflowFlowFor("13800000000"), "send_code",
		authflow.Input{Phone: "13800000000", InviteCode: "bad code!"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "waiting_phone" {
		t.Fatalf("a malformed invite code was accepted: status=%q", got.Status)
	}
	if !strings.Contains(got.Notice, "邀请码") {
		t.Fatalf("notice does not mention the invite code: %q", got.Notice)
	}
}
