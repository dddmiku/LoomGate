package taskproviders

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

type businessFixture struct {
	mu                                                       sync.Mutex
	counts                                                   map[string]int
	checked, claimed, failGrowth, brokenBalance, slowBalance bool
}

func (f *businessFixture) serve(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	f.mu.Lock()
	f.counts[r.URL.Path]++
	fail, broken, slow := f.failGrowth, f.brokenBalance, f.slowBalance
	checked, claimed := f.checked, f.claimed
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
		if claimed {
			state = "claimed"
		}
		data = map[string]any{"tasks": []any{map[string]any{"task_code": "synthetic-reward", "title": "合成任务", "accept_status": state, "reward_credit": 5}}}
	case "/activity/growth/tasks/synthetic-reward/claim":
		f.mu.Lock()
		f.claimed = true
		f.mu.Unlock()
		data = map[string]any{}
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
	deadline := time.NewTimer(5 * time.Second)
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
func TestGrowthClaimRequiresServerCompletionAndVerify(t *testing.T) {
	a, f, _ := setupModules(t)
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
			t.Fatal("reward readback failed")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/activity/growth/tasks/synthetic-reward/claim"] != 1 {
		t.Fatal("already claimed reward replayed")
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

type testModule struct {
	moduleBase
	panicDiscover bool
	inspect       func()
}

func (m *testModule) Discover(context.Context, *auth.Auth) ([]taskengine.Definition, error) {
	if m.panicDiscover {
		panic("synthetic-private-panic")
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
