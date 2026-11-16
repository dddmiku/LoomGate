package taskproviders

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream/trae"
)

type traeModuleStub struct {
	status, claim func(context.Context, *auth.Auth) (trae.CheckinStatus, error)
}

func (s *traeModuleStub) Status(ctx context.Context, a *auth.Auth) (trae.CheckinStatus, error) {
	return s.status(ctx, a)
}
func (s *traeModuleStub) Claim(ctx context.Context, a *auth.Auth) (trae.CheckinStatus, error) {
	return s.claim(ctx, a)
}
func (s *traeModuleStub) Credits(context.Context, *auth.Auth) (trae.Credits, error) {
	return trae.Credits{}, nil
}

type traeTaskFixture struct {
	mu                    sync.Mutex
	checked               bool
	mode                  string
	reads, claims, status int
	now                   time.Time
}

func (f *traeTaskFixture) serve(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	var value any
	switch {
	case strings.HasSuffix(r.URL.Path, "/claim"):
		f.claims++
		switch f.mode {
		case "rate":
			value = map[string]any{"code": 9074}
		case "device":
			value = map[string]any{"code": 9095}
		case "missing":
			value = map[string]any{}
		case "unknown":
			http.Error(w, "SYNTHETIC_PRIVATE_UPSTREAM", 503)
			return
		default:
			f.checked = true
			value = map[string]any{"code": 0, "credits": 100}
		}
	case strings.HasSuffix(r.URL.Path, "/status"):
		f.status++
		value = map[string]any{"code": 0, "enable": true, "checked_in": f.checked}
	default:
		f.reads++
		value = map[string]any{"code": 0, "usage_summary": map[string]any{"total_amount": 1000, "consumed_amount": 200}, "user_entitlement_pack_list": []any{map[string]any{"entitlement_base_info": map[string]any{"end_time": f.now.Add(time.Hour).Unix(), "product_extra": map[string]any{"package_extra": map[string]any{"quota": map[string]any{"credits_limit": 500}}}}, "usage": map[string]any{"credits_amount": 100}}}}
		if f.mode == "broken-balance" {
			value = map[string]any{"code": 0}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}
func traeTaskSetup(t *testing.T) (*Accounts, *traeTaskFixture, *pool.Pool, *accountvault.Vault, string, *time.Time) {
	t.Helper()
	now := time.Date(2099, 1, 3, 4, 0, 0, 0, time.UTC)
	f := &traeTaskFixture{now: now}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	p := pool.New("")
	t.Cleanup(p.Close)
	raw, _ := json.Marshal(map[string]any{"uid": "synthetic-trae", "realm": "trae", "domain": "api.trae.cn", "accessToken": "synthetic-access", "expiresAt": now.Add(time.Hour).Unix()})
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	p.Add(a)
	path := filepath.Join(t.TempDir(), "provider-state")
	v, err := accountvault.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	store, err := NewTraeTaskStore(v)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewAccounts(p, nil, "trae", 2*time.Hour, AccountsOptions{TraeStore: store, TraeClient: &trae.TaskClient{HTTP: srv.Client(), Origin: srv.URL}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return adapter, f, p, v, path, &now
}
func traeEngine(t *testing.T, a *Accounts, path string) *taskengine.Engine {
	t.Helper()
	e, err := taskengine.Open(taskengine.Options{Path: path, PollMin: time.Millisecond, PollMax: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Register("trae", a); err != nil {
		t.Fatal(err)
	}
	return e
}
func traeRun(t *testing.T, e *taskengine.Engine, key string) taskengine.Run {
	t.Helper()
	r, err := e.Submit(taskengine.Request{ProviderID: "trae", TaskKey: key, AccountIDs: []string{"synthetic-trae"}})
	if err != nil {
		t.Fatal(err)
	}
	return waitModuleRun(t, e, r.ID)
}
func TestTraeBalanceUpdatesPoolDuringPersistentCheckinBackoff(t *testing.T) {
	a, f, p, v, storePath, now := traeTaskSetup(t)
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := traeEngine(t, a, path)
	f.mode = "rate"
	run := traeRun(t, e, "checkin")
	if run.Status != taskengine.StatusManual {
		t.Fatal("typed 9074 refusal not surfaced")
	}
	found := false
	for _, s := range run.Steps {
		if s.Stage == taskengine.StageExecute {
			found = s.ActionOutcome == "rejected" && s.UpstreamCode != nil && *s.UpstreamCode == 9074
		}
	}
	if !found {
		t.Fatal("business refusal audit missing")
	}
	e.Close()
	v.Close()
	v, err := accountvault.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	store, err := NewTraeTaskStore(v)
	if err != nil {
		t.Fatal(err)
	}
	a.Modules[0].(*traeCheckinModule).store = store
	e = traeEngine(t, a, path)
	defer e.Close()
	if traeRun(t, e, "checkin").Status != taskengine.StatusManual {
		t.Fatal("restart lost cooldown")
	}
	if traeRun(t, e, "balance").Status != taskengine.StatusSucceeded {
		t.Fatal("balance blocked by signin")
	}
	status, _ := p.Status("synthetic-trae")
	if status.Credits != 800 || status.CreditsExpiring != 400 {
		t.Fatal("pool balance/expiry not updated")
	}
	f.mu.Lock()
	if f.claims != 1 || f.reads != 1 {
		t.Fatal("cooldown sent claims or blocked balance")
	}
	f.mode = "success"
	f.mu.Unlock()
	*now = now.Add(61 * time.Second)
	if traeRun(t, e, "checkin").Status != taskengine.StatusSucceeded {
		t.Fatal("explicit post-backoff run failed to recheck/claim/verify")
	}
	traeRun(t, e, "checkin")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims != 2 {
		t.Fatal("confirmed daily signin replayed")
	}
	raw, _ := os.ReadFile(filepath.Join(storePath, "trae-task-state.account"))
	if strings.Contains(string(raw), "synthetic-trae") || strings.Contains(string(raw), "2099-01-03") {
		t.Fatal("task state persisted in plaintext")
	}
}
func TestTraeUnknownAndDeviceResponsesNeverClaimCompletion(t *testing.T) {
	for _, mode := range []string{"unknown", "missing", "device"} {
		t.Run(mode, func(t *testing.T) {
			a, f, _, _, _, now := traeTaskSetup(t)
			f.mode = mode
			e := traeEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
			defer e.Close()
			want := taskengine.StatusUncertain
			if mode == "device" {
				want = taskengine.StatusManual
			}
			if traeRun(t, e, "checkin").Status != want {
				t.Fatal("unconfirmed/rejected claim falsely completed")
			}
			*now = now.Add(61 * time.Second)
			if traeRun(t, e, "checkin").Status != want {
				t.Fatal("unconfirmed claim guard lost")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.claims != 1 {
				t.Fatal("unconfirmed/device rejection replayed")
			}
		})
	}
}
func TestTraeMalformedBalanceCannotZeroPoolAndDisableIsPreserved(t *testing.T) {
	a, f, p, _, _, _ := traeTaskSetup(t)
	p.SetCreditsDetailed("synthetic-trae", 800, 400)
	f.mode = "broken-balance"
	e := traeEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	if traeRun(t, e, "balance").Status != taskengine.StatusFailed {
		t.Fatal("malformed balance was treated as zero success")
	}
	status, _ := p.Status("synthetic-trae")
	if status.Credits != 800 || status.CreditsExpiring != 400 {
		t.Fatal("malformed credits overwrote pool")
	}
	p.Disable("synthetic-trae", "manual")
	before := f.reads
	traeRun(t, e, "balance")
	status, _ = p.Status("synthetic-trae")
	if !status.Disabled || f.reads != before {
		t.Fatal("explicitly disabled account refreshed or revived")
	}
}

func TestTraeClockRolloverAndPersistentGlobalClaimGap(t *testing.T) {
	a, f, _, _, _, now := traeTaskSetup(t)
	e := traeEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	if traeRun(t, e, "checkin").Status != taskengine.StatusSucceeded {
		t.Fatal("initial day failed")
	}
	*now = now.Add(24 * time.Hour)
	f.mu.Lock()
	f.checked = false
	f.mu.Unlock()
	if traeRun(t, e, "checkin").Status != taskengine.StatusSucceeded {
		t.Fatal("new business day stayed cached")
	}
	module := a.Modules[0].(*traeCheckinModule)
	first := module.store.read("synthetic-trae")
	if first.CheckedDay != module.day() {
		t.Fatal("daily cache did not move with business day")
	}
	// The shared reservation, unlike per-account state, survives independent
	// account IDs and refuses an immediate second mutating request.
	err := module.store.change("synthetic-second", func(_ *traeTaskState, state *traeTaskStateData) error {
		if now.Before(state.NextClaim) {
			return errTraeClaimGap
		}
		return nil
	})
	if err != errTraeClaimGap {
		t.Fatal("global claim spacing lost")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims != 2 {
		t.Fatal("wrong number of actual accepted days")
	}
}
