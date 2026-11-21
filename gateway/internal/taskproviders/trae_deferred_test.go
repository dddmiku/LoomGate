package taskproviders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream/trae"
)

type deferredCheckinAdapter interface {
	TakeDeferredRequests(context.Context, time.Time, []taskengine.Schedule) ([]taskengine.Request, error)
}

func deferredCheckin(t *testing.T, a *Accounts, now time.Time, schedules []taskengine.Schedule) []taskengine.Request {
	t.Helper()
	hook, ok := any(a).(deferredCheckinAdapter)
	if !ok {
		t.Fatal("Trae 9074 cooldown has no automatic recovery hook")
	}
	requests, err := hook.TakeDeferredRequests(context.Background(), now, schedules)
	if err != nil {
		t.Fatal(err)
	}
	return requests
}

func deferredCheckinPlan() []taskengine.Schedule {
	return []taskengine.Schedule{{ID: "synthetic-trae-plan", ProviderID: "trae", TaskKey: "checkin", Enabled: true, Hours: []int{0}, Minute: 5, Timezone: "Asia/Shanghai"}}
}

// This is the observed production seam: an accepted daily run rejects one
// account with 9074, its durable cooldown expires, then only that account may
// be queued through the ordinary task engine and read/claim/verify path.
func TestTraeDeferred9074RecoversThroughEngineAfterCooldown(t *testing.T) {
	a, f, _, _, _, now := traeTaskSetup(t)
	e := traeEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	f.mode = "rate"
	if run := traeRun(t, e, "checkin"); run.Status != taskengine.StatusManual {
		t.Fatal("9074 fixture did not reach production refusal")
	}
	if requests := deferredCheckin(t, a, *now, deferredCheckinPlan()); len(requests) != 0 {
		t.Fatal("cooldown was ignored")
	}
	*now = now.Add(61 * time.Second)
	requests := deferredCheckin(t, a, *now, deferredCheckinPlan())
	if len(requests) != 1 || requests[0].Trigger != "retry" || requests[0].TaskKey != "checkin" || requests[0].ProviderID != "trae" || requests[0].ScheduleID != "synthetic-trae-plan" || len(requests[0].AccountIDs) != 1 || requests[0].AccountIDs[0] != "synthetic-trae" {
		t.Fatal("expired refusal did not produce the exact original account task")
	}
	f.mu.Lock()
	f.mode = "success"
	f.mu.Unlock()
	run, err := e.Submit(requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if final := waitModuleRun(t, e, run.ID); final.Status != taskengine.StatusSucceeded {
		t.Fatal("recovery did not verify official checked state")
	}
	*now = now.Add(10 * time.Minute)
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
		t.Fatal("completed account was queued again")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims != 2 {
		t.Fatalf("claim count=%d, want one rejected and one verified retry", f.claims)
	}
}

func TestTraeDeferred9074RespectsScopeCancellationAndConcurrency(t *testing.T) {
	a, f, p, _, _, now := traeTaskSetup(t)
	e := traeEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	f.mode = "rate"
	traeRun(t, e, "checkin")
	*now = now.Add(61 * time.Second)
	if len(deferredCheckin(t, a, *now, nil)) != 0 {
		t.Fatal("paused plans armed recovery")
	}
	plans := deferredCheckinPlan()
	plans[0].AccountIDs = []string{"another-account"}
	if len(deferredCheckin(t, a, *now, plans)) != 0 {
		t.Fatal("plan account scope broadened")
	}
	plans = deferredCheckinPlan()
	plans[0].Enabled = false
	if len(deferredCheckin(t, a, *now, plans)) != 0 {
		t.Fatal("disabled plan armed recovery")
	}
	plans = deferredCheckinPlan()
	plans[0].TaskKey = "balance"
	if len(deferredCheckin(t, a, *now, plans)) != 0 {
		t.Fatal("read-only plan armed recovery")
	}
	p.Disable("synthetic-trae", "manual")
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
		t.Fatal("disabled account armed recovery")
	}
	p.ReviveDisabled("synthetic-trae")
	hook, ok := any(a).(deferredCheckinAdapter)
	if !ok {
		t.Fatal("missing deferred recovery")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hook.TakeDeferredRequests(ctx, *now, deferredCheckinPlan()); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled recovery reserved an attempt")
	}
	var wg sync.WaitGroup
	counts := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			requests, err := hook.TakeDeferredRequests(context.Background(), *now, deferredCheckinPlan())
			if err != nil {
				counts <- -100
				return
			}
			counts <- len(requests)
		}()
	}
	wg.Wait()
	close(counts)
	total := 0
	for n := range counts {
		total += n
	}
	if total != 1 {
		t.Fatalf("concurrent cooldown reservations=%d, want 1", total)
	}
}

func TestTraeDeferred9074RestartBoundAndLegacyDay(t *testing.T) {
	a, _, _, v, path, now := traeTaskSetup(t)
	module := a.Modules[0].(*traeCheckinModule)
	// v1.0.8 stores have no recovery fields: only a 9074 refusal timestamped in
	// this business day can be migrated; older and future days remain inert.
	if err := module.store.change("synthetic-trae", func(s *traeTaskState, _ *traeTaskStateData) error {
		s.Code = 9074
		s.Failures = 1
		s.RetryAt = now.Add(-time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 1 {
		t.Fatal("same-day v1.0.8 refusal was not recovered")
	}
	v.Close()
	reopened, err := accountvault.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	module.store, err = NewTraeTaskStore(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
		t.Fatal("restart lost reservation cooldown")
	}
	for range 2 {
		*now = now.Add(61 * time.Second)
		if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 1 {
			t.Fatal("bounded retry disappeared")
		}
	}
	*now = now.Add(61 * time.Second)
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
		t.Fatal("more than three automatic retries were reserved")
	}
	*now = now.Add(24 * time.Hour)
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
		t.Fatal("old-day refusal replayed after rollover")
	}
	if err = module.backoff(&auth.Auth{UID: "synthetic-trae"}, 9074); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(61 * time.Second)
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 1 {
		t.Fatal("new-day genuine refusal did not reset bounded count")
	}
}

func TestTraeDeferred9074StopsAfterUnknownAuthAndDevice(t *testing.T) {
	for _, mode := range []string{"unknown", "missing", "device", "auth"} {
		t.Run(mode, func(t *testing.T) {
			a, f, _, _, _, now := traeTaskSetup(t)
			e := traeEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
			defer e.Close()
			f.mode = "rate"
			traeRun(t, e, "checkin")
			*now = now.Add(61 * time.Second)
			requests := deferredCheckin(t, a, *now, deferredCheckinPlan())
			if len(requests) != 1 {
				t.Fatal("missing rate retry")
			}
			f.mu.Lock()
			f.mode = mode
			f.mu.Unlock()
			if mode == "auth" {
				module := a.Modules[0].(*traeCheckinModule)
				module.api = &traeModuleStub{status: func(context.Context, *auth.Auth) (trae.CheckinStatus, error) {
					return trae.CheckinStatus{}, &trae.AuthError{}
				}, claim: func(context.Context, *auth.Auth) (trae.CheckinStatus, error) {
					t.Fatal("auth failure reached claim")
					return trae.CheckinStatus{}, nil
				}}
			}
			run, err := e.Submit(requests[0])
			if err != nil {
				t.Fatal(err)
			}
			waitModuleRun(t, e, run.ID)
			*now = now.Add(10 * time.Minute)
			if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
				t.Fatal("unknown/auth/device result stayed eligible for automatic mutation")
			}
		})
	}
}

func TestTraeDeferred9074BudgetSurvivesRepeatedBusinessRefusals(t *testing.T) {
	a, f, _, _, _, now := traeTaskSetup(t)
	e := traeEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	f.mode = "rate"
	traeRun(t, e, "checkin")
	for range traeAutoRetryLimit {
		*now = now.Add(9 * time.Minute)
		requests := deferredCheckin(t, a, *now, deferredCheckinPlan())
		if len(requests) != 1 {
			t.Fatal("genuine 9074 lost bounded automatic retry")
		}
		r, err := e.Submit(requests[0])
		if err != nil {
			t.Fatal(err)
		}
		if run := waitModuleRun(t, e, r.ID); run.Status != taskengine.StatusManual {
			t.Fatal("rejected recovery did not stay manual")
		}
	}
	*now = now.Add(9 * time.Minute)
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
		t.Fatal("each 9074 reset the daily retry budget")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims != 1+traeAutoRetryLimit {
		t.Fatalf("automatic retries not bounded: claims=%d", f.claims)
	}
}

func TestTraeDeferred9074LegacyEligibilityAndLocalOnlyReservation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		dayOffset int
		want      int
	}{
		{"today", 9074, 0, 1}, {"previous_day", 9074, -1, 0}, {"future_day", 9074, 1, 0}, {"device", 9095, 0, 0}, {"auth", 1001, 0, 0}, {"none", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, _, _, _, now := traeTaskSetup(t)
			module := a.Modules[0].(*traeCheckinModule)
			if err := module.store.change("synthetic-trae", func(s *traeTaskState, _ *traeTaskStateData) error {
				s.Code = tc.code
				s.Failures = 1
				s.RetryAt = now.AddDate(0, 0, tc.dayOffset).Add(-time.Minute)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if got := len(deferredCheckin(t, a, *now, deferredCheckinPlan())); got != tc.want {
				t.Fatalf("legacy candidate count=%d want=%d", got, tc.want)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.status != 0 || f.claims != 0 || f.reads != 0 {
				t.Fatal("local recovery hook made an upstream request")
			}
		})
	}
}

func TestTraeRecoveryJournalRejectsInvalidNewFields(t *testing.T) {
	for _, state := range []traeTaskState{
		{RetryDay: "not-a-date"},
		{RetryDay: "2099-1-03"},
		{AutoAttempts: 1, NextAutoAt: time.Now()},
		{RetryDay: "2099-01-03", AutoAttempts: -1},
		{RetryDay: "2099-01-03", AutoAttempts: 4, NextAutoAt: time.Now()},
		{RetryDay: "2099-01-03", AutoAttempts: 1},
		{RetryDay: "2099-01-03", AutoPending: true},
		{RetryDay: "2099-01-03", AutoPending: true, Code: 9095, RetryAt: time.Now()},
	} {
		v, err := accountvault.Open(filepath.Join(t.TempDir(), "invalid-vault"))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(traeTaskStateData{Accounts: map[string]traeTaskState{traeStateKey("synthetic-trae"): state}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = v.Put(accountvault.Account{ID: "trae-task-state", Provider: "trae", Enabled: true}, raw, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = NewTraeTaskStore(v)
		v.Close()
		if err == nil {
			t.Fatal("invalid recovery journal admitted")
		}
	}
}

func TestTraeDeferred9074RequiresDurableStoreAndExistingAccount(t *testing.T) {
	a, _, _, _, _, now := traeTaskSetup(t)
	module := a.Modules[0].(*traeCheckinModule)
	if err := module.backoff(&auth.Auth{UID: "synthetic-trae"}, 9074); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(61 * time.Second)
	vault := module.store.vault
	module.store.vault = nil
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
		t.Fatal("non-durable store armed automatic mutation")
	}
	module.store.vault = vault
	original := a.Pool
	a.Pool = pool.New("")
	defer a.Pool.Close()
	defer func() { a.Pool = original }()
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
		t.Fatal("removed account armed automatic mutation")
	}
}

func TestTraeDeferred9074RecoversViaSchedulerTick(t *testing.T) {
	a, f, _, _, _, now := traeTaskSetup(t)
	e := traeEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	plan := deferredCheckinPlan()[0]
	if _, err := e.SaveSchedule(plan); err != nil {
		t.Fatal(err)
	}
	f.mode = "rate"
	// Coalesce the old scheduled instant into this fixture business day.
	if err := e.Tick(*now); err != nil {
		t.Fatal(err)
	}
	page := e.Runs()
	if len(page) != 1 {
		t.Fatalf("scheduled initial runs=%d", len(page))
	}
	if first := waitModuleRun(t, e, page[0].ID); first.Status != taskengine.StatusManual {
		t.Fatal("scheduled fixture did not persist 9074 refusal")
	}
	before := e.Schedules()[0]
	if before.NextRun == nil || before.LastDispatched == nil {
		t.Fatal("initial schedule lost runtime instant")
	}
	*now = now.Add(30 * time.Second)
	if err := e.Tick(*now); err != nil {
		t.Fatal(err)
	}
	if page = e.Runs(); len(page) != 1 {
		t.Fatal("scheduler ignored provider cooldown")
	}
	f.mu.Lock()
	f.mode = "success"
	f.mu.Unlock()
	*now = now.Add(31 * time.Second)
	// FinishedAt is visible before the prior worker releases its active lease.
	// The real scheduler keeps ticking; do not require this exact nanosecond
	// to bypass that lease, or weaken the production duplicate-work guard.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := e.Tick(*now); err != nil {
			t.Fatal(err)
		}
		page = e.Runs()
		if len(page) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(page) != 2 {
		t.Fatalf("expired 9074 retry was never dispatched: runs=%d", len(page))
	}
	var retry taskengine.Run
	for _, r := range page {
		if r.Trigger == "retry" {
			retry = r
			break
		}
	}
	if retry.ID == "" {
		t.Fatal("recovery did not retain retry audit trigger")
	}
	if final := waitModuleRun(t, e, retry.ID); final.Status != taskengine.StatusSucceeded {
		t.Fatal("scheduled recovery did not verify checked state")
	}
	after := e.Schedules()[0]
	if !before.NextRun.Equal(*after.NextRun) || !before.LastDispatched.Equal(*after.LastDispatched) || before.Minute != after.Minute || !reflect.DeepEqual(before.Hours, after.Hours) || before.Enabled != after.Enabled {
		t.Fatal("deferred retry changed the user configured schedule")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims != 2 {
		t.Fatalf("scheduler claim count=%d, want 2", f.claims)
	}
}

func TestTraeDeferred9074QueuedAcrossBusinessDayDoesNotClaimNewPeriod(t *testing.T) {
	a, f, _, _, _, now := traeTaskSetup(t)
	*now = time.Date(2099, 1, 3, 15, 58, 58, 0, time.UTC) // CST 23:58:58
	e := traeEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	f.mode = "rate"
	if first := traeRun(t, e, "checkin"); first.Status != taskengine.StatusManual {
		t.Fatal("midnight fixture did not persist rate refusal")
	}
	*now = now.Add(61 * time.Second) // CST 23:59:59, same-day retry is due.
	requests := deferredCheckin(t, a, *now, deferredCheckinPlan())
	if len(requests) != 1 || requests[0].ExpectedPeriodKey != "2099-01-03" {
		t.Fatal("retry did not bind its original business period")
	}
	// Discovery occurs after midnight, as can happen while the request waits
	// for the engine worker. It must not become a new-day automatic claim.
	*now = now.Add(2 * time.Second)
	f.mu.Lock()
	f.mode = "success"
	f.mu.Unlock()
	retry, err := e.Submit(requests[0])
	if err != nil {
		t.Fatal(err)
	}
	waitModuleRun(t, e, retry.ID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims != 1 {
		t.Fatalf("old-day queued retry claimed next business day: claims=%d", f.claims)
	}
}

func TestTraeDeferred9074LegacyMidnightDeadlineDoesNotMigrateYesterday(t *testing.T) {
	for _, failures := range []int{0, 1} {
		t.Run(fmt.Sprintf("failures_%d", failures), func(t *testing.T) {
			a, _, _, _, _, now := traeTaskSetup(t)
			*now = time.Date(2099, 1, 2, 16, 0, 30, 0, time.UTC) // CST Jan 3, 00:00:30
			module := a.Modules[0].(*traeCheckinModule)
			if err := module.store.change("synthetic-trae", func(s *traeTaskState, _ *traeTaskStateData) error {
				s.Code = 9074
				s.Failures = failures
				s.RetryAt = now.Add(-30 * time.Second)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Failures=1 implies yesterday 23:59 refused and today 00:00 deadline;
			// Failures=0 has no provable refusal instant. Neither may migrate today.
			if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
				t.Fatal("midnight deadline manufactured a current-day refusal")
			}
			if err := module.backoff(&auth.Auth{UID: "synthetic-trae"}, 9074); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(61 * time.Second)
			requests := deferredCheckin(t, a, *now, deferredCheckinPlan())
			if len(requests) != 1 || requests[0].ExpectedPeriodKey != "2099-01-03" {
				t.Fatal("genuine new-day refusal did not start its own bounded recovery")
			}
			state := module.store.read("synthetic-trae")
			if state.RetryDay != "2099-01-03" || state.AutoAttempts != 1 || state.Failures != 1 {
				t.Fatal("new business day inherited stale recovery budget")
			}
		})
	}
}

func TestTraeDeferred9074ReservesOnlyOneDueAccountPerCall(t *testing.T) {
	a, f, p, _, _, now := traeTaskSetup(t)
	second := p.AuthByUID("synthetic-trae").Snapshot()
	second.UID = "synthetic-second-trae"
	p.Add(second)
	module := a.Modules[0].(*traeCheckinModule)
	for _, id := range []string{"synthetic-trae", second.UID} {
		if err := module.backoff(&auth.Auth{UID: id}, 9074); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(61 * time.Second)
	requests := deferredCheckin(t, a, *now, deferredCheckinPlan())
	if len(requests) != 1 {
		t.Fatalf("one tick reserved %d accounts", len(requests))
	}
	firstBudget := module.store.read("synthetic-trae").AutoAttempts
	secondBudget := module.store.read(second.UID).AutoAttempts
	if firstBudget+secondBudget != 1 || firstBudget != 0 && secondBudget != 0 {
		t.Fatal("undispatched tail account consumed automatic retry budget")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != 0 || f.claims != 0 {
		t.Fatal("reservation performed upstream work")
	}
}

func TestTraeYesterdayCooldownDoesNotSwallowTodayScheduledCheckin(t *testing.T) {
	for _, retryDay := range []string{"", "2099-01-02"} {
		t.Run("retry_day_"+retryDay, func(t *testing.T) {
			a, f, _, _, _, now := traeTaskSetup(t)
			*now = time.Date(2099, 1, 2, 16, 5, 0, 0, time.UTC) // CST Jan 3, 00:05
			module := a.Modules[0].(*traeCheckinModule)
			if err := module.store.change("synthetic-trae", func(s *traeTaskState, _ *traeTaskStateData) error {
				s.Code = 9074
				s.Failures = 4
				s.RetryAt = now.Add(2 * time.Minute)
				s.RetryDay = retryDay
				if retryDay != "" {
					s.AutoPending = true
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			e := traeEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
			defer e.Close()
			if _, err := e.SaveSchedule(deferredCheckinPlan()[0]); err != nil {
				t.Fatal(err)
			}
			if err := e.Tick(*now); err != nil {
				t.Fatal(err)
			}
			runs := e.Runs()
			if len(runs) != 1 {
				t.Fatalf("today plan dispatches=%d", len(runs))
			}
			if final := waitModuleRun(t, e, runs[0].ID); final.Status != taskengine.StatusSucceeded {
				t.Fatal("yesterday cooldown swallowed the new-day plan before official status")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.status == 0 || f.claims != 1 {
				t.Fatalf("new-day status/claim path was not reached: status=%d claims=%d", f.status, f.claims)
			}
		})
	}
}

func TestTraeUnknownVerification9074DoesNotArmAutomaticRecovery(t *testing.T) {
	a, f, _, _, _, now := traeTaskSetup(t)
	e := traeEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	f.mode = "unknown"
	if first := traeRun(t, e, "checkin"); first.Status != taskengine.StatusUncertain {
		t.Fatal("fixture did not retain the unknown original claim")
	}
	module := a.Modules[0].(*traeCheckinModule)
	module.api = &traeModuleStub{status: func(context.Context, *auth.Auth) (trae.CheckinStatus, error) {
		return trae.CheckinStatus{}, &trae.RateLimitError{}
	}, claim: func(context.Context, *auth.Auth) (trae.CheckinStatus, error) {
		t.Fatal("unknown claim was replayed")
		return trae.CheckinStatus{}, nil
	}}
	// Catalog discovery sees 9074 before the engine attaches PendingVerification;
	// the later inspect must close that qualification even under its local gate.
	if reconciled := traeRun(t, e, "checkin"); reconciled.Status != taskengine.StatusUncertain {
		t.Fatal("9074 verification cleared the original unknown action")
	}
	state := module.store.read("synthetic-trae")
	if state.AutoPending || state.RetryDay != module.day() || state.Code != 9074 {
		t.Fatal("unknown verification rearmed automatic retry or left legacy migration open")
	}
	*now = now.Add(9 * time.Minute)
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
		t.Fatal("unknown original mutation produced a deferred candidate")
	}
	// Also exercise the API-rate path with the pending flag already present.
	account, err := a.account("synthetic-trae")
	if err != nil {
		t.Fatal(err)
	}
	task := taskengine.Task{Definition: taskengine.Definition{Key: "checkin", PeriodKey: module.day()}, PendingVerification: true}
	if _, err = module.Inspect(context.Background(), account, task); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(9 * time.Minute)
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
		t.Fatal("pending rate status acquired automatic action permission")
	}
	*now = now.Add(24 * time.Hour)
	if reconciled := traeRun(t, e, "checkin"); reconciled.Status != taskengine.StatusUncertain {
		t.Fatal("new-day discovery cleared the original unknown action")
	}
	*now = now.Add(9 * time.Minute)
	if len(deferredCheckin(t, a, *now, deferredCheckinPlan())) != 0 {
		t.Fatal("new-day discovery armed a retry around the original unknown action")
	}
}
