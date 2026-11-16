package taskproviders

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

func keepaliveAccount(t *testing.T, root, id, realm string) *auth.Auth {
	t.Helper()
	domain := "www.codebuddy.cn"
	if realm == "global" {
		domain = "www.workbuddy.ai"
	}
	if realm == "trae" {
		domain = "api.trae.cn"
	}
	raw, _ := json.Marshal(map[string]any{"uid": id, "realm": realm, "domain": domain, "accessToken": "synthetic-original", "refreshToken": "synthetic-refresh", "expiresAt": time.Now().Add(time.Hour).Unix()})
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatal("synthetic account invalid")
	}
	a.FilePath = filepath.Join(root, "workbuddy-"+id+".json")
	if err = a.SaveAtomic(); err != nil {
		t.Fatal("synthetic save failed")
	}
	return a
}
func keepaliveEngine(t *testing.T, path, provider string, a *Accounts) *taskengine.Engine {
	t.Helper()
	e, err := taskengine.Open(taskengine.Options{Path: path, Timeout: 2 * time.Second, PollMin: time.Millisecond, PollMax: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Register(provider, a); err != nil {
		t.Fatal(err)
	}
	return e
}
func runKeepalive(t *testing.T, e *taskengine.Engine, provider string, ids ...string) taskengine.Run {
	t.Helper()
	r, err := e.Submit(taskengine.Request{ProviderID: provider, TaskKey: "keepalive", AccountIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	return waitModuleRun(t, e, r.ID)
}

func TestKeepaliveRegisteredForAllRealmsSavesThenReadsBack(t *testing.T) {
	for _, realm := range []string{"cn", "global", "trae"} {
		t.Run(realm, func(t *testing.T) {
			root := t.TempDir()
			p := pool.New("")
			defer p.Close()
			a := keepaliveAccount(t, root, "synthetic-keepalive", realm)
			p.Add(a)
			v, err := accountvault.Open(filepath.Join(root, "secure"))
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			var calls atomic.Int32
			refresh := func(context.Context, *auth.Auth) (auth.TokenUpdate, error) {
				calls.Add(1)
				return auth.TokenUpdate{AccessToken: "synthetic-rotated", RefreshToken: "synthetic-refresh-rotated", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}, nil
			}
			provider := "workbuddy"
			if realm == "trae" {
				provider = "trae"
			}
			adapter, err := NewAccounts(p, nil, provider, time.Hour, AccountsOptions{RefreshVault: v, Refresh: refresh})
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := adapter.DiscoverCatalog(context.Background(), taskengine.Scope{}, "keepalive")
			if err != nil || len(catalog.Tasks) != 1 || calls.Load() != 0 {
				t.Fatal("discovery did not remain read-only")
			}
			e := keepaliveEngine(t, filepath.Join(root, "engine.json"), provider, adapter)
			defer e.Close()
			run := runKeepalive(t, e, provider, a.UID)
			if run.Status != taskengine.StatusSucceeded || calls.Load() != 1 {
				t.Fatal("refresh did not complete exactly once")
			}
			persisted, err := a.PersistedRevision()
			if err != nil || persisted != a.CredentialRevision() || a.Snapshot().AccessToken != "synthetic-rotated" {
				t.Fatal("credentials not persisted and adopted")
			}
			raw, _ := json.Marshal(run)
			if strings.Contains(string(raw), "synthetic-rotated") || strings.Contains(string(raw), "synthetic-refresh") {
				t.Fatal("credentials escaped to task history")
			}
			sealed, _ := os.ReadFile(filepath.Join(root, "secure", refreshRecordID(provider, a.UID)+".account"))
			if strings.Contains(string(sealed), "synthetic-rotated") {
				t.Fatal("refresh journal not encrypted")
			}
		})
	}
}

func TestKeepaliveUnknownNeverReplaysAndSessionDisablePolicy(t *testing.T) {
	for _, mode := range []string{"unknown", "manual", "review", "session_dead", "disabled_while_refreshing"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			p := pool.New("")
			defer p.Close()
			a := keepaliveAccount(t, root, "synthetic-account", "cn")
			p.Add(a)
			switch mode {
			case "manual":
				p.Disable(a.UID, "manual")
			case "review":
				p.Disable(a.UID, pool.ReviewFailReason())
			case "session_dead":
				p.Disable(a.UID, pool.SessionDeadReason())
			}
			v, err := accountvault.Open(filepath.Join(root, "secure"))
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			var calls atomic.Int32
			adapter, err := NewAccounts(p, nil, "workbuddy", time.Hour, AccountsOptions{RefreshVault: v, Refresh: func(context.Context, *auth.Auth) (auth.TokenUpdate, error) {
				calls.Add(1)
				if mode == "unknown" {
					return auth.TokenUpdate{}, errors.New("synthetic_response_lost")
				}
				if mode == "disabled_while_refreshing" {
					p.Disable(a.UID, "manual")
				}
				return auth.TokenUpdate{AccessToken: "synthetic-new", ExpiresAt: time.Now().Add(time.Hour).Unix()}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "engine.json")
			e := keepaliveEngine(t, path, "workbuddy", adapter)
			run := runKeepalive(t, e, "workbuddy", a.UID)
			if mode == "unknown" {
				if run.Status != taskengine.StatusUncertain {
					t.Fatal("unknown incorrectly completed")
				}
				e.Close()
				e = keepaliveEngine(t, path, "workbuddy", adapter)
				if runKeepalive(t, e, "workbuddy", a.UID).Status != taskengine.StatusUncertain || calls.Load() != 1 {
					t.Fatal("unknown refresh replayed")
				}
			}
			defer e.Close()
			status, _ := p.Status(a.UID)
			if mode == "manual" || mode == "review" {
				if calls.Load() != 0 || !status.Disabled {
					t.Fatal("manual/review disable not protected")
				}
			}
			if mode == "session_dead" && (calls.Load() != 1 || status.Disabled || run.Status != taskengine.StatusSucceeded) {
				t.Fatal("session recovery not verified")
			}
			if mode == "disabled_while_refreshing" && !status.Disabled {
				t.Fatal("concurrent manual disable revived")
			}
		})
	}
}

func TestKeepaliveEncryptedResponseRecoversSaveFailureWithoutRefresh(t *testing.T) {
	root := t.TempDir()
	p := pool.New("")
	defer p.Close()
	a := keepaliveAccount(t, root, "synthetic-recover", "cn")
	p.Add(a)
	v, err := accountvault.Open(filepath.Join(root, "secure"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var calls atomic.Int32
	original, _ := os.ReadFile(a.FilePath)
	refresh := func(context.Context, *auth.Auth) (auth.TokenUpdate, error) {
		calls.Add(1)
		if err := os.Remove(a.FilePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(a.FilePath, 0700); err != nil {
			t.Fatal(err)
		}
		return auth.TokenUpdate{AccessToken: "synthetic-pending", ExpiresAt: time.Now().Add(time.Hour).Unix()}, nil
	}
	adapter, err := NewAccounts(p, nil, "workbuddy", time.Hour, AccountsOptions{RefreshVault: v, Refresh: refresh})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "engine.json")
	e := keepaliveEngine(t, path, "workbuddy", adapter)
	if r := runKeepalive(t, e, "workbuddy", a.UID); r.Status != taskengine.StatusUncertain {
		t.Fatal("failed disk save incorrectly succeeded")
	}
	e.Close()
	if a.Snapshot().AccessToken != "synthetic-original" {
		t.Fatal("memory published before persistence")
	}
	if err = os.Remove(a.FilePath); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(a.FilePath, original, 0600); err != nil {
		t.Fatal(err)
	}
	v.Close()
	v, err = accountvault.Open(filepath.Join(root, "secure"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	adapter, err = NewAccounts(p, nil, "workbuddy", time.Hour, AccountsOptions{RefreshVault: v, Refresh: refresh})
	if err != nil {
		t.Fatal(err)
	}
	e = keepaliveEngine(t, path, "workbuddy", adapter)
	defer e.Close()
	if r := runKeepalive(t, e, "workbuddy", a.UID); r.Status != taskengine.StatusSucceeded || calls.Load() != 1 {
		t.Fatal("stored response recovery reissued refresh or failed")
	}
	persisted, err := a.PersistedRevision()
	if err != nil || persisted != a.CredentialRevision() {
		t.Fatal("recovered credential mismatch")
	}
}

func TestKeepaliveStaleRefreshCannotOverwriteConcurrentRelogin(t *testing.T) {
	root := t.TempDir()
	p := pool.New("")
	defer p.Close()
	a := keepaliveAccount(t, root, "synthetic-race", "cn")
	p.Add(a)
	v, err := accountvault.Open(filepath.Join(root, "secure"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var calls atomic.Int32
	adapter, err := NewAccounts(p, nil, "workbuddy", time.Hour, AccountsOptions{RefreshVault: v, Refresh: func(context.Context, *auth.Auth) (auth.TokenUpdate, error) {
		calls.Add(1)
		a.Lock()
		a.AccessToken = "synthetic-relogin"
		a.Unlock()
		if err := a.SaveAtomic(); err != nil {
			t.Fatal(err)
		}
		return auth.TokenUpdate{AccessToken: "synthetic-stale", ExpiresAt: time.Now().Add(time.Hour).Unix()}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	e := keepaliveEngine(t, filepath.Join(root, "engine.json"), "workbuddy", adapter)
	defer e.Close()
	r := runKeepalive(t, e, "workbuddy", a.UID)
	if r.Status != taskengine.StatusUncertain || a.Snapshot().AccessToken != "synthetic-relogin" {
		t.Fatal("stale refresh overwrote relogin")
	}
	runKeepalive(t, e, "workbuddy", a.UID)
	if calls.Load() != 1 {
		t.Fatal("conflicting refresh replayed")
	}
}

func TestKeepaliveDefinitiveSessionRejectionUsesExistingThreshold(t *testing.T) {
	root := t.TempDir()
	p := pool.New("")
	defer p.Close()
	a := keepaliveAccount(t, root, "synthetic-dead", "cn")
	p.Add(a)
	v, err := accountvault.Open(filepath.Join(root, "secure"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	adapter, err := NewAccounts(p, nil, "workbuddy", time.Hour, AccountsOptions{RefreshVault: v, Refresh: func(context.Context, *auth.Auth) (auth.TokenUpdate, error) {
		return auth.TokenUpdate{}, &upstream.TaskRefreshRejected{Code: 12153}
	}})
	if err != nil {
		t.Fatal(err)
	}
	e := keepaliveEngine(t, filepath.Join(root, "engine.json"), "workbuddy", adapter)
	defer e.Close()
	for n := 1; n <= pool.SessionDeadThreshold(); n++ {
		if r := runKeepalive(t, e, "workbuddy", a.UID); r.Status != taskengine.StatusManual {
			t.Fatal("definitive rejection misclassified")
		}
		st, _ := p.Status(a.UID)
		if st.Disabled != (n == pool.SessionDeadThreshold()) {
			t.Fatal("session threshold changed")
		}
	}
}

func TestKeepaliveUnchangedCredentialResponseDoesNotCacheForever(t *testing.T) {
	root := t.TempDir()
	p := pool.New("")
	defer p.Close()
	a := keepaliveAccount(t, root, "synthetic-unchanged", "cn")
	p.Add(a)
	v, err := accountvault.Open(filepath.Join(root, "secure"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var calls atomic.Int32
	adapter, err := NewAccounts(p, nil, "workbuddy", time.Hour, AccountsOptions{RefreshVault: v, Refresh: func(_ context.Context, a *auth.Auth) (auth.TokenUpdate, error) {
		calls.Add(1)
		return auth.TokenUpdate{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ExpiresAt: a.ExpiresAt}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	e := keepaliveEngine(t, filepath.Join(root, "engine.json"), "workbuddy", adapter)
	defer e.Close()
	for i := 0; i < 2; i++ {
		if runKeepalive(t, e, "workbuddy", a.UID).Status != taskengine.StatusSucceeded {
			t.Fatal("unchanged response failed verification")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("old response cached all future keepalive attempts")
	}
}

func TestKeepaliveDailyPlanUsesSelectedAccountsAndSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	p := pool.New("")
	defer p.Close()
	selected := keepaliveAccount(t, root, "synthetic-selected", "global")
	other := keepaliveAccount(t, root, "synthetic-other", "cn")
	p.Add(selected)
	p.Add(other)
	v, err := accountvault.Open(filepath.Join(root, "secure"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var calls atomic.Int32
	a, err := NewAccounts(p, nil, "workbuddy", time.Hour, AccountsOptions{RefreshVault: v, Refresh: func(_ context.Context, s *auth.Auth) (auth.TokenUpdate, error) {
		if s.UID != selected.UID {
			t.Error("schedule scope escaped")
		}
		calls.Add(1)
		return auth.TokenUpdate{AccessToken: "synthetic-scheduled", ExpiresAt: time.Now().Add(time.Hour).Unix()}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "engine.json")
	e := keepaliveEngine(t, path, "workbuddy", a)
	s, err := e.SaveSchedule(taskengine.Schedule{ID: "keepalive-plan", ProviderID: "workbuddy", TaskKey: "keepalive", AccountIDs: []string{selected.UID}, Hours: []int{1, 13}, Minute: 23, Timezone: "UTC", OncePerDay: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	due := *s.NextRun
	if err = e.Tick(due); err != nil {
		t.Fatal(err)
	}
	run := waitModuleRun(t, e, e.Runs()[0].ID)
	if run.Status != taskengine.StatusSucceeded || calls.Load() != 1 {
		t.Fatal("scheduled refresh failed")
	}
	e.Close()
	e = keepaliveEngine(t, path, "workbuddy", a)
	defer e.Close()
	if err = e.Tick(due.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(e.Runs()) != 1 || calls.Load() != 1 || other.Snapshot().AccessToken != "synthetic-original" {
		t.Fatal("restart duplicated scheduled refresh or expanded scope")
	}
	s = e.Schedules()[0]
	s.Enabled = false
	if _, err = e.SaveSchedule(s); err != nil {
		t.Fatal(err)
	}
	if err = e.Tick(due.Add(48 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("disabled plan dispatched")
	}
}
