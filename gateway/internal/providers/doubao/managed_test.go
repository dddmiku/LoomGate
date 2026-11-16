package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
)

type managedFixture struct {
	state, reason                    string
	err                              error
	starts, polls, cancels, launches int
	before                           func()
}

func (m *managedFixture) Endpoint(_ context.Context, start bool) (int, error) {
	if start {
		m.launches++
	}
	return 19227, nil
}
func (m *managedFixture) Login(_ context.Context, action, nonce, method string) (string, string, error) {
	if len(nonce) != 32 || method != "qr" && method != "sms" {
		return "", "", errors.New("bad_fixture_request")
	}
	if m.before != nil {
		m.before()
	}
	switch action {
	case "login_start":
		m.starts++
	case "login_poll":
		m.polls++
	case "login_cancel":
		m.cancels++
	}
	return m.state, m.reason, m.err
}
func managedService(t *testing.T) (*authflow.Service, *ManagedConnection, *managedFixture, *bindingRuntimeFixture, *accountvault.Vault, *time.Time, *accountvault.Vault) {
	t.Helper()
	binding, r, v := connectionFixture(t)
	r.err = runtimeError("desktop_login_required")
	m := &managedFixture{state: "waiting_scan"}
	d := &ManagedConnection{Binding: binding, Client: m}
	fv, err := accountvault.Open(filepath.Join(t.TempDir(), "flows"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fv.Close() })
	now := time.Now()
	s, err := authflow.Open(authflow.Options{Vault: fv, Drivers: map[string]authflow.Driver{"doubao": d}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return s, d, m, r, v, &now, fv
}
func TestManagedIntentPersistedBeforeOfficialUIAndRecoveryDoesNotReopen(t *testing.T) {
	s, d, m, r, v, now, fv := managedService(t)
	m.before = func() {
		_, raw, err := fv.Get("authentication-flows")
		if err != nil {
			t.Fatal(err)
		}
		var flows []authflow.Flow
		if json.Unmarshal(raw, &flows) != nil || len(flows) != 1 || len(flows[0].Secret) == 0 {
			t.Fatal("login intent was not durable before UI action")
		}
	}
	f, err := s.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "qr"})
	if err != nil || f.Status != "waiting_scan" || f.InputMode != "official" {
		t.Fatal("official login not started")
	}
	if rows, _ := v.List(); len(rows) != 0 {
		t.Fatal("unverified account saved")
	}
	// Reopen the flow service over the same protected store to model recovery.
	s, err = authflow.Open(authflow.Options{Vault: fv, Drivers: map[string]authflow.Driver{"doubao": d}, Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(4 * time.Second)
	m.state = "scanned"
	f, err = s.Step(context.Background(), f.ID, "poll", authflow.Input{})
	if err != nil || f.Status != "scanned" {
		t.Fatal("scan improperly finalized")
	}
	r.err = nil
	m.state = "verified"
	*now = now.Add(10 * time.Second)
	f, err = s.Step(context.Background(), f.ID, "verify", authflow.Input{})
	if err != nil || f.Status != "verified" {
		t.Fatal("identity not verified")
	}
	f, err = s.Step(context.Background(), f.ID, "commit", authflow.Input{})
	if err != nil || f.Status != "succeeded" {
		t.Fatal("verified account not committed")
	}
	rows, _ := v.List()
	if len(rows) != 1 || rows[0].Revision != 1 || m.starts != 1 {
		t.Fatal("recovery duplicated side effects")
	}
}
func TestManagedSMSStaysOfficialAndDoesNotResend(t *testing.T) {
	s, _, m, _, v, now, _ := managedService(t)
	m.state = "waiting_phone"
	f, err := s.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "sms"})
	if err != nil || f.Status != "waiting_phone" {
		t.Fatal("SMS flow missing")
	}
	if _, err = s.Step(context.Background(), f.ID, "send_code", authflow.Input{Phone: "+1 2025550100"}); err != authflow.ErrInvalid {
		t.Fatal("official form accepted local sensitive input")
	}
	*now = now.Add(4 * time.Second)
	m.state = "waiting_code"
	f, err = s.Step(context.Background(), f.ID, "poll", authflow.Input{})
	if err != nil || f.Status != "waiting_code" {
		t.Fatal("official SMS state not observed")
	}
	*now = now.Add(10 * time.Second)
	m.state = "manual_action"
	m.reason = "official_code_rejected"
	f, err = s.Step(context.Background(), f.ID, "verify", authflow.Input{})
	if err != nil || f.Status != "manual_action" {
		t.Fatal("invalid code did not preserve official action")
	}
	rows, _ := v.List()
	if len(rows) != 0 || m.starts != 1 {
		t.Fatal("SMS retry or unverified save")
	}
	if _, err = s.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "sms"}); err != authflow.ErrBusy {
		t.Fatal("active SMS flow replaced")
	}
	m.err = errors.New("synthetic_cancel_lost")
	f, err = s.Step(context.Background(), f.ID, "cancel", authflow.Input{})
	if err != nil || f.Status != "uncertain" {
		t.Fatal("unknown cancel reported success")
	}
	m.err = nil
	f, err = s.Step(context.Background(), f.ID, "cancel", authflow.Input{})
	if err != nil || f.Status != "canceled" {
		t.Fatal("cancel not confirmed")
	}
}
func TestManagedCommitRejectsSwitchAndDisable(t *testing.T) {
	for _, kind := range []string{"switch", "disable"} {
		t.Run(kind, func(t *testing.T) {
			s, _, m, r, v, _, _ := managedService(t)
			r.err = nil
			m.state = "verified"
			f, err := s.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "qr"})
			if err != nil || f.Status != "verified" {
				t.Fatal("setup failed")
			}
			f, err = s.Step(context.Background(), f.ID, "commit", authflow.Input{})
			if err != nil || f.Status != "succeeded" {
				t.Fatal("setup commit failed")
			}
			f, err = s.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop", AccountID: f.AccountID})
			if err != nil || f.Status != "verified" {
				t.Fatal("relogin setup failed")
			}
			rows, _ := v.List()
			before := rows[0].Revision
			if kind == "switch" {
				r.identity = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			} else {
				a, b, e := v.Get(rows[0].ID)
				if e != nil {
					t.Fatal(e)
				}
				a.Enabled = false
				if _, e = v.Put(a, b, a.Revision); e != nil {
					t.Fatal(e)
				}
				before++
			}
			f, err = s.Step(context.Background(), f.ID, "commit", authflow.Input{})
			if err != nil || f.Status == "succeeded" {
				t.Fatal("changed account accepted")
			}
			rows, _ = v.List()
			if rows[0].Revision != before {
				t.Fatal("relogin overwrote account change")
			}
		})
	}
}
func TestManagedUnknownStartPreservesIntentAndExpiry(t *testing.T) {
	s, _, m, r, v, now, _ := managedService(t)
	m.err = errors.New("synthetic_lost_response")
	f, err := s.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "qr"})
	if err != nil || f.Status != "uncertain" {
		t.Fatal("unknown start not quarantined")
	}
	r.err = nil
	m.err = nil
	m.state = "verified"
	*now = now.Add(4 * time.Second)
	f, err = s.Step(context.Background(), f.ID, "verify", authflow.Input{})
	if err != nil || f.Status != "verified" || m.starts != 1 {
		t.Fatal("unknown start not recovered read-only")
	}
	*now = now.Add(time.Hour)
	if _, err = s.Step(context.Background(), f.ID, "commit", authflow.Input{}); err != authflow.ErrInvalid {
		t.Fatal("expired flow committed")
	}
	rows, _ := v.List()
	if len(rows) != 0 {
		t.Fatal("expired intent created account")
	}
}

func TestManagedExistingSessionNotMisreportedAsFreshLogin(t *testing.T) {
	s, _, m, r, _, now, _ := managedService(t)
	r.err = nil
	m.state = "already_authenticated"
	f, err := s.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "qr"})
	if err != nil || f.Status != "waiting_authorization" {
		t.Fatal("existing session was called fresh login")
	}
	*now = now.Add(4 * time.Second)
	f, err = s.Step(context.Background(), f.ID, "verify", authflow.Input{})
	if err != nil || f.Status != "verified" || f.Authentication != "existing_session" {
		t.Fatal("existing authentication provenance lost")
	}
	if m.polls != 0 {
		t.Fatal("existing session polled a nonexistent login")
	}
}
