package taskproviders

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
)

func TestSequenceAccountScopeUsesFirstFamilyRealmWithoutBroadening(t *testing.T) {
	accounts, _, _ := setupModules(t)
	checkin, err := accounts.SequenceAccounts(context.Background(), nil, []string{"credential-preflight", "checkin", "balance"})
	if err != nil || !reflect.DeepEqual(checkin, []string{"cn-fixture", "global-fixture"}) {
		t.Fatalf("check-in sequence realms = %v, err=%v", checkin, err)
	}
	growth, err := accounts.SequenceAccounts(context.Background(), nil, []string{"travel/", "growth/", "makeup"})
	if err != nil || !reflect.DeepEqual(growth, []string{"cn-fixture"}) {
		t.Fatalf("growth sequence realms = %v, err=%v", growth, err)
	}
	if _, err = accounts.SequenceAccounts(context.Background(), []string{"global-fixture"}, []string{"travel/", "growth/"}); err == nil {
		t.Fatal("explicit account outside first-family realm was silently accepted")
	}
	if _, err = accounts.SequenceAccounts(context.Background(), []string{"global-fixture"}, []string{"credential-preflight", "checkin"}); err != nil {
		t.Fatalf("explicit valid sequence scope rejected: %v", err)
	}
}

func TestCheckinSequencePreflightsThenSignsInAndReadsBalance(t *testing.T) {
	for _, nearExpiry := range []bool{false, true} {
		name := "safe_window"
		if nearExpiry {
			name = "inside_refresh_window_without_vault"
		}
		t.Run(name, func(t *testing.T) {
			accounts, fixture, pool := setupModules(t)
			account := pool.AuthByUID("cn-fixture")
			account.FilePath = filepath.Join(t.TempDir(), "cn-account.json")
			account.ExpiresAt = time.Now().Add(time.Hour).Unix()
			if nearExpiry {
				account.ExpiresAt = time.Now().Add(5 * time.Minute).Unix()
			}
			if err := account.SaveAtomic(); err != nil {
				t.Fatal(err)
			}
			engine, err := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "sequence.json"), Timeout: 2 * time.Second, PollMin: time.Millisecond, PollMax: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			if err = engine.Register("workbuddy", accounts); err != nil {
				t.Fatal(err)
			}
			sequence := []string{"credential-preflight", "checkin", "balance"}
			run, err := engine.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "checkin-balance-sequence", TaskSequence: sequence, AccountIDs: []string{"cn-fixture"}})
			if err != nil {
				t.Fatal(err)
			}
			final := waitModuleRun(t, engine, run.ID)
			fixture.mu.Lock()
			checkins := fixture.counts["/v2/billing/meter/daily-checkin"]
			balances := fixture.counts["/v2/billing/meter/get-user-resource"]
			fixture.mu.Unlock()
			if nearExpiry {
				if final.Status != taskengine.StatusFailed || checkins != 0 || balances != 1 {
					t.Fatalf("preflight without durable refresh vault must stop before writes: status=%s checkins=%d balances=%d", final.Status, checkins, balances)
				}
				return
			}
			if final.Status != taskengine.StatusSucceeded || checkins != 1 || balances != 1 {
				t.Fatalf("sequence did not complete preflight→checkin→balance: status=%s checkins=%d balances=%d", final.Status, checkins, balances)
			}
			steps := []string{}
			for _, step := range final.Steps {
				if step.Stage == taskengine.StageInspect || step.Stage == taskengine.StageExecute || step.Stage == taskengine.StageClaim || step.Stage == taskengine.StageVerify {
					steps = append(steps, step.TaskKey+":"+string(step.Stage))
				}
			}
			firstCheckin, firstBalance := -1, -1
			for i, step := range steps {
				if strings.HasPrefix(step, "checkin:") && firstCheckin < 0 {
					firstCheckin = i
				}
				if strings.HasPrefix(step, "balance:") && firstBalance < 0 {
					firstBalance = i
				}
			}
			if firstCheckin < 0 || firstBalance <= firstCheckin {
				t.Fatalf("balance did not follow checkin in durable run steps: %v", steps)
			}
		})
	}
}

func TestCheckinSequenceRefreshesOnlyExpiringCredentialsAndKeepsGlobalReadOnly(t *testing.T) {
	for _, realm := range []string{"cn", "global"} {
		for _, nearExpiry := range []bool{false, true} {
			name := realm + "/safe"
			if nearExpiry {
				name = realm + "/expiring"
			}
			t.Run(name, func(t *testing.T) {
				original, fixture, pool := setupModules(t)
				account := pool.AuthByUID(realm + "-fixture")
				account.FilePath = filepath.Join(t.TempDir(), "synthetic-account.json")
				account.RefreshToken = "synthetic-refresh-fixture"
				if nearExpiry {
					account.ExpiresAt = time.Now().Add(5 * time.Minute).Unix()
				}
				if err := account.SaveAtomic(); err != nil {
					t.Fatal(err)
				}
				vault, err := accountvault.Open(filepath.Join(t.TempDir(), "vault"))
				if err != nil {
					t.Fatal(err)
				}
				defer vault.Close()
				var refreshes atomic.Int32
				adapter, err := NewAccounts(pool, original.Upstream, "workbuddy", time.Hour, AccountsOptions{RefreshVault: vault, Refresh: func(context.Context, *auth.Auth) (auth.TokenUpdate, error) {
					refreshes.Add(1)
					return auth.TokenUpdate{AccessToken: "synthetic-refreshed-access", RefreshToken: "synthetic-refreshed-refresh", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				engine := keepaliveEngine(t, filepath.Join(t.TempDir(), "engine.json"), "workbuddy", adapter)
				defer engine.Close()
				r, err := engine.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "checkin-balance-sequence", TaskSequence: []string{"credential-preflight", "checkin", "balance"}, AccountIDs: []string{account.UID}})
				if err != nil {
					t.Fatal(err)
				}
				final := waitModuleRun(t, engine, r.ID)
				wantRefresh := int32(0)
				if nearExpiry {
					wantRefresh = 1
				}
				if final.Status != taskengine.StatusSucceeded || refreshes.Load() != wantRefresh {
					t.Fatalf("preflight status=%s refreshes=%d want=%d", final.Status, refreshes.Load(), wantRefresh)
				}
				if persisted, err := account.PersistedRevision(); err != nil || persisted != account.CredentialRevision() {
					t.Fatal("preflight credential file and account snapshot diverged")
				}
				fixture.mu.Lock()
				checkins := fixture.counts["/v2/billing/meter/daily-checkin"]
				balances := fixture.counts["/v2/billing/meter/get-user-resource"] + fixture.counts["/billing/meter/get-user-resource"]
				fixture.mu.Unlock()
				wantCheckin := 0
				if realm == "cn" {
					wantCheckin = 1
				}
				if checkins != wantCheckin || balances != 1 {
					t.Fatalf("wrong realm operations: checkins=%d want=%d balances=%d", checkins, wantCheckin, balances)
				}
			})
		}
	}
}

func TestCheckinSequenceUnknownRefreshOnlyReadsBalanceAfterRestart(t *testing.T) {
	original, fixture, pool := setupModules(t)
	account := pool.AuthByUID("cn-fixture")
	account.FilePath = filepath.Join(t.TempDir(), "synthetic-account.json")
	account.RefreshToken = "synthetic-refresh-fixture"
	account.ExpiresAt = time.Now().Add(5 * time.Minute).Unix()
	if err := account.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	vault, err := accountvault.Open(filepath.Join(t.TempDir(), "vault"))
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	var refreshes atomic.Int32
	adapter, err := NewAccounts(pool, original.Upstream, "workbuddy", time.Hour, AccountsOptions{RefreshVault: vault, Refresh: func(context.Context, *auth.Auth) (auth.TokenUpdate, error) {
		refreshes.Add(1)
		return auth.TokenUpdate{}, errors.New("synthetic_lost_refresh_response")
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "engine.json")
	engine := keepaliveEngine(t, path, "workbuddy", adapter)
	defer func() { _ = engine.Close() }()
	req := taskengine.Request{ProviderID: "workbuddy", TaskKey: "checkin-balance-sequence", TaskSequence: []string{"credential-preflight", "checkin", "balance"}, AccountIDs: []string{account.UID}}
	first, err := engine.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	if run := waitModuleRun(t, engine, first.ID); run.Status != taskengine.StatusUncertain {
		t.Fatalf("lost refresh ended %s", run.Status)
	}
	_ = engine.Close()
	engine = keepaliveEngine(t, path, "workbuddy", adapter)
	r, err := engine.Submit(req)
	if err != nil || r.ID != first.ID {
		t.Fatalf("restart lost exact unknown run: %v", err)
	}
	if run := waitModuleRun(t, engine, r.ID); run.Status != taskengine.StatusUncertain || refreshes.Load() != 1 {
		t.Fatalf("unknown refresh replayed or confirmed: %s calls=%d", run.Status, refreshes.Load())
	}
	fixture.mu.Lock()
	checkins := fixture.counts["/v2/billing/meter/daily-checkin"]
	balances := fixture.counts["/v2/billing/meter/get-user-resource"]
	fixture.mu.Unlock()
	if checkins != 0 || balances != 2 {
		t.Fatalf("unknown must only run the read-only balance tail: writes=%d reads=%d", checkins, balances)
	}
}

func TestCheckinCredentialPreflightDoesNotRefreshOutsideWindow(t *testing.T) {
	root := t.TempDir()
	account := keepaliveAccount(t, root, "synthetic-preflight", "cn")
	module, err := newCheckinRefreshModule(&Accounts{Provider: "workbuddy"})
	if err != nil || module == nil {
		t.Fatalf("build preflight module: module=%v err=%v", module, err)
	}
	definitions, err := module.Discover(context.Background(), account)
	if err != nil || len(definitions) != 1 {
		t.Fatalf("discover preflight: %d err=%v", len(definitions), err)
	}
	task := taskengine.Task{Definition: definitions[0], Target: taskengine.Target{AccountID: account.UID, Realm: "cn"}}
	state, err := module.Inspect(context.Background(), account, task)
	if err != nil || state.Phase != "done" || module.(*checkinRefreshModule).keepalive != nil {
		t.Fatalf("non-expiring credential was not a read-only no-op: state=%+v err=%v", state, err)
	}
	account.ExpiresAt = time.Now().Add(5 * time.Minute).Unix()
	if err = account.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	task.PeriodKey = account.CredentialRevision()
	state, err = module.Inspect(context.Background(), account, task)
	if err != nil || state.Phase != "blocked" {
		t.Fatalf("near-expiry credential without secure refresh store was not blocked: state=%+v err=%v", state, err)
	}
}
