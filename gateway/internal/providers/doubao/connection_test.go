package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
)

type bindingRuntimeFixture struct {
	identity string
	err      error
	calls    int
	after    func()
}

type interruptedBindingDriver struct {
	*DesktopConnection
	interrupt bool
}

func (d *interruptedBindingDriver) Step(ctx context.Context, f authflow.Flow, action string, input authflow.Input) (authflow.Result, error) {
	result, err := d.DesktopConnection.Step(ctx, f, action, input)
	if err == nil && result.Status == "succeeded" && d.interrupt {
		d.interrupt = false
		return authflow.Result{}, errors.New("synthetic_result_interrupted")
	}
	return result, err
}
func TestDesktopSavedAccountSurvivesInterruptedFlowAndRestart(t *testing.T) {
	d, _, v := connectionFixture(t)
	flowDir := filepath.Join(t.TempDir(), "flows")
	fv, err := accountvault.Open(flowDir)
	if err != nil {
		t.Fatal(err)
	}
	driver := &interruptedBindingDriver{DesktopConnection: d, interrupt: true}
	now := time.Now()
	opts := authflow.Options{Vault: fv, Drivers: map[string]authflow.Driver{"doubao": driver}, Now: func() time.Time { return now }}
	s, err := authflow.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Step(context.Background(), f.ID, "commit", authflow.Input{})
	if err != nil || result.Status != "uncertain" {
		t.Fatal("interrupted save not marked uncertain")
	}
	rows, err := v.List()
	if err != nil || len(rows) != 1 {
		t.Fatal("expected one saved binding")
	}
	revision := rows[0].Revision
	fv.Close()
	fv, err = accountvault.Open(flowDir)
	if err != nil {
		t.Fatal(err)
	}
	defer fv.Close()
	opts.Vault = fv
	s, err = authflow.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Second)
	result, err = s.Step(context.Background(), f.ID, "verify", authflow.Input{})
	if err != nil || result.Status != "verified" {
		t.Fatal("read-only recovery failed")
	}
	rows, _ = v.List()
	if len(rows) != 1 || rows[0].Revision != revision {
		t.Fatal("verification mutated saved binding")
	}
	result, err = s.Step(context.Background(), f.ID, "commit", authflow.Input{})
	if err != nil || result.Status != "succeeded" {
		t.Fatal("idempotent flow completion failed")
	}
	rows, _ = v.List()
	if len(rows) != 1 || rows[0].Revision != revision {
		t.Fatal("interrupted commit replayed account write")
	}
}

func TestDesktopInvalidProofAndDuplicateStatus(t *testing.T) {
	d, r, v := connectionFixture(t)
	r.identity = "invalid"
	status, err := d.Status(context.Background())
	if err != nil || status.State != "disconnected" {
		t.Fatal("invalid identity reported connected")
	}
	result, err := d.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop"})
	if err != nil || result.Status == "verified" {
		t.Fatal("invalid identity verified")
	}
	r.identity = strings.Repeat("a", 64)
	raw, _ := json.Marshal(StoredProfile{Runtime: &RuntimeBinding{Identity: r.identity}, BotID: "synthetic-bot"})
	for _, id := range []string{"synthetic-one", "synthetic-two"} {
		if _, err := v.Put(accountvault.Account{ID: id, Provider: "doubao", Enabled: true}, raw, 0); err != nil {
			t.Fatal(err)
		}
	}
	status, err = d.Status(context.Background())
	if err != nil || status.State != "conflict" {
		t.Fatal("duplicate bindings reported ready")
	}
	result, err = d.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop"})
	if err != nil || result.Status == "verified" || result.ErrorCode != "duplicate_account_binding" {
		t.Fatal("duplicate binding accepted")
	}
}

func (f *bindingRuntimeFixture) Bind(context.Context) (RuntimeBinding, error) {
	if f.err != nil {
		return RuntimeBinding{}, f.err
	}
	return RuntimeBinding{f.identity}, nil
}
func (f *bindingRuntimeFixture) Connection(_ context.Context, expected RuntimeBinding) (ConnectionProof, error) {
	f.calls++
	if f.err != nil {
		return ConnectionProof{}, f.err
	}
	if expected.Identity != "" && expected.Identity != f.identity {
		return ConnectionProof{}, runtimeError("desktop_identity_mismatch")
	}
	proof := ConnectionProof{Binding: RuntimeBinding{f.identity}, BotID: "synthetic-dynamic-bot", Models: 3}
	if f.after != nil {
		action := f.after
		f.after = nil
		action()
	}
	return proof, nil
}
func connectionFixture(t *testing.T) (*DesktopConnection, *bindingRuntimeFixture, *accountvault.Vault) {
	t.Helper()
	v, err := accountvault.Open(filepath.Join(t.TempDir(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	r := &bindingRuntimeFixture{identity: strings.Repeat("a", 64)}
	return &DesktopConnection{Vault: v, Runtime: r}, r, v
}
func bindingFlow(result authflow.Result) authflow.Flow {
	return authflow.Flow{View: authflow.View{Request: authflow.Request{Provider: "doubao", Method: "desktop", AccountID: result.AccountID}, Status: result.Status}, Secret: result.Secret}
}
func TestDesktopConnectionNewSaveReverifyAndIdempotentRecovery(t *testing.T) {
	d, r, v := connectionFixture(t)
	start, err := d.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop"})
	if err != nil || start.Status != "verified" {
		t.Fatal("current client not verified")
	}
	if rows, _ := v.List(); len(rows) != 0 {
		t.Fatal("start wrote an account before commit")
	}
	recheck, err := d.Verify(context.Background(), bindingFlow(start))
	if err != nil || recheck.Status != "verified" {
		t.Fatal("new unsaved candidate cannot be reverified")
	}
	done, err := d.Step(context.Background(), bindingFlow(recheck), "commit", authflow.Input{})
	if err != nil || done.Status != "succeeded" {
		t.Fatal("save failed")
	}
	a, p, err := d.profile(done.AccountID)
	if err != nil || a.Revision != 1 || p.Runtime.Identity != r.identity || p.BindingFlow == "" || p.VerifiedAt.IsZero() {
		t.Fatal("binding metadata missing")
	}
	again, err := d.Step(context.Background(), bindingFlow(recheck), "commit", authflow.Input{})
	if err != nil || again.Status != "succeeded" {
		t.Fatal("commit recovery failed")
	}
	latest, _, _ := d.profile(done.AccountID)
	if latest.Revision != a.Revision {
		t.Fatal("same commit rewrote account")
	}
	status, err := d.Status(context.Background())
	raw, _ := json.Marshal(status)
	if err != nil || status.State != "ready" || strings.Contains(string(raw), r.identity) || strings.Contains(string(raw), "synthetic-dynamic-bot") {
		t.Fatal("public status leaked private proof or lost readiness")
	}
}
func TestDesktopConnectionSwitchAndConcurrentDisableFailClosed(t *testing.T) {
	for _, what := range []string{"switch", "disable", "revision"} {
		t.Run(what, func(t *testing.T) {
			d, r, v := connectionFixture(t)
			start, _ := d.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop"})
			done, err := d.Step(context.Background(), bindingFlow(start), "commit", authflow.Input{})
			if err != nil {
				t.Fatal(err)
			}
			verified, _ := d.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop", AccountID: done.AccountID})
			a, _, _ := d.profile(done.AccountID)
			switch what {
			case "switch":
				r.identity = strings.Repeat("b", 64)
			case "disable":
				_, err = v.SetEnabled(a.ID, false, a.Revision)
			case "revision":
				_, err = v.SetEnabled(a.ID, true, a.Revision)
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := d.Step(context.Background(), bindingFlow(verified), "commit", authflow.Input{})
			if err != nil || result.Status == "succeeded" {
				t.Fatal("changed identity or revision overwritten")
			}
			rows, _ := v.List()
			if len(rows) != 1 {
				t.Fatal("conflict created another account")
			}
		})
	}
}
func TestDesktopConnectionDisconnectedFlowResumesAfterServiceRestart(t *testing.T) {
	d, r, _ := connectionFixture(t)
	r.err = runtimeError("desktop_unavailable")
	dir := filepath.Join(t.TempDir(), "flows")
	v, err := accountvault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	options := authflow.Options{Vault: v, Drivers: map[string]authflow.Driver{"doubao": d}, Now: func() time.Time { return now }}
	service, err := authflow.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	flow, err := service.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop"})
	if err != nil || flow.Status != "waiting_authorization" || !flow.CanVerify {
		t.Fatal("disconnected flow not recoverable")
	}
	v.Close()
	v, err = accountvault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	options.Vault = v
	service, err = authflow.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	r.err = nil
	now = now.Add(10 * time.Second)
	verified, err := service.Step(context.Background(), flow.ID, "verify", authflow.Input{})
	if err != nil || verified.Status != "verified" {
		t.Fatal("restarted flow did not resume")
	}
	saved, err := service.Step(context.Background(), flow.ID, "commit", authflow.Input{})
	if err != nil || saved.Status != "succeeded" {
		t.Fatal("resumed commit failed")
	}
	for _, file := range []string{"authentication-flows.account"} {
		raw, _ := os.ReadFile(filepath.Join(dir, file))
		if strings.Contains(string(raw), r.identity) {
			t.Fatal("flow proof persisted in plaintext")
		}
	}
}
func TestDesktopConnectionNeverClobbersLegacyOrCreatesDuplicateIdentity(t *testing.T) {
	d, r, v := connectionFixture(t)
	raw, _ := json.Marshal(StoredProfile{Runtime: &RuntimeBinding{r.identity}, BotID: "synthetic-bot"})
	a, err := v.Put(accountvault.Account{ID: "retained-alias", Provider: "doubao", Enabled: true}, raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop"})
	if err != nil || result.AccountID != a.ID {
		t.Fatal("existing identity alias duplicated")
	}
	v.SetEnabled(a.ID, false, a.Revision)
	result, err = d.Step(context.Background(), bindingFlow(result), "commit", authflow.Input{})
	if err != nil || result.Status == "succeeded" {
		t.Fatal("disabled account re-enabled")
	}
}

func TestDesktopReconnectionPreservesExistingProfileFields(t *testing.T) {
	d, r, v := connectionFixture(t)
	raw, _ := json.Marshal(StoredProfile{Runtime: &RuntimeBinding{r.identity}, BotID: "synthetic-old-bot", Query: url.Values{"synthetic-option": {"retained"}}, Headers: http.Header{"X-Synthetic-Option": {"retained"}}})
	a, err := v.Put(accountvault.Account{ID: "retained-alias", Provider: "doubao", Enabled: true}, raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop", AccountID: a.ID})
	if err != nil || result.Status != "verified" {
		t.Fatal("existing profile not verified")
	}
	result, err = d.Step(context.Background(), bindingFlow(result), "commit", authflow.Input{})
	if err != nil || result.Status != "succeeded" {
		t.Fatal("reconnect failed")
	}
	_, profile, err := d.profile(a.ID)
	if err != nil || profile.Query.Get("synthetic-option") != "retained" || profile.Headers.Get("X-Synthetic-Option") != "retained" || profile.BotID != "synthetic-dynamic-bot" {
		t.Fatal("reconnection discarded existing profile fields")
	}
}
