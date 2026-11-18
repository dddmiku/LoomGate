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
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

type lifecycleFixture struct {
	mu                                                 sync.Mutex
	day, travel, tier, offerEnd                        string
	record                                             int64
	daily, claimed, noBuddy, reject, silent, malformed bool
	refuseDrawOnce                                     bool
	cards                                              int
	chances, buddyAffordable                           int
	made                                               []string
	afterWrite                                         func()
	counts                                             map[string]int
	writeBodies                                        map[string]map[string]any
}

func (f *lifecycleFixture) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	r.Body.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	f.counts[path]++
	mutation := r.Method == "POST" && !strings.Contains(path, "status")
	if mutation {
		var body map[string]any
		json.Unmarshal(raw, &body)
		f.writeBodies[path] = body
		if f.reject {
			http.Error(w, "SYNTHETIC_PRIVATE_ERROR", 503)
			return
		}
	}
	var data any
	switch path {
	case "/activity/growth/buddy/info":
		data = map[string]any{"buddy": map[string]any{"id": 17, "name": "Synthetic Buddy"}}
		if f.noBuddy {
			data = map[string]any{"buddy": nil}
		}
	case "/activity/growth/buddy/travel/status":
		data = map[string]any{"state": f.travel, "daily_limit_reached": f.daily, "record_id": f.record, "reward_credit": 7}
	case "/activity/growth/buddy/travel/config":
		data = map[string]any{"locations": []any{map[string]any{"id": 73, "name": "合成目的地", "duration_hours_min": 1, "duration_hours_max": 2}}}
	case "/activity/growth/buddy/travel/depart":
		if !f.silent {
			f.travel = "traveling"
			f.daily = true
			f.record = 412
		}
		data = map[string]any{}
	case "/activity/growth/buddy/travel/claim":
		if !f.silent {
			f.travel = "idle"
			f.record = 0
		}
		data = map[string]any{"reward_credit": 7}
	case "/activity/growth/streak":
		data = map[string]any{"launch_date": "2099-01-01", "streak": map[string]any{"days": 12, "makeup_dates": f.made}, "makeup_cards": map[string]any{"balance": f.cards}, "redemption_status": map[string]any{"tiers": []any{map[string]any{"tier": "future-plan", "days": 9, "credit": 23}}, "tier_future-plan_status": f.tier}}
	case "/activity/growth/heatmap":
		data = map[string]any{"today": map[string]any{"date": f.day}, "cells": []any{map[string]any{"date": "2098-12-31", "score": 0}, map[string]any{"date": "2099-01-01", "score": 0}, map[string]any{"date": "2099-01-02", "score": 0}, map[string]any{"date": f.day, "score": 1}}}
	case "/activity/growth/redeem":
		if !f.silent {
			f.tier = "claimed"
		}
		data = map[string]any{"credit_granted": 23}
	case "/activity/growth/makeup-cards/use":
		if !f.silent {
			f.cards--
			f.made = append(f.made, "2099-01-02")
		}
		data = map[string]any{"makeup_cards": f.cards}
	case "/activity/growth/energy":
		data = map[string]any{"balance": 11}
	case "/activity/growth/lottery/chances":
		data = map[string]any{"balance": f.chances}
	case "/activity/growth/lottery/draw":
		if !f.silent {
			if f.refuseDrawOnce {
				f.refuseDrawOnce = false
				http.Error(w, `{"code":400,"msg":"insufficient lottery chance balance"}`, 400)
				return
			}
			if f.chances <= 0 {
				http.Error(w, `{"code":400,"msg":"insufficient lottery chance balance"}`, 400)
				return
			}
			f.chances--
			data = map[string]any{"prize_code": "synthetic-prize", "prize_name": "合成奖品", "prize_type": "credit", "credit_amount": 5}
		} else {
			data = map[string]any{"prize_code": "synthetic-prize"}
		}
	case "/activity/growth/buddy/quota":
		data = map[string]any{"affordable": f.buddyAffordable, "max_open_count": 1}
	case "/activity/growth/buddy/open":
		if !f.silent {
			if f.buddyAffordable <= 0 {
				http.Error(w, `{"code":400,"msg":"insufficient energy"}`, 400)
				return
			}
			f.buddyAffordable--
			data = map[string]any{"buddy": "合成Buddy"}
		}
	case "/billing/meter/compensation-status":
		data = map[string]any{"active": true, "claimed": f.claimed, "credit_num": 31, "end_time": f.offerEnd}
	case "/billing/meter/claim-compensation":
		if !f.silent {
			f.claimed = true
		}
		data = map[string]any{}
	default:
		http.Error(w, "unexpected_fixture_path", 404)
		return
	}
	if f.malformed && !mutation {
		data = map[string]any{}
	}
	if mutation && f.afterWrite != nil {
		f.afterWrite()
		f.afterWrite = nil
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
}
func lifecycleSetup(t *testing.T) (*Accounts, *lifecycleFixture, *pool.Pool) {
	t.Helper()
	f := &lifecycleFixture{day: "2099-01-03", travel: "idle", tier: "available", offerEnd: "2099-02-01 00:00:00", cards: 3, chances: 4, buddyAffordable: 2, made: []string{}, counts: map[string]int{}, writeBodies: map[string]map[string]any{}}
	s := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(s.Close)
	p := pool.New("")
	t.Cleanup(p.Close)
	state, err := accountvault.Open(filepath.Join(t.TempDir(), "task-state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	for _, realm := range []string{"cn", "global", "trae"} {
		raw, _ := json.Marshal(map[string]any{"uid": "synthetic-" + realm, "realm": realm, "domain": map[string]string{"cn": "copilot.tencent.com", "global": "www.workbuddy.ai", "trae": "api.trae.cn"}[realm], "accessToken": "synthetic-only", "expiresAt": time.Now().Add(time.Hour).Unix()})
		a, e := auth.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		p.Add(a)
	}
	up := &upstream.Client{HTTP: s.Client(), ChatBaseCN: s.URL, BillingBaseCN: s.URL}
	a, e := NewAccounts(p, up, "workbuddy", time.Hour, AccountsOptions{SpendVault: state})
	if e != nil {
		t.Fatal(e)
	}
	// Exercise the production registered lifecycle modules without legacy endpoints.
	a.Modules = a.Modules[3:]
	return a, f, p
}
func lifecycleEngine(t *testing.T, a *Accounts, path string) *taskengine.Engine {
	t.Helper()
	e, err := taskengine.Open(taskengine.Options{Path: path, PollMin: time.Millisecond, PollMax: 2 * time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Register("workbuddy", a); err != nil {
		t.Fatal(err)
	}
	return e
}
func runLifecycle(t *testing.T, e *taskengine.Engine, key string) taskengine.Run {
	t.Helper()
	r, err := e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: key, AccountIDs: []string{"synthetic-cn"}})
	if err != nil {
		t.Fatal(err)
	}
	return waitModuleRun(t, e, r.ID)
}
func TestLifecycleModulesDynamicDiscoveryAndVerifiedMutations(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	catalog, err := e.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{}, "")
	if err != nil || len(catalog.Issues) != 0 || len(catalog.Tasks) != 8 {
		t.Fatal("new lifecycle directory incomplete")
	}
	for _, task := range catalog.Tasks {
		if task.AccountID != "synthetic-cn" || task.State == nil {
			t.Fatal("foreign realm or missing authoritative state")
		}
	}
	for _, key := range []string{"travel/depart", "redeem/future-plan", "makeup", "resources", "compensation"} {
		if run := runLifecycle(t, e, key); run.Status != taskengine.StatusSucceeded {
			t.Fatalf("task %s failed: %s", key, run.Status)
		}
	}
	f.mu.Lock()
	if f.writeBodies["/activity/growth/buddy/travel/depart"]["location_id"] != float64(73) || f.writeBodies["/activity/growth/redeem"]["tier"] != "future-plan" || f.writeBodies["/activity/growth/makeup-cards/use"]["target_date"] != "2099-01-02" || f.cards != 2 {
		t.Fatal("dynamic operation parameters or one-card limit lost")
	}
	f.travel = "arrived"
	f.mu.Unlock()
	if run := runLifecycle(t, e, "travel/claim"); run.Status != taskengine.StatusSucceeded {
		t.Fatal("travel reward did not verify settlement")
	}
	f.mu.Lock()
	if f.writeBodies["/activity/growth/buddy/travel/claim"]["record_id"] != float64(412) {
		t.Fatal("claim lost observed record")
	}
	before := len(f.writeBodies)
	f.mu.Unlock()
	for _, key := range []string{"travel/depart", "redeem/future-plan", "compensation"} {
		runLifecycle(t, e, key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writeBodies) != before || f.counts["/activity/growth/redeem"] != 1 || f.counts["/billing/meter/claim-compensation"] != 1 || f.counts["/activity/growth/buddy/travel/depart"] != 1 {
		t.Fatal("completed operation replayed")
	}
}
func TestLifecycleUnknownMutationSurvivesPeriodChangesAndRestart(t *testing.T) {
	for _, key := range []string{"travel/depart", "redeem/future-plan", "makeup", "compensation"} {
		t.Run(key, func(t *testing.T) {
			a, f, _ := lifecycleSetup(t)
			f.reject = true
			path := filepath.Join(t.TempDir(), "tasks.json")
			e := lifecycleEngine(t, a, path)
			if run := runLifecycle(t, e, key); run.Status != taskengine.StatusUncertain {
				t.Fatal("rejected submission discarded uncertainty")
			}
			e.Close()
			f.mu.Lock()
			f.day = "2099-02-03"
			f.offerEnd = "2099-03-01 00:00:00"
			before := map[string]int{}
			for k, v := range f.counts {
				before[k] = v
			}
			f.mu.Unlock()
			e = lifecycleEngine(t, a, path)
			defer e.Close()
			catalog, err := e.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{AccountIDs: []string{"synthetic-cn"}}, key)
			if err != nil || len(catalog.Tasks) != 1 || !catalog.Tasks[0].PendingVerification {
				t.Fatal("old instance disappeared after directory period changed")
			}
			if run := runLifecycle(t, e, key); run.Status != taskengine.StatusUncertain {
				t.Fatal("restart permitted another instance before resolving old write")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for path := range f.writeBodies {
				if f.counts[path] != before[path] {
					t.Fatal("unknown mutation replayed after period change")
				}
			}
		})
	}
}
func TestLifecycleHTTP200WithoutStateChangeNeverCompletes(t *testing.T) {
	for _, key := range []string{"travel/depart", "redeem/future-plan", "makeup", "compensation"} {
		t.Run(key, func(t *testing.T) {
			a, f, _ := lifecycleSetup(t)
			f.silent = true
			e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
			defer e.Close()
			if r := runLifecycle(t, e, key); r.Status != taskengine.StatusUncertain {
				t.Fatal("HTTP200 without authoritative state was accepted")
			}
		})
	}
}
func TestLifecycleSchemaRealmAndDisableGuards(t *testing.T) {
	a, f, p := lifecycleSetup(t)
	f.malformed = true
	catalog, err := a.DiscoverCatalog(context.Background(), taskengine.Scope{}, "")
	if err != nil || len(catalog.Issues) != 7 || len(catalog.Tasks) != 0 {
		t.Fatal("schema failure produced task eligibility")
	}
	f.malformed = false
	for _, id := range []string{"synthetic-global", "synthetic-trae"} {
		_, err := a.Execute(context.Background(), taskengine.Task{Definition: taskengine.Definition{ProviderID: "workbuddy", Key: "travel/depart", PeriodKey: f.day}, Target: taskengine.Target{AccountID: id}})
		if err == nil {
			t.Fatal("foreign account crossed module realm")
		}
	}
	catalog, err = a.DiscoverCatalog(context.Background(), taskengine.Scope{AccountIDs: []string{"synthetic-cn"}}, "travel/depart")
	if err != nil {
		t.Fatal(err)
	}
	p.Disable("synthetic-cn", "manual")
	for _, task := range catalog.Tasks {
		if task.Key == "travel/depart" {
			if _, err = a.Execute(context.Background(), task); err == nil {
				t.Fatal("disabled account executed")
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writeBodies) != 0 {
		t.Fatal("guard caused upstream mutation")
	}
}

func TestLifecycleScheduleConfigurationSurvivesRestartAndDispatchesOnce(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := lifecycleEngine(t, a, path)
	schedule, err := e.SaveSchedule(taskengine.Schedule{ID: "synthetic-travel-plan", ProviderID: "workbuddy", TaskKey: "travel/depart", AccountIDs: []string{"synthetic-cn"}, Hours: []int{9, 21}, Minute: 17, Timezone: "Asia/Shanghai", Enabled: false, OncePerDay: true})
	if err != nil || schedule.NextRun != nil {
		t.Fatal("disabled schedule was armed")
	}
	e.Close()
	e = lifecycleEngine(t, a, path)
	defer e.Close()
	stored := e.Schedules()
	if len(stored) != 1 || stored[0].Minute != 17 || stored[0].Timezone != "Asia/Shanghai" || stored[0].AccountIDs[0] != "synthetic-cn" || stored[0].Enabled {
		t.Fatal("schedule settings changed after restart")
	}
	schedule = stored[0]
	schedule.Enabled = true
	schedule, err = e.SaveSchedule(schedule)
	if err != nil || schedule.NextRun == nil {
		t.Fatal("enabled schedule not armed")
	}
	due := *schedule.NextRun
	if err = e.Tick(due); err != nil {
		t.Fatal(err)
	}
	runs := e.Runs()
	if len(runs) != 1 {
		t.Fatal("due plan not dispatched")
	}
	if waitModuleRun(t, e, runs[0].ID).Status != taskengine.StatusSucceeded {
		t.Fatal("scheduled real module did not complete")
	}
	e.Close()
	e = lifecycleEngine(t, a, path)
	defer e.Close()
	if err = e.Tick(due); err != nil {
		t.Fatal(err)
	}
	if len(e.Runs()) != 1 {
		t.Fatal("persisted scheduled instant replayed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/activity/growth/buddy/travel/depart"] != 1 {
		t.Fatal("scheduler double dispatched mutation")
	}
}

func TestLifecycleUnknownTaskMissingFromDirectoryRemainsVisible(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	f.reject = true
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := lifecycleEngine(t, a, path)
	if runLifecycle(t, e, "travel/depart").Status != taskengine.StatusUncertain {
		t.Fatal("expected unknown departure")
	}
	e.Close()
	f.mu.Lock()
	f.noBuddy = true
	f.mu.Unlock()
	e = lifecycleEngine(t, a, path)
	defer e.Close()
	catalog, err := e.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{AccountIDs: []string{"synthetic-cn"}}, "travel/depart")
	if err != nil || len(catalog.Tasks) != 1 || catalog.Tasks[0].Key != "travel/depart" || !catalog.Tasks[0].PendingVerification {
		t.Fatal("vanished upstream instance hid pending operation")
	}
	if runLifecycle(t, e, "travel/depart").Status != taskengine.StatusUncertain {
		t.Fatal("vanished task replayed")
	}
	other, err := e.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{AccountIDs: []string{"synthetic-global"}}, "travel/depart")
	if err != nil || len(other.Tasks) != 0 {
		t.Fatal("pending record crossed account scope")
	}
}

func TestLifecycleCancellationAndManualAdoptionDoNotReplayOrPoisonWorker(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	started, closed := make(chan struct{}), make(chan struct{})
	transport := a.Upstream.HTTP.Transport
	a.Upstream.HTTP = &http.Client{Transport: lifecycleRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/billing/meter/claim-compensation" {
			close(started)
			<-r.Context().Done()
			close(closed)
			return nil, r.Context().Err()
		}
		return transport.RoundTrip(r)
	})}
	e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	run, err := e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "compensation", AccountIDs: []string{"synthetic-cn"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("claim did not start")
	}
	if !e.Cancel(run.ID) {
		t.Fatal("cancel was not accepted")
	}
	select {
	case <-closed:
	case <-time.After(20 * time.Second):
		t.Fatal("cancel not propagated")
	}
	if waitModuleRun(t, e, run.ID).Status != taskengine.StatusUncertain {
		t.Fatal("canceled mutation falsely confirmed")
	}
	if runLifecycle(t, e, "compensation").Status != taskengine.StatusUncertain {
		t.Fatal("unknown operation replayed")
	}
	if runLifecycle(t, e, "resources").Status != taskengine.StatusSucceeded {
		t.Fatal("canceled module held account worker")
	}
	f.mu.Lock()
	f.noBuddy = true
	f.mu.Unlock()
	if runLifecycle(t, e, "travel/adopt").Status != taskengine.StatusManual {
		t.Fatal("agreement or adoption was fabricated")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writeBodies) != 0 {
		t.Fatal("manual adoption or readback performed a write")
	}
}

type lifecycleRoundTrip func(*http.Request) (*http.Response, error)

func (f lifecycleRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLifecycleSuccessfulCyclesFollowServerDayAndMonth(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	for _, key := range []string{"travel/depart", "redeem/future-plan"} {
		if runLifecycle(t, e, key).Status != taskengine.StatusSucceeded {
			t.Fatal("first cycle failed")
		}
	}
	f.mu.Lock()
	first := f.writeBodies["/activity/growth/redeem"]["client_token"]
	f.day = "2099-02-03"
	f.travel = "idle"
	f.record = 0
	f.daily = false
	f.tier = "available"
	f.mu.Unlock()
	for _, key := range []string{"travel/depart", "redeem/future-plan"} {
		if runLifecycle(t, e, key).Status != taskengine.StatusSucceeded {
			t.Fatal("new server cycle did not become available")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/activity/growth/buddy/travel/depart"] != 2 || f.counts["/activity/growth/redeem"] != 2 || f.writeBodies["/activity/growth/redeem"]["client_token"] == first {
		t.Fatal("new day/month reused old task or operation identity")
	}
}

func TestTravelDisappearedRecordWithoutAcceptedReceiptCannotClearUnknown(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	f.travel = "arrived"
	f.record = 812
	f.daily = false
	f.reject = true
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := lifecycleEngine(t, a, path)
	if runLifecycle(t, e, "travel/claim").Status != taskengine.StatusUncertain {
		t.Fatal("claim failure did not retain unknown")
	}
	e.Close()
	f.mu.Lock()
	f.travel = "idle"
	f.record = 0
	f.mu.Unlock()
	e = lifecycleEngine(t, a, path)
	defer e.Close()
	if runLifecycle(t, e, "travel/claim").Status != taskengine.StatusUncertain {
		t.Fatal("missing record was incorrectly called reward success")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/activity/growth/buddy/travel/claim"] != 1 {
		t.Fatal("unknown claim replayed")
	}
}

func TestTravelPreviousDayArrivalCanSettleWithVerifiedReceipt(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	f.travel = "arrived"
	f.record = 813
	f.daily = false
	e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	if runLifecycle(t, e, "travel/claim").Status != taskengine.StatusSucceeded {
		t.Fatal("valid claim could not settle when today's departure quota is unused")
	}
}

func TestAcceptedTravelReceiptPersistsUntilDelayedSettlement(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	f.travel = "arrived"
	f.record = 814
	f.silent = true
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := lifecycleEngine(t, a, path)
	if runLifecycle(t, e, "travel/claim").Status != taskengine.StatusUncertain {
		t.Fatal("unsettled accepted claim should wait")
	}
	e.Close()
	f.mu.Lock()
	f.travel = "idle"
	f.record = 0
	f.mu.Unlock()
	e = lifecycleEngine(t, a, path)
	defer e.Close()
	if runLifecycle(t, e, "travel/claim").Status != taskengine.StatusSucceeded {
		t.Fatal("persisted accepted receipt did not reconcile settled record")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/activity/growth/buddy/travel/claim"] != 1 {
		t.Fatal("settled receipt replayed claim")
	}
}


// spendJournalForTest reads the durable spend record through the registered
// production module, proving the test exercises the real vault path.
func spendJournalForTest(t *testing.T, a *Accounts, uid, kind string) spendJournal {
	t.Helper()
	for _, m := range a.Modules {
		var base *spendRewardModule
		switch x := m.(type) {
		case *lotteryModule:
			base = &x.spendRewardModule
		case *buddyBoxModule:
			base = &x.spendRewardModule
		}
		if base != nil && base.Spec().Key == kind {
			auth := a.Pool.AuthByUID(uid).Snapshot()
			j, _, err := base.loadSpend(auth, kind)
			if err != nil {
				t.Fatalf("load spend journal: %v", err)
			}
			return j
		}
	}
	t.Fatalf("spend module %s not registered", kind)
	return spendJournal{}
}


func TestRewardModuleExplicitRefusalDoesNotBlockLaterChance(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	f.mu.Lock()
	f.chances = 1
	f.refuseDrawOnce = true
	f.mu.Unlock()
	e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	first := runLifecycle(t, e, "lottery")
	if first.Status != taskengine.StatusSkipped {
		t.Fatalf("explicit refusal reported %s", first.Status)
	}
	f.mu.Lock()
	f.chances = 2
	f.mu.Unlock()
	if run := runLifecycle(t, e, "lottery"); run.Status != taskengine.StatusSucceeded {
		t.Fatalf("refused attempt blocked a later chance: %s", run.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/activity/growth/lottery/draw"] != 2 {
		t.Fatalf("expected one refused and one accepted draw, got %d", f.counts["/activity/growth/lottery/draw"])
	}
}

func TestRewardModulesBindStableTokenAndPersistAcceptedBaseline(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	if run := runLifecycle(t, e, "lottery"); run.Status != taskengine.StatusSucceeded {
		t.Fatalf("lottery result %s", run.Status)
	}
	f.mu.Lock()
	token, _ := f.writeBodies["/activity/growth/lottery/draw"]["client_token"].(string)
	f.mu.Unlock()
	if !strings.HasPrefix(token, "lottery-") || len(token) != len("lottery-")+32 {
		t.Fatalf("client_token not bound to the attempt: %q", token)
	}
	j := spendJournalForTest(t, a, "synthetic-cn", "lottery")
	if j.State != "accepted" || j.ClientToken != token || j.After != j.Before-1 || j.Reference == "" {
		t.Fatalf("durable receipt incomplete: %+v", j)
	}
}

func TestRewardModuleLostResponseSurvivesRestartWithoutReplay(t *testing.T) {
	for _, key := range []string{"lottery", "buddy_box"} {
		t.Run(key, func(t *testing.T) {
			a, f, _ := lifecycleSetup(t)
			f.silent = true
			path := filepath.Join(t.TempDir(), "tasks.json")
			e := lifecycleEngine(t, a, path)
			if run := runLifecycle(t, e, key); run.Status != taskengine.StatusUncertain {
				t.Fatalf("lost response reported %s", run.Status)
			}
			e.Close()
			e = lifecycleEngine(t, a, path)
			defer e.Close()
			if run := runLifecycle(t, e, key); run.Status != taskengine.StatusUncertain {
				t.Fatalf("restart cleared unknown %s", run.Status)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.counts["/activity/growth/lottery/draw"] != 1 && key == "lottery" {
				t.Fatalf("lottery replayed %d times", f.counts["/activity/growth/lottery/draw"])
			}
			if f.counts["/activity/growth/buddy/open"] != 1 && key == "buddy_box" {
				t.Fatalf("buddy box replayed %d times", f.counts["/activity/growth/buddy/open"])
			}
		})
	}
}

func TestRewardModuleNoChanceDoesNotPersistACompletedDay(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	f.mu.Lock()
	f.chances = 0
	f.mu.Unlock()
	e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	if run := runLifecycle(t, e, "lottery"); run.Status == taskengine.StatusSucceeded {
		t.Fatal("no-chance state reported completed")
	}
	if _, _, err := func() (spendJournal, uint64, error) {
		for _, m := range a.Modules {
			if x, ok := m.(*lotteryModule); ok {
				return x.loadSpend(a.Pool.AuthByUID("synthetic-cn").Snapshot(), "lottery")
			}
		}
		return spendJournal{}, 0, nil
	}(); err == nil {
		t.Fatal("no-chance run persisted a durable spend attempt")
	}
	f.mu.Lock()
	f.chances = 2
	f.mu.Unlock()
	if run := runLifecycle(t, e, "lottery"); run.Status != taskengine.StatusSucceeded {
		t.Fatalf("later chance was blocked by the earlier read-only run %s", run.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/activity/growth/lottery/draw"] != 1 {
		t.Fatal("later chance did not draw exactly once")
	}
}

func TestRewardModulePostSuccessSaveFailureStaysUnknown(t *testing.T) {
	for _, key := range []string{"lottery", "buddy_box"} {
		t.Run(key, func(t *testing.T) {
			a, f, _ := lifecycleSetup(t)
			e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
			defer e.Close()
			// Close the state vault after the fixture accepted the spend: the
			// accepted receipt cannot be saved, so the outcome must stay unknown
			// and the next run must only inspect.
			for _, m := range a.Modules {
				var base *spendRewardModule
				switch x := m.(type) {
				case *lotteryModule:
					base = &x.spendRewardModule
				case *buddyBoxModule:
					base = &x.spendRewardModule
				}
				if base != nil && base.Spec().Key == key {
					f.afterWrite = func() { base.vault.Close() }
				}
			}
			if run := runLifecycle(t, e, key); run.Status != taskengine.StatusUncertain {
				t.Fatalf("save failure reported %s", run.Status)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.counts["/activity/growth/lottery/draw"]+f.counts["/activity/growth/buddy/open"] != 1 {
				t.Fatal("save failure replayed the spend")
			}
		})
	}
}

func TestLotteryConsumesAtMostOneOfficialChanceAndNeverInventsPrize(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	if run := runLifecycle(t, e, "lottery"); run.Status != taskengine.StatusSucceeded {
		t.Fatalf("lottery result %s", run.Status)
	}
	f.mu.Lock()
	if f.chances != 3 || f.counts["/activity/growth/lottery/draw"] != 1 {
		t.Fatalf("lottery consumed %d chances with %d draws", 4-f.chances, f.counts["/activity/growth/lottery/draw"])
	}
	f.chances = 0
	f.mu.Unlock()
	if run := runLifecycle(t, e, "lottery"); run.Status != taskengine.StatusSkipped {
		t.Fatalf("zero chances still submitted %s", run.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts["/activity/growth/lottery/draw"] != 1 {
		t.Fatal("no-chance run submitted a draw")
	}
}

func TestBuddyBoxRespectsOfficialEnergyAndUsesOneOpen(t *testing.T) {
	a, f, _ := lifecycleSetup(t)
	e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	if run := runLifecycle(t, e, "buddy_box"); run.Status != taskengine.StatusSucceeded {
		t.Fatalf("buddy box result %s", run.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buddyAffordable != 1 || f.counts["/activity/growth/buddy/open"] != 1 || f.writeBodies["/activity/growth/buddy/open"]["count"] != float64(1) {
		t.Fatal("buddy box did not consume exactly one official open")
	}
}

func TestRewardModulesNeverSubmitOnMalformedResourceState(t *testing.T) {
	for _, key := range []string{"lottery", "buddy_box"} {
		t.Run(key, func(t *testing.T) {
			a, f, _ := lifecycleSetup(t)
			f.malformed = true
			e := lifecycleEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
			defer e.Close()
			if run := runLifecycle(t, e, key); run.Status == taskengine.StatusSucceeded {
				t.Fatal("malformed resource state produced a successful draw/open")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.counts["/activity/growth/lottery/draw"] != 0 || f.counts["/activity/growth/buddy/open"] != 0 {
				t.Fatal("malformed state still submitted a spend")
			}
		})
	}
}
