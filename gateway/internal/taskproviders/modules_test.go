package taskproviders

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

type businessFixture struct {
	mu                                                                                                      sync.Mutex
	counts                                                                                                  map[string]int
	checked, claimed, accepted, notAccepted, workDone, rejectAccept, failGrowth, brokenBalance, slowBalance bool
	claimMalformed                                                                                          bool
	claimCredit, claimEnergy                                                                                int
}

func (f *businessFixture) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	r.Body.Close()
	f.mu.Lock()
	f.counts[r.URL.Path]++
	fail, broken, slow := f.failGrowth, f.brokenBalance, f.slowBalance
	checked, claimed, accepted, notAccepted, workDone := f.checked, f.claimed, f.accepted, f.notAccepted, f.workDone
	f.mu.Unlock()
	var data any
	switch r.URL.Path {
	case "/activity/growth/buddy/info":
		data = map[string]any{"buddy": nil}
	case "/activity/growth/streak":
		data = map[string]any{"launch_date": "2099-01-01", "streak": map[string]any{"days": 0, "makeup_dates": []string{}}, "makeup_cards": 0, "redemption_status": map[string]any{"tiers": []any{}}}
	case "/activity/growth/heatmap":
		data = map[string]any{"today": map[string]any{"date": "2099-01-02"}, "cells": []any{}}
	case "/activity/growth/energy", "/activity/growth/lottery/chances":
		data = map[string]any{"balance": 0}
	case "/activity/growth/buddy/quota":
		data = map[string]any{"affordable": 0, "max_open_count": 1}
	case "/billing/meter/compensation-status":
		data = map[string]any{"active": false, "claimed": false}
	case "/v2/activity/growth/tasks":
		if fail {
			http.Error(w, "synthetic-private-upstream", 503)
			return
		}
		state := "completed"
		if notAccepted {
			state = "not_accepted"
		}
		if accepted && !claimed {
			state = "accepted"
		}
		if claimed {
			state = "claimed"
		}
		current := 0
		if workDone {
			current = 1
		}
		data = map[string]any{"tasks": []any{map[string]any{"task_code": "synthetic-reward", "title": "合成任务", "accept_status": state, "reward_credit": 5, "progress": map[string]any{"current": current, "target": 1}}}}
	case "/activity/growth/tasks/accept":
		if f.rejectAccept {
			http.Error(w, `{"code":503,"msg":"synthetic accept outage"}`, 503)
			return
		}
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		codes, _ := body["task_codes"].([]any)
		results := []any{}
		for _, c := range codes {
			code, _ := c.(string)
			if code == "synthetic-reward" {
				f.mu.Lock()
				f.accepted = true
				f.mu.Unlock()
				results = append(results, map[string]any{"task_code": code, "status": "ok"})
			} else {
				results = append(results, map[string]any{"task_code": code, "status": "error", "message": "task not found"})
			}
		}
		data = map[string]any{"results": results}
	case "/activity/growth/tasks/synthetic-reward/claim":
		f.mu.Lock()
		f.claimed = true
		malformed, credit, energy := f.claimMalformed, f.claimCredit, f.claimEnergy
		f.mu.Unlock()
		if malformed {
			data = map[string]any{}
		} else {
			data = map[string]any{"credit": credit, "energy": energy, "already_claimed": false}
		}
	case "/v2/billing/meter/checkin-activity-status":
		data = map[string]any{"today_checked_in": checked, "active": true}
	case "/v2/billing/meter/daily-checkin":
		f.mu.Lock()
		f.checked = true
		f.mu.Unlock()
		data = map[string]any{}
	case "/v2/billing/meter/get-user-resource", "/billing/meter/get-user-resource":
		if slow {
			<-r.Context().Done()
			return
		}
		if broken {
			data = map[string]any{}
		} else {
			data = map[string]any{"Response": map[string]any{"Data": map[string]any{"Accounts": []any{map[string]any{"CapacityRemain": 900, "CycleEndTime": time.Now().Add(time.Hour).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05")}}}}}
		}
	default:
		http.Error(w, "unexpected_synthetic_path", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
}
func setupModules(t *testing.T) (*Accounts, *businessFixture, *pool.Pool) {
	t.Helper()
	f := &businessFixture{counts: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	p := pool.New("")
	t.Cleanup(p.Close)
	for _, x := range []struct{ id, realm string }{{"cn-fixture", "cn"}, {"global-fixture", "global"}, {"trae-fixture", "trae"}} {
		domain := map[string]string{"cn": "copilot.tencent.com", "global": "www.workbuddy.ai", "trae": "api.trae.cn"}[x.realm]
		data, _ := json.Marshal(map[string]any{"uid": x.id, "realm": x.realm, "domain": domain, "accessToken": "synthetic-access", "expiresAt": time.Now().Add(time.Hour).Unix()})
		account, err := auth.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		p.Add(account)
	}
	up := &upstream.Client{GlobalEnabled: true, HTTP: server.Client(), ChatBaseCN: server.URL, ChatBaseGlobal: server.URL, BillingBaseCN: server.URL, BillingBaseGlobal: server.URL}
	a, err := NewAccounts(p, up, "workbuddy", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return a, f, p
}
func waitModuleRun(t *testing.T, e *taskengine.Engine, id string) taskengine.Run {
	t.Helper()
	// Deadlock guard, not a latency assertion: each module run is bounded by
	// the engine's own timeout. Under full-suite parallelism on Windows, child
	// process startup and state fsync can take seconds; a tight wall-clock
	// bound here produced false failures while the same test passed serially.
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, r := range e.Runs() {
			if r.ID == id && r.FinishedAt != nil {
				return r
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("module run timed out")
		case <-tick.C:
		}
	}
}
func TestBalanceIsIndependentScopedReadAndUpdatesPool(t *testing.T) {
	a, f, p := setupModules(t)
	f.failGrowth = true
	e, err := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "tasks.json"), PollMin: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Register("workbuddy", a)
	rows, err := a.DiscoverCatalog(context.Background(), taskengine.Scope{}, "")
	if err != nil || len(rows.Issues) != 1 {
		t.Fatal("growth failure not isolated")
	}
	for _, task := range rows.Tasks {
		if task.AccountID == "global-fixture" && task.Key != "balance" {
			t.Fatal("global received task mutation")
		}
	}
	r, err := e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "balance", AccountIDs: []string{"global-fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	run := waitModuleRun(t, e, r.ID)
	if run.Status != taskengine.StatusSucceeded || len(run.DiscoveryIssues) != 0 {
		t.Fatal("isolated balance run failed")
	}
	status, _ := p.Status("global-fixture")
	if status.Credits != 900 || status.CreditsExpiring != 900 {
		t.Fatal("pool credits or expiry bucket not updated")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/billing/meter/get-user-resource"] != 1 || f.counts["/v2/billing/meter/daily-checkin"] != 0 {
		t.Fatalf("balance performed signin or extra fetch: %v", f.counts)
	}
}
func TestWorkBuddyGrowthPacingIsGlobalAndCancelable(t *testing.T) {
	a, _, p := setupModules(t)
	clock := time.Date(2099, 1, 2, 0, 0, 0, 0, time.UTC)
	a.pacingNow = func() time.Time { return clock }
	waits := []time.Duration{}
	a.pacingWait = func(ctx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		if err := ctx.Err(); err != nil {
			return err
		}
		clock = clock.Add(delay)
		return nil
	}
	secondRaw, _ := json.Marshal(map[string]any{"uid": "cn-second", "realm": "cn", "domain": "copilot.tencent.com", "accessToken": "synthetic-only"})
	second, err := auth.Parse(secondRaw)
	if err != nil {
		t.Fatal(err)
	}
	p.Add(second)
	firstTask := taskengine.Task{Definition: taskengine.Definition{ProviderID: "workbuddy", Key: "growth/fixture"}, Target: taskengine.Target{AccountID: "cn-fixture"}}
	secondTask := firstTask
	secondTask.AccountID = "cn-second"
	release, err := a.AcquireTask(context.Background(), firstTask)
	if err != nil {
		t.Fatal(err)
	}
	release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = a.AcquireTask(ctx, secondTask); !errors.Is(err, context.Canceled) {
		t.Fatal("paced queue ignored cancellation")
	}
	waits = nil
	release, err = a.AcquireTask(context.Background(), secondTask)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(waits) != 1 || waits[0] != 800*time.Millisecond {
		t.Fatal("different WorkBuddy accounts were admitted without the exact pacing gap")
	}
	waits = nil
	release, err = a.AcquireTask(context.Background(), secondTask)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(waits) != 0 {
		t.Fatal("same-account subtask incorrectly paid another account gap")
	}
}

func TestWildcardContinuesModulesButPersistsPartialFailure(t *testing.T) {
	a, f, p := setupModules(t)
	f.failGrowth = true
	e, err := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "tasks.json"), PollMin: time.Millisecond, PollMax: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Register("workbuddy", a)
	r, err := e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "*", AccountIDs: []string{"cn-fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	run := waitModuleRun(t, e, r.ID)
	if run.Status != taskengine.StatusFailed || len(run.DiscoveryIssues) != 1 || run.ErrorCode != "partial_discovery_failed" {
		t.Fatalf("partial discovery became success: %s", run.Status)
	}
	s, _ := p.Status("cn-fixture")
	if s.Credits != 900 {
		t.Fatal("independent balance skipped")
	}
	raw, _ := json.Marshal(run)
	if strings.Contains(string(raw), "private-upstream") {
		t.Fatal("raw discovery error leaked")
	}
}

func TestGrowthAcceptUnknownSurvivesRestartWithoutReplay(t *testing.T) {
	a, f, _ := setupModules(t)
	f.mu.Lock()
	f.notAccepted = true
	f.rejectAccept = true
	f.mu.Unlock()
	path := filepath.Join(t.TempDir(), "tasks.json")
	open := func() *taskengine.Engine {
		e, err := taskengine.Open(taskengine.Options{Path: path, PollMin: time.Millisecond, PollMax: 2 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if err = e.Register("workbuddy", a); err != nil {
			t.Fatal(err)
		}
		return e
	}
	submit := func(e *taskengine.Engine) taskengine.Run {
		r, err := e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "growth/accept/synthetic-reward", AccountIDs: []string{"cn-fixture"}})
		if err != nil {
			t.Fatal(err)
		}
		return waitModuleRun(t, e, r.ID)
	}
	e := open()
	if run := submit(e); run.Status != taskengine.StatusUncertain {
		t.Fatalf("lost accept response reported %s", run.Status)
	}
	e.Close()
	e = open()
	defer e.Close()
	if run := submit(e); run.Status != taskengine.StatusUncertain {
		t.Fatalf("restart cleared accept unknown %s", run.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/activity/growth/tasks/accept"] != 1 {
		t.Fatalf("accept replayed after restart: %d", f.counts["/activity/growth/tasks/accept"])
	}
}

func TestGrowthAcceptStartsTrackingButDoesNotClaimUnfinishedWork(t *testing.T) {
	a, f, _ := setupModules(t)
	f.mu.Lock()
	f.notAccepted = true
	f.mu.Unlock()
	e, _ := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "tasks.json"), PollMin: time.Millisecond, PollMax: time.Millisecond})
	defer e.Close()
	e.Register("workbuddy", a)
	submit := func(key string) taskengine.Run {
		r, err := e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: key, AccountIDs: []string{"cn-fixture"}})
		if err != nil {
			t.Fatal(err)
		}
		return waitModuleRun(t, e, r.ID)
	}
	if run := submit("growth/synthetic-reward"); run.Status != taskengine.StatusManual {
		t.Fatalf("claim before acceptance reported %s", run.Status)
	}
	if run := submit("growth/accept/synthetic-reward"); run.Status != taskengine.StatusSucceeded {
		t.Fatalf("official task accept reported %s", run.Status)
	}
	f.mu.Lock()
	if f.counts["/activity/growth/tasks/accept"] != 1 || f.counts["/activity/growth/tasks/synthetic-reward/claim"] != 0 {
		f.mu.Unlock()
		t.Fatal("accept/claim ordering violated")
	}
	// The official directory now reports progress 1/1 after real work while the
	// status still says accepted; the next run may claim exactly once.
	f.workDone = true
	f.notAccepted = false
	f.mu.Unlock()
	if run := submit("growth/synthetic-reward"); run.Status != taskengine.StatusSucceeded {
		t.Fatalf("completed growth task did not claim: %s", run.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/activity/growth/tasks/accept"] != 1 || f.counts["/activity/growth/tasks/synthetic-reward/claim"] != 1 {
		t.Fatalf("unexpected accept=%d claim=%d", f.counts["/activity/growth/tasks/accept"], f.counts["/activity/growth/tasks/synthetic-reward/claim"])
	}
}

func TestGrowthClaimRequiresServerCompletionAndVerify(t *testing.T) {
	a, f, _ := setupModules(t)
	f.claimCredit, f.claimEnergy = 37, 2
	e, _ := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "tasks.json"), PollMin: time.Millisecond, PollMax: time.Millisecond})
	defer e.Close()
	e.Register("workbuddy", a)
	for i := 0; i < 2; i++ {
		r, err := e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "growth/synthetic-reward", AccountIDs: []string{"cn-fixture"}})
		if err != nil {
			t.Fatal(err)
		}
		run := waitModuleRun(t, e, r.ID)
		if run.Status != taskengine.StatusSucceeded {
			t.Fatalf("reward readback failed: %s", run.Status)
		}
		if i == 0 {
			log, err := e.TaskLogFor("workbuddy", "growth/")
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Join(log.Lines, "\n")
			for _, want := range []string{"合成任务", "37 积分", "2 能量"} {
				if !strings.Contains(text, want) {
					t.Fatalf("actual award output missing %q: %s", want, text)
				}
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/activity/growth/tasks/synthetic-reward/claim"] != 1 {
		t.Fatal("already claimed reward replayed")
	}
}
func TestGrowthClaimReceiptMissingKeepsUnknownAndDoesNotRetry(t *testing.T) {
	a, f, _ := setupModules(t)
	f.claimMalformed = true
	e, err := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "tasks.json"), PollMin: time.Millisecond, PollMax: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err = e.Register("workbuddy", a); err != nil {
		t.Fatal(err)
	}
	r, err := e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "growth/synthetic-reward", AccountIDs: []string{"cn-fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	run := waitModuleRun(t, e, r.ID)
	if run.Status != taskengine.StatusUncertain {
		t.Fatalf("missing receipt reported %s", run.Status)
	}
	if f.counts["/activity/growth/tasks/synthetic-reward/claim"] != 1 {
		t.Fatal("unconfirmed reward claim was repeated")
	}
	log, err := e.TaskLogFor("workbuddy", "growth/")
	if err != nil || !strings.Contains(strings.Join(log.Lines, "\n"), "合成任务") {
		t.Fatal("unknown claim log lost its discovered task name")
	}
}

func TestBalanceMalformedCancellationDisabledAndForeignScope(t *testing.T) {
	a, f, p := setupModules(t)
	p.SetCreditsDetailed("cn-fixture", 777, 7)
	task := taskengine.Task{Definition: taskengine.Definition{Key: "balance", ProviderID: "workbuddy"}, Target: taskengine.Target{AccountID: "cn-fixture"}}
	f.brokenBalance = true
	if _, err := a.Inspect(context.Background(), task); err == nil {
		t.Fatal("absent balance treated as zero")
	}
	status, _ := p.Status("cn-fixture")
	if status.Credits != 777 {
		t.Fatal("bad response overwrote balance")
	}
	f.mu.Lock()
	f.brokenBalance = false
	f.slowBalance = true
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := a.Inspect(ctx, task); err == nil {
		t.Fatal("cancellation ignored")
	}
	p.Disable("cn-fixture", "synthetic-disabled")
	if _, err := a.Inspect(context.Background(), task); err == nil {
		t.Fatal("disabled account used")
	}
	task.AccountID = "global-fixture"
	task.Key = "checkin"
	if _, err := a.Execute(context.Background(), task); err == nil {
		t.Fatal("global mutation admitted")
	}
	task.AccountID = "trae-fixture"
	task.Key = "balance"
	if _, err := a.Inspect(context.Background(), task); err == nil {
		t.Fatal("foreign provider admitted")
	}
}

type catalogProbeModule struct {
	moduleBase
	started   chan<- string
	active    *atomic.Int32
	maxActive *atomic.Int32
	wait      bool
}

func (m *catalogProbeModule) Discover(ctx context.Context, _ *auth.Auth) ([]taskengine.Definition, error) {
	if m.active != nil {
		current := m.active.Add(1)
		for {
			old := m.maxActive.Load()
			if current <= old || m.maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		defer m.active.Add(-1)
	}
	if m.started != nil {
		m.started <- m.Spec().ID
	}
	if m.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []taskengine.Definition{m.definition}, nil
}
func (m *catalogProbeModule) Inspect(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error) {
	return taskengine.Observation{Phase: "ready"}, nil
}
func (m *catalogProbeModule) Execute(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error) {
	return taskengine.Observation{Phase: "pending"}, nil
}
func (m *catalogProbeModule) Claim(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error) {
	return taskengine.Observation{}, nil
}
func (m *catalogProbeModule) Verify(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error) {
	return taskengine.Observation{Phase: "done"}, nil
}

func TestDiscoverCatalogConcurrentBoundedAndIsolatesSlowModules(t *testing.T) {
	a, _, _ := setupModules(t)
	started := make(chan string, 8)
	var active, maxActive atomic.Int32
	makeModule := func(id, key string, slow bool) Module {
		spec := ModuleSpec{ID: id, Key: key, Realms: []string{"cn"}}
		return &catalogProbeModule{moduleBase: moduleBase{spec: spec, definition: taskengine.Definition{Key: key, Name: key}}, started: started, active: &active, maxActive: &maxActive, wait: slow}
	}
	a.Modules = []Module{makeModule("slow-a", "slow-a", true), makeModule("fast", "fast", false), makeModule("slow-b", "slow-b", true)}
	a.ModuleTimeout = 35 * time.Millisecond
	a.DiscoveryConcurrency = 2
	begin := time.Now()
	catalog, err := a.DiscoverCatalog(context.Background(), taskengine.Scope{AccountIDs: []string{"cn-fixture"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed > 250*time.Millisecond {
		t.Fatalf("discovery waited serially: %v", elapsed)
	}
	if maxActive.Load() != 2 {
		t.Fatalf("observed concurrency=%d want 2", maxActive.Load())
	}
	if len(catalog.Tasks) != 1 || catalog.Tasks[0].Key != "fast" {
		t.Fatalf("fast result lost: %+v", catalog.Tasks)
	}
	if len(catalog.Issues) != 2 || catalog.Issues[0].ModuleID != "slow-a" || catalog.Issues[1].ModuleID != "slow-b" {
		t.Fatalf("slow module issues lost or reordered: %+v", catalog.Issues)
	}
}

func TestDiscoverCatalogParentCancellationStopsQueuedWork(t *testing.T) {
	a, _, _ := setupModules(t)
	started := make(chan string, 8)
	var active, maxActive atomic.Int32
	makeModule := func(id, key string) Module {
		spec := ModuleSpec{ID: id, Key: key, Realms: []string{"cn"}}
		return &catalogProbeModule{moduleBase: moduleBase{spec: spec, definition: taskengine.Definition{Key: key, Name: key}}, started: started, active: &active, maxActive: &maxActive, wait: true}
	}
	a.Modules = []Module{makeModule("slow-a", "slow-a"), makeModule("slow-b", "slow-b"), makeModule("queued", "queued")}
	a.DiscoveryConcurrency = 1
	a.ModuleTimeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.DiscoverCatalog(ctx, taskengine.Scope{AccountIDs: []string{"cn-fixture"}}, "")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("discovery did not stop on parent cancellation")
	}
	select {
	case got := <-started:
		t.Fatalf("queued discovery started after cancellation: %s", got)
	default:
	}
}

type testModule struct {
	moduleBase
	panicDiscover bool
	inspect       func()
	discover      func(context.Context, *auth.Auth) ([]taskengine.Definition, error)
}

func (m *testModule) Discover(ctx context.Context, account *auth.Auth) ([]taskengine.Definition, error) {
	if m.panicDiscover {
		panic("synthetic-private-panic")
	}
	if m.discover != nil {
		return m.discover(ctx, account)
	}
	return []taskengine.Definition{m.definition}, nil
}
func (m *testModule) Inspect(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error) {
	if m.inspect != nil {
		m.inspect()
	}
	return taskengine.Observation{Phase: "done"}, nil
}
func (m *testModule) Verify(c context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(c, a, t)
}
func TestAdditionalModuleNeedsNoCoreChangeAndPanicIsolated(t *testing.T) {
	a, _, _ := setupModules(t)
	calls := 0
	a.Modules = append(a.Modules, &testModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "extra", Key: "extra", Realms: []string{"cn"}}, definition: taskengine.Definition{Key: "extra", Name: "Synthetic extra"}}, inspect: func() { calls++ }}, &testModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "fault", Key: "fault", Realms: []string{"cn"}}}, panicDiscover: true})
	catalog, err := a.DiscoverCatalog(context.Background(), taskengine.Scope{AccountIDs: []string{"cn-fixture"}}, "")
	if err != nil || len(catalog.Issues) != 1 {
		t.Fatal("module panic poisoned catalog")
	}
	for _, task := range catalog.Tasks {
		if task.Key == "extra" {
			a.Inspect(context.Background(), task)
		}
	}
	if calls != 1 {
		t.Fatal("additional module not dispatched")
	}
	a.Modules = append(a.Modules, a.Modules[0])
	if _, err := a.Discover(context.Background(), taskengine.Scope{}); err == nil {
		t.Fatal("conflicting module routes accepted")
	}
}
func TestTraeModuleKeepsItsOwnContract(t *testing.T) {
	a, _, p := setupModules(t)
	traeAdapter, err := NewAccounts(p, a.Upstream, "trae", 0)
	if err != nil {
		t.Fatal(err)
	}
	module := traeAdapter.Modules[0].(*traeCheckinModule)
	checked := false
	module.api = &traeModuleStub{status: func(context.Context, *auth.Auth) (trae.CheckinStatus, error) {
		return trae.CheckinStatus{Enabled: true, CheckedIn: checked}, nil
	}, claim: func(context.Context, *auth.Auth) (trae.CheckinStatus, error) {
		checked = true
		return trae.CheckinStatus{CheckedIn: true}, nil
	}}
	catalog, err := traeAdapter.DiscoverCatalog(context.Background(), taskengine.Scope{}, "checkin")
	if err != nil || len(catalog.Tasks) != 1 || catalog.Tasks[0].AccountID != "trae-fixture" {
		t.Fatal("Trae scope changed")
	}
	task := catalog.Tasks[0]
	o, err := traeAdapter.Execute(context.Background(), task)
	if err != nil || o.Phase != "pending" {
		t.Fatal("Trae execute failed")
	}
	o, err = traeAdapter.Verify(context.Background(), task)
	if err != nil || o.Phase != "done" {
		t.Fatal("Trae verify failed")
	}
}
