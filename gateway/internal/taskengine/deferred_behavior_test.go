package taskengine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

type deferredAdapter struct {
	*behaviorAdapter
	take func(context.Context, time.Time, []taskengine.Schedule) ([]taskengine.Request, error)
}

func (a *deferredAdapter) TakeDeferredRequests(ctx context.Context, now time.Time, schedules []taskengine.Schedule) ([]taskengine.Request, error) {
	return a.take(ctx, now, schedules)
}

func deferredPlan(t *testing.T, e *taskengine.Engine, provider string) taskengine.Schedule {
	t.Helper()
	s, err := e.SaveSchedule(taskengine.Schedule{ID: provider + "-plan", ProviderID: provider, TaskKey: "checkin", Enabled: true, Hours: []int{0}, Minute: 5, Timezone: "Asia/Shanghai", AccountIDs: []string{"account-a"}, OncePerDay: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func deferredRequest() taskengine.Request {
	return taskengine.Request{ProviderID: "fake", TaskKey: "checkin", ScheduleID: "fake-plan", AccountIDs: []string{"account-a"}}
}

func TestDeferredTickUsesOriginalTaskAndPreservesSchedule(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	var executes atomic.Int32
	a := &deferredAdapter{behaviorAdapter: singleAdapter(syntheticTask("fake", "account-a", "checkin"))}
	a.execute = func(_ context.Context, task taskengine.Task) (taskengine.Observation, error) {
		executes.Add(1)
		return taskengine.Accepted(task, taskengine.StageExecute, phase("pending")), nil
	}
	a.take = func(_ context.Context, _ time.Time, plans []taskengine.Schedule) ([]taskengine.Request, error) {
		// Accessing the engine here also proves the hook runs outside its mutex.
		_ = e.Schedules()
		if len(plans) != 1 {
			t.Errorf("enabled plan count = %d", len(plans))
		}
		return []taskengine.Request{deferredRequest(), deferredRequest()}, nil
	}
	if err := e.Register("fake", a); err != nil {
		t.Fatal(err)
	}
	before := deferredPlan(t, e, "fake")
	if err := e.Tick(time.Now()); err != nil {
		t.Fatal(err)
	}
	runs := e.Runs()
	if len(runs) != 1 {
		t.Fatalf("deferred work dispatched %d times, want 1", len(runs))
	}
	run := awaitBehaviorRun(t, e, runs[0].ID)
	if run.Trigger != "retry" || run.TaskKey != "checkin" || run.ScheduleID != before.ID || executes.Load() != 1 || run.Status != taskengine.StatusSucceeded {
		t.Fatalf("deferred work did not use original verified path: %+v executes=%d", run, executes.Load())
	}
	if !reflect.DeepEqual(before, e.Schedules()[0]) {
		t.Fatal("deferred dispatch changed ordinary schedule timing or attribution")
	}
	log, err := e.TaskLogFor("fake", "checkin")
	if err != nil || len(log.Lines) == 0 {
		t.Fatalf("missing retry log: %v", err)
	}
}

func TestDeferredTickRevalidatesPausedPlanAfterHook(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	a := &deferredAdapter{behaviorAdapter: singleAdapter(syntheticTask("fake", "account-a", "checkin"))}
	a.take = func(_ context.Context, _ time.Time, plans []taskengine.Schedule) ([]taskengine.Request, error) {
		s := plans[0]
		s.Enabled = false
		if _, err := e.SaveSchedule(s); err != nil {
			t.Error(err)
		}
		return []taskengine.Request{deferredRequest()}, nil
	}
	if err := e.Register("fake", a); err != nil {
		t.Fatal(err)
	}
	deferredPlan(t, e, "fake")
	if err := e.Tick(time.Now()); err != nil || len(e.Runs()) != 0 {
		t.Fatalf("paused plan dispatched deferred work: err=%v runs=%d", err, len(e.Runs()))
	}
}

func TestDeferredTickRejectsExpandedAndForeignRequests(t *testing.T) {
	for _, mutate := range []struct {
		name string
		fn   func(*taskengine.Request)
	}{
		{"other-provider", func(r *taskengine.Request) { r.ProviderID = "other" }},
		{"other-plan", func(r *taskengine.Request) { r.ScheduleID = "other-plan" }},
		{"other-account", func(r *taskengine.Request) { r.AccountIDs = []string{"account-b"} }},
		{"all-accounts", func(r *taskengine.Request) { r.AccountIDs = nil }},
		{"all-tasks", func(r *taskengine.Request) { r.TaskKey = "*" }},
		{"empty-task", func(r *taskengine.Request) { r.TaskKey = "" }},
		{"module-selector", func(r *taskengine.Request) { r.TaskKey = "checkin/" }},
		{"other-task", func(r *taskengine.Request) { r.TaskKey = "balance" }},
		{"sequence", func(r *taskengine.Request) { r.TaskSequence = []string{"checkin", "balance"} }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			e := openBehaviorEngine(t, taskengine.Options{})
			a := &deferredAdapter{behaviorAdapter: singleAdapter(syntheticTask("fake", "account-a", "checkin"))}
			a.take = func(context.Context, time.Time, []taskengine.Schedule) ([]taskengine.Request, error) {
				r := deferredRequest()
				mutate.fn(&r)
				return []taskengine.Request{r}, nil
			}
			if err := e.Register("fake", a); err != nil {
				t.Fatal(err)
			}
			deferredPlan(t, e, "fake")
			if err := e.Tick(time.Now()); err != nil || len(e.Runs()) != 0 {
				t.Fatalf("invalid request dispatched: err=%v runs=%d", err, len(e.Runs()))
			}
		})
	}
}

func TestDeferredTickIsolatesHookFailureAndProcessesReservedPartialResult(t *testing.T) {
	for _, panicHook := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial-error", true: "panic"}[panicHook], func(t *testing.T) {
			e := openBehaviorEngine(t, taskengine.Options{})
			a := &deferredAdapter{behaviorAdapter: singleAdapter(syntheticTask("fake", "account-a", "checkin"))}
			a.take = func(context.Context, time.Time, []taskengine.Schedule) ([]taskengine.Request, error) {
				if panicHook {
					panic("synthetic failure")
				}
				return []taskengine.Request{deferredRequest()}, errors.New("synthetic local store failure")
			}
			if err := e.Register("fake", a); err != nil {
				t.Fatal(err)
			}
			deferredPlan(t, e, "fake")
			if err := e.Tick(time.Now()); err != nil {
				t.Fatal(err)
			}
			want := 1
			if panicHook {
				want = 0
			}
			if len(e.Runs()) != want {
				t.Fatalf("partial result run count=%d want=%d", len(e.Runs()), want)
			}
			for _, r := range e.Runs() {
				awaitBehaviorRun(t, e, r.ID)
			}
		})
	}
}

func TestDeferredTickDoesNotSpendReservationsBehindActiveOrUnknownWork(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "unknown"}[unknown], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			e := openBehaviorEngine(t, taskengine.Options{Path: path})
			entered, release := make(chan struct{}), make(chan struct{})
			var takes, executes atomic.Int32
			a := &deferredAdapter{behaviorAdapter: singleAdapter(syntheticTask("fake", "account-a", "checkin"))}
			a.execute = func(ctx context.Context, task taskengine.Task) (taskengine.Observation, error) {
				executes.Add(1)
				if unknown {
					return taskengine.UnknownAction(task, taskengine.StageExecute, phase("pending")), errors.New("synthetic lost response")
				}
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
				return taskengine.Accepted(task, taskengine.StageExecute, phase("pending")), nil
			}
			a.take = func(context.Context, time.Time, []taskengine.Schedule) ([]taskengine.Request, error) {
				takes.Add(1)
				return []taskengine.Request{deferredRequest()}, nil
			}
			if err := e.Register("fake", a); err != nil {
				t.Fatal(err)
			}
			deferredPlan(t, e, "fake")
			r := submitBehavior(t, e, deferredRequest())
			if unknown {
				if got := awaitBehaviorRun(t, e, r.ID); got.Status != taskengine.StatusUncertain {
					t.Fatalf("missing unknown marker: %s", got.Status)
				}
			} else {
				defer close(release)
				awaitBehaviorSignal(t, entered)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if err = e.Tick(time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if takes.Load() != 0 || executes.Load() != 1 || len(e.Runs()) != 1 {
				t.Fatalf("reserved or repeated protected work: takes=%d executes=%d runs=%d", takes.Load(), executes.Load(), len(e.Runs()))
			}
			if unknown && string(before) != string(after) {
				t.Fatal("deferred tick rewrote the unknown journal")
			}
		})
	}
}

func TestDeferredTickPlanSnapshotIsDetached(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	a := &deferredAdapter{behaviorAdapter: singleAdapter(syntheticTask("fake", "account-a", "checkin"))}
	a.take = func(_ context.Context, _ time.Time, plans []taskengine.Schedule) ([]taskengine.Request, error) {
		plans[0].AccountIDs[0] = "account-b"
		plans[0].Hours[0] = 23
		plans[0].TaskSequence[0] = "balance"
		*plans[0].NextRun = plans[0].NextRun.Add(time.Hour)
		*plans[0].LegacyEnabled = false
		return nil, nil
	}
	if err := e.Register("fake", a); err != nil {
		t.Fatal(err)
	}
	s := deferredPlan(t, e, "fake")
	s.TaskKey, s.TaskSequence = "sequence", []string{"checkin", "balance"}
	yes := true
	s.LegacyEnabled = &yes
	before, err := e.SaveSchedule(s)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Tick(time.Now()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, e.Schedules()[0]) {
		t.Fatal("provider mutated the live plan through its snapshot")
	}
}

func TestDeferredTickCloseWaitsForHookAndPreventsLateDispatch(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	entered, release := make(chan struct{}), make(chan struct{})
	a := &deferredAdapter{behaviorAdapter: singleAdapter(syntheticTask("fake", "account-a", "checkin"))}
	a.take = func(context.Context, time.Time, []taskengine.Schedule) ([]taskengine.Request, error) {
		close(entered)
		<-release
		return []taskengine.Request{deferredRequest()}, nil
	}
	if err := e.Register("fake", a); err != nil {
		t.Fatal(err)
	}
	deferredPlan(t, e, "fake")
	tickDone := make(chan error, 1)
	go func() { tickDone <- e.Tick(time.Now()) }()
	awaitBehaviorSignal(t, entered)
	e.Quiesce()
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned while provider journal was in use: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-tickDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if len(e.Runs()) != 0 {
		t.Fatal("deferred hook dispatched during shutdown")
	}
}
