package autoclaw

// Drives the real authflow service, not just the driver. The bug this pins:
// submit_code returned status "succeeded" without a commit branch, so the
// service rejected it as authentication_commit_required and the operator saw
// "操作结果未确认" on a login that had actually worked.

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
)

func TestRealAuthflowReachesSucceeded(t *testing.T) {
	vault, err := accountvault.Open(filepath.Join(t.TempDir(), "flows"))
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()

	tr := &inviteTransport{}
	save := SaveProfile(mustProviderVault(t))
	driver := &LoginDriver{
		HTTP:       &http.Client{Transport: tr},
		Save:       save,
		InviteCode: "-",
	}
	svc, err := authflow.Open(authflow.Options{
		Vault:   vault,
		Drivers: map[string]authflow.Driver{"autoclaw": driver},
		Now:     func() time.Time { return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}

	flow, err := svc.Start(t.Context(), authflow.Request{Provider: "autoclaw", Method: "sms"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if flow.Status != "waiting_phone" {
		t.Fatalf("status=%q", flow.Status)
	}
	flow, err = svc.Step(t.Context(), flow.ID, "send_code", authflow.Input{Phone: "13800000000"})
	if err != nil || flow.Status != "waiting_code" {
		t.Fatalf("send_code: status=%q err=%v", flow.Status, err)
	}
	flow, err = svc.Step(t.Context(), flow.ID, "submit_code", authflow.Input{Code: "123456"})
	if err != nil {
		t.Fatalf("submit_code: %v", err)
	}
	if flow.Status != "verified" {
		t.Fatalf("after submit_code status=%q want verified (a provider must not claim success before commit)", flow.Status)
	}

	// The service's own guard is what turned the old flow into "uncertain".
	flow, err = svc.Step(t.Context(), flow.ID, "commit", authflow.Input{})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if flow.Status != "succeeded" {
		t.Fatalf("final status=%q want succeeded (notice=%q error=%q)",
			flow.Status, flow.Notice, flow.ErrorCode)
	}
	if flow.AccountID == "" {
		t.Fatal("succeeded flow carries no account id")
	}
	if strings.Contains(string(mustJSON(t, flow)), "synthetic.access.token") {
		t.Fatal("the access token leaked into the public flow view")
	}
}

func mustProviderVault(t *testing.T) *accountvault.Vault {
	t.Helper()
	v, err := accountvault.Open(filepath.Join(t.TempDir(), "provider-accounts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return v
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A commit with no proven identity must ask for a fresh login instead of
// writing an empty account.
func TestCommitWithoutProfileAsksForLogin(t *testing.T) {
	d := &LoginDriver{HTTP: &http.Client{Transport: stubbedOK{}}, InviteCode: "-"}
	got, err := d.Step(t.Context(), flowWithSecret(mustSecretRaw(t, loginState{Phone: "13800000000"})), "commit", authflow.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "manual_action" || got.ErrorCode != "resume_requires_login" {
		t.Fatalf("status=%q code=%q", got.Status, got.ErrorCode)
	}
}

func mustSecretRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type stubbedOK struct{}

func (stubbedOK) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"code":0}`)), Request: r}, nil
}
