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

func TestManagedMFARecoversWithoutRestartingLoginAndRetainsExpiry(t *testing.T) {
	for _, end := range []string{"recover", "cancel", "expire"} {
		t.Run(end, func(t *testing.T) {
			s, d, m, r, v, now, fv := managedService(t)
			m.state = "manual_action"
			m.reason = "official_verification_required"
			f, err := s.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "qr"})
			if err != nil || f.Status != "manual_action" || f.ErrorCode != "official_mfa_required" || f.InputMode != "official" {
				t.Fatal("MFA not projected as an official action")
			}
			expires := f.ExpiresAt
			for _, input := range []authflow.Input{{Phone: "synthetic-sensitive"}, {Code: "000000"}, {Callback: "synthetic-sensitive"}} {
				if _, err = s.Step(context.Background(), f.ID, "verify", input); err != authflow.ErrInvalid {
					t.Fatal("MFA accepted sensitive input")
				}
			}
			s, err = authflow.Open(authflow.Options{Vault: fv, Drivers: map[string]authflow.Driver{"doubao": d}, Now: func() time.Time { return *now }})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := s.List()
			if err != nil || len(rows) != 1 || !rows[0].ExpiresAt.Equal(expires) || !rows[0].CanVerify {
				t.Fatal("MFA restart lost its state or extended deadline")
			}
			switch end {
			case "recover":
				*now = now.Add(5 * time.Second)
				m.state = "verified"
				r.err = nil
				f, err = s.Step(context.Background(), f.ID, "verify", authflow.Input{})
				if err != nil || f.Status != "verified" || f.Authentication != "official_login" {
					t.Fatal("MFA completion was not verified")
				}
			case "cancel":
				f, err = s.Step(context.Background(), f.ID, "cancel", authflow.Input{})
				if err != nil || f.Status != "canceled" || m.cancels != 1 {
					t.Fatal("MFA cancellation failed")
				}
			case "expire":
				*now = now.Add(11 * time.Minute)
				rows, err = s.List()
				if err != nil || rows[0].Status != "expired" || rows[0].CanVerify {
					t.Fatal("expired MFA remained actionable")
				}
				if _, err = s.Step(context.Background(), f.ID, "verify", authflow.Input{}); err != authflow.ErrInvalid {
					t.Fatal("expired MFA was polled")
				}
			}
			accounts, _ := v.List()
			if len(accounts) != 0 || m.starts != 1 || m.launches != 1 {
				t.Fatal("MFA recovery performed an extra start or saved uncommitted account")
			}
		})
	}
}

func TestManagedLostOutcomeRetainsErrorForSafeRecovery(t *testing.T) {
	s, _, m, _, _, _, _ := managedService(t)
	m.err = errors.New("synthetic-interrupted-start")
	f, err := s.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "qr"})
	if err != nil || f.Status != "uncertain" || f.ErrorCode != "official_login_unconfirmed" {
		t.Fatal("uncertain driver result was replaced by generic invalid state")
	}
}

func TestSessionExpiryReadbackAndRenewalNeverMutateAccount(t *testing.T) {
	d, r, v := connectionFixture(t)
	start, err := d.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := d.Step(context.Background(), bindingFlow(start), "commit", authflow.Input{})
	if err != nil {
		t.Fatal(err)
	}
	a, before, err := v.Get(saved.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	r.err = runtimeError("desktop_login_required")
	state, err := d.Status(context.Background())
	if err != nil || state.State != "login_required" || state.Accounts[0].State != "login_required" {
		t.Fatal("expired official session presented as connected")
	}
	r.err = nil
	state, err = d.Status(context.Background())
	if err != nil || state.State != "ready" {
		t.Fatal("official session recovery not recognized")
	}
	after, plain, err := v.Get(saved.AccountID)
	if err != nil || a.Revision != after.Revision || string(plain) != string(before) {
		t.Fatal("read-only renewal changed credentials or revision")
	}
	v.SetEnabled(a.ID, false, a.Revision)
	state, err = d.Status(context.Background())
	if err != nil || state.State == "ready" || state.Accounts[0].State != "disabled" {
		t.Fatal("renewal resurrected disabled account")
	}
}

func TestConcurrentManagedRenewalCannotOverwriteNewerBinding(t *testing.T) {
	d, r, v := connectionFixture(t)
	start, err := d.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := d.Step(context.Background(), bindingFlow(start), "commit", authflow.Input{})
	if err != nil {
		t.Fatal(err)
	}
	newFlow := func() (*authflow.Service, authflow.View) {
		fv, err := accountvault.Open(filepath.Join(t.TempDir(), "flow"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { fv.Close() })
		binding := &DesktopConnection{Vault: v, Runtime: r}
		service, err := authflow.Open(authflow.Options{Vault: fv, Drivers: map[string]authflow.Driver{"doubao": &ManagedConnection{Binding: binding, Client: &managedFixture{state: "verified"}}}})
		if err != nil {
			t.Fatal(err)
		}
		flow, err := service.Start(context.Background(), authflow.Request{Provider: "doubao", Method: "desktop", AccountID: saved.AccountID})
		if err != nil || flow.Status != "verified" {
			t.Fatal("renewal verification failed")
		}
		return service, flow
	}
	first, a := newFlow()
	second, b := newFlow()
	if result, err := first.Step(context.Background(), a.ID, "commit", authflow.Input{}); err != nil || result.Status != "succeeded" {
		t.Fatal("first renewal did not commit")
	}
	before, plain, _ := v.Get(saved.AccountID)
	result, err := second.Step(context.Background(), b.ID, "commit", authflow.Input{})
	if err != nil || result.Status == "succeeded" || result.ErrorCode != "account_revision_conflict" {
		t.Fatal("stale renewal overwrote newer binding")
	}
	after, current, _ := v.Get(saved.AccountID)
	if after.Revision != before.Revision || string(current) != string(plain) {
		t.Fatal("stale renewal changed account material")
	}
}
