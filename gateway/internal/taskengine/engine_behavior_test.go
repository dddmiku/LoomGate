package taskengine_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

type behaviorAdapter struct {
	discover func(context.Context, taskengine.Scope) ([]taskengine.Task, error)
	inspect  func(context.Context, taskengine.Task) (taskengine.Observation, error)
	execute  func(context.Context, taskengine.Task) (taskengine.Observation, error)
	claim    func(context.Context, taskengine.Task) (taskengine.Observation, error)
	verify   func(context.Context, taskengine.Task) (taskengine.Observation, error)
}

func (a *behaviorAdapter) Discover(ctx context.Context, s taskengine.Scope) ([]taskengine.Task, error) {
	return a.discover(ctx, s)
}
func (a *behaviorAdapter) Inspect(ctx context.Context, task taskengine.Task) (taskengine.Observation, error) {
	if a.inspect != nil {
		return a.inspect(ctx, task)
	}
	return phase("ready"), nil
}
func (a *behaviorAdapter) Execute(ctx context.Context, task taskengine.Task) (taskengine.Observation, error) {
	if a.execute != nil {
		return a.execute(ctx, task)
	}
	return phase("pending"), nil
}
func (a *behaviorAdapter) Claim(ctx context.Context, task taskengine.Task) (taskengine.Observation, error) {
	if a.claim != nil {
		return a.claim(ctx, task)
	}
	return phase("pending"), nil
}
func (a *behaviorAdapter) Verify(ctx context.Context, task taskengine.Task) (taskengine.Observation, error) {
	if a.verify != nil {
		return a.verify(ctx, task)
	}
	return phase("done"), nil
}
func phase(p string) taskengine.Observation {
	return taskengine.Observation{Phase: p, Status: taskengine.StatusSucceeded}
}
func syntheticTask(provider, account, key string) taskengine.Task {
	return taskengine.Task{
		Definition: taskengine.Definition{Key: key, ProviderID: provider, Name: "Synthetic task", SideEffect: true},
		Target:     taskengine.Target{AccountID: account},
	}
}
func singleAdapter(task taskengine.Task) *behaviorAdapter {
	return &behaviorAdapter{discover: func(context.Context, taskengine.Scope) ([]taskengine.Task, error) {
		return []taskengine.Task{task}, nil
	}}
}
func openBehaviorEngine(t *testing.T, opts taskengine.Options) *taskengine.Engine {
	t.Helper()
	if opts.Path == "" {
		opts.Path = filepath.Join(t.TempDir(), "state.json")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 3 * time.Second
	}
	if opts.PollMin == 0 {
		opts.PollMin = time.Millisecond
	}
	if opts.PollMax == 0 {
		opts.PollMax = 2 * time.Millisecond
	}
	e, err := taskengine.Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return e
}
func registerBehavior(t *testing.T, e *taskengine.Engine, provider string, a *behaviorAdapter) {
	t.Helper()
	if err := e.Register(provider, a); err != nil {
		t.Fatalf("Register: %v", err)
	}
}
func submitBehavior(t *testing.T, e *taskengine.Engine, req taskengine.Request) taskengine.Run {
	t.Helper()
	r, err := e.Submit(req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return r
}
func awaitBehaviorRun(t *testing.T, e *taskengine.Engine, id string) taskengine.Run {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, r := range e.Runs() {
			if r.ID == id && r.FinishedAt != nil {
				return r
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("run did not reach a terminal state: %s", id)
		case <-tick.C:
		}
	}
}
func awaitBehaviorSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("adapter did not reach expected stage")
	}
}

func TestMutationResponseCannotSubstituteForVerification(t *testing.T) {
	for _, operation := range []string{"execute", "claim"} {
		t.Run(operation, func(t *testing.T) {
			e := openBehaviorEngine(t, taskengine.Options{Timeout: 80 * time.Millisecond})
			a := singleAdapter(syntheticTask("fake", "account-a", "task-a"))
			var mutations, verifications atomic.Int32
			mutate := func(context.Context, taskengine.Task) (taskengine.Observation, error) {
				mutations.Add(1)
				// An HTTP-success adapter may optimistically report done. Only read-back counts.
				return phase("done"), nil
			}
			if operation == "execute" {
				a.execute = mutate
			} else {
				a.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) { return phase("claimable"), nil }
				a.claim = mutate
			}
			a.verify = func(context.Context, taskengine.Task) (taskengine.Observation, error) {
				verifications.Add(1)
				return phase("pending"), nil
			}
			registerBehavior(t, e, "fake", a)
			r := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
			if r.Status != taskengine.StatusUncertain {
				t.Errorf("unverified mutation status = %s, want uncertain", r.Status)
			}
			if mutations.Load() != 1 || verifications.Load() == 0 {
				t.Errorf("mutations=%d verifications=%d", mutations.Load(), verifications.Load())
			}
			awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
			if mutations.Load() != 1 {
				t.Fatal("unconfirmed mutation was replayed")
			}
		})
	}
}

func TestVerifiedDailyAndOneTimeIdempotencyUsesUpstreamPeriod(t *testing.T) {
	for _, recurrence := range []string{"daily", "once", "repeatable"} {
		t.Run(recurrence, func(t *testing.T) {
			e := openBehaviorEngine(t, taskengine.Options{})
			var period, executes atomic.Int32
			a := &behaviorAdapter{discover: func(context.Context, taskengine.Scope) ([]taskengine.Task, error) {
				task := syntheticTask("fake", "account-a", "task-a")
				task.OncePerDay, task.Once = recurrence == "daily", recurrence == "once"
				if period.Load() == 0 {
					task.PeriodKey = "upstream-business-day-a"
				} else {
					task.PeriodKey = "upstream-business-day-b"
				}
				return []taskengine.Task{task}, nil
			}, execute: func(context.Context, taskengine.Task) (taskengine.Observation, error) {
				executes.Add(1)
				return phase("pending"), nil
			}}
			registerBehavior(t, e, "fake", a)
			for i := 0; i < 2; i++ {
				awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
			}
			want := int32(1)
			if recurrence == "repeatable" {
				want = 2
			}
			if executes.Load() != want {
				t.Fatalf("same period executions = %d, want %d", executes.Load(), want)
			}
			period.Store(1)
			awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
			if recurrence != "once" {
				want++
			}
			if executes.Load() != want {
				t.Errorf("changed upstream period executions = %d, want %d", executes.Load(), want)
			}
		})
	}
}

func TestCancellationDistinguishesReadOnlyWaitFromUnconfirmedMutation(t *testing.T) {
	cases := []struct {
		name               string
		mutated, readError bool
	}{
		{"before-mutation-successful-read", false, false},
		{"before-mutation-read-canceled", false, true},
		{"after-mutation-successful-read", true, false},
		{"after-mutation-read-canceled", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := openBehaviorEngine(t, taskengine.Options{})
			entered := make(chan struct{})
			a := singleAdapter(syntheticTask("fake", "account-a", "task-a"))
			if !tc.mutated {
				a.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) { return phase("pending"), nil }
			}
			a.verify = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
				close(entered)
				<-ctx.Done()
				if tc.readError {
					return taskengine.Observation{}, ctx.Err()
				}
				// Cancellation can race with a successful read returning still-pending state.
				return phase("pending"), nil
			}
			registerBehavior(t, e, "fake", a)
			r := submitBehavior(t, e, taskengine.Request{ProviderID: "fake"})
			awaitBehaviorSignal(t, entered)
			if !e.Cancel(r.ID) {
				t.Fatal("active run was not cancelable")
			}
			finished := awaitBehaviorRun(t, e, r.ID)
			want := taskengine.StatusCanceled
			if tc.mutated {
				want = taskengine.StatusUncertain
			}
			if finished.Status != want {
				t.Errorf("status=%s want=%s", finished.Status, want)
			}
		})
	}
}

func TestPanicIsolatedFromOtherProvidersAndNoMutationReplay(t *testing.T) {
	for _, stage := range []string{"discover", "inspect", "execute", "claim", "verify"} {
		t.Run(stage, func(t *testing.T) {
			e := openBehaviorEngine(t, taskengine.Options{})
			a := singleAdapter(syntheticTask("broken", "account-a", "task-a"))
			var panics atomic.Int32
			panicCall := func(context.Context, taskengine.Task) (taskengine.Observation, error) {
				panics.Add(1)
				panic("synthetic-adapter-panic")
			}
			switch stage {
			case "discover":
				a.discover = func(context.Context, taskengine.Scope) ([]taskengine.Task, error) { panic("synthetic-discovery-panic") }
			case "inspect":
				a.inspect = panicCall
			case "execute":
				a.execute = panicCall
			case "claim":
				a.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) { return phase("claimable"), nil }
				a.claim = panicCall
			case "verify":
				a.verify = panicCall
			}
			registerBehavior(t, e, "broken", a)
			registerBehavior(t, e, "healthy", singleAdapter(syntheticTask("healthy", "account-a", "task-a")))
			bad := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "broken"}).ID)
			if bad.Status == taskengine.StatusSucceeded {
				t.Fatal("panic was reported as success")
			}
			good := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "healthy"}).ID)
			if good.Status != taskengine.StatusSucceeded {
				t.Fatalf("other provider failed: %s", good.Status)
			}
			if stage == "execute" || stage == "claim" || stage == "verify" {
				awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "broken"}).ID)
				if panics.Load() != 1 {
					t.Errorf("panic during a mutation led to blind replay: calls=%d", panics.Load())
				}
			}
		})
	}
}

func TestSeparateAccountsAndProvidersCanProceedWhileOneIsBlocked(t *testing.T) {
	for _, separateProvider := range []bool{false, true} {
		t.Run(map[bool]string{false: "accounts", true: "providers"}[separateProvider], func(t *testing.T) {
			e := openBehaviorEngine(t, taskengine.Options{Concurrency: 2})
			blocked := make(chan struct{})
			a := &behaviorAdapter{discover: func(_ context.Context, scope taskengine.Scope) ([]taskengine.Task, error) {
				return []taskengine.Task{syntheticTask("fake", scope.AccountIDs[0], "task-a")}, nil
			}, execute: func(ctx context.Context, task taskengine.Task) (taskengine.Observation, error) {
				if task.AccountID == "account-a" {
					close(blocked)
					<-ctx.Done()
					return taskengine.Observation{}, ctx.Err()
				}
				return phase("pending"), nil
			}}
			registerBehavior(t, e, "fake", a)
			blockedRun := submitBehavior(t, e, taskengine.Request{ProviderID: "fake", AccountIDs: []string{"account-a"}})
			awaitBehaviorSignal(t, blocked)
			req := taskengine.Request{ProviderID: "fake", AccountIDs: []string{"account-b"}}
			if separateProvider {
				registerBehavior(t, e, "other", singleAdapter(syntheticTask("other", "account-a", "task-a")))
				req = taskengine.Request{ProviderID: "other", AccountIDs: []string{"account-a"}}
			}
			good := awaitBehaviorRun(t, e, submitBehavior(t, e, req).ID)
			if good.Status != taskengine.StatusSucceeded {
				t.Errorf("independent run status=%s", good.Status)
			}
			if !e.Cancel(blockedRun.ID) {
				t.Error("blocked run finished before it was released")
			}
		})
	}
}

func TestSameAccountNeverExecutesConcurrently(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{Concurrency: 2})
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var active, overlap atomic.Int32
	a := singleAdapter(syntheticTask("fake", "account-a", "task-a"))
	a.execute = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
		if active.Add(1) != 1 {
			overlap.Add(1)
		}
		defer active.Add(-1)
		entered <- struct{}{}
		select {
		case <-release:
			return phase("pending"), nil
		case <-ctx.Done():
			return taskengine.Observation{}, ctx.Err()
		}
	}
	registerBehavior(t, e, "fake", a)
	first := submitBehavior(t, e, taskengine.Request{ProviderID: "fake"})
	awaitBehaviorSignal(t, entered)
	second := submitBehavior(t, e, taskengine.Request{ProviderID: "fake"})
	select {
	case <-entered:
		t.Error("second operation entered before first released")
	case <-time.After(35 * time.Millisecond):
	}
	close(release)
	awaitBehaviorRun(t, e, first.ID)
	awaitBehaviorRun(t, e, second.ID)
	if overlap.Load() != 0 {
		t.Errorf("overlapping operations=%d", overlap.Load())
	}
}

func TestWaitingForAccountDoesNotConsumeAllWorkerCapacity(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{Concurrency: 2})
	firstEntered, duplicateDiscovered, otherEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var discoveries atomic.Int32
	a := &behaviorAdapter{discover: func(_ context.Context, s taskengine.Scope) ([]taskengine.Task, error) {
		if s.AccountIDs[0] == "account-a" && discoveries.Add(1) == 2 {
			close(duplicateDiscovered)
		}
		return []taskengine.Task{syntheticTask("fake", s.AccountIDs[0], "task-a")}, nil
	}, execute: func(ctx context.Context, task taskengine.Task) (taskengine.Observation, error) {
		if task.AccountID == "account-a" {
			close(firstEntered)
			<-ctx.Done()
			return taskengine.Observation{}, ctx.Err()
		}
		close(otherEntered)
		return phase("pending"), nil
	}}
	registerBehavior(t, e, "fake", a)
	first := submitBehavior(t, e, taskengine.Request{ProviderID: "fake", AccountIDs: []string{"account-a"}})
	awaitBehaviorSignal(t, firstEntered)
	second := submitBehavior(t, e, taskengine.Request{ProviderID: "fake", AccountIDs: []string{"account-a"}})
	awaitBehaviorSignal(t, duplicateDiscovered)
	third := submitBehavior(t, e, taskengine.Request{ProviderID: "fake", AccountIDs: []string{"account-b"}})
	select {
	case <-otherEntered:
	case <-time.After(250 * time.Millisecond):
		t.Error("same-account waiter exhausted worker capacity and blocked an independent account")
	}
	e.Cancel(first.ID)
	e.Cancel(second.ID)
	e.Cancel(third.ID)
}

func TestUncertaintyCannotBeOverwrittenByALaterTaskFailure(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	a := &behaviorAdapter{discover: func(context.Context, taskengine.Scope) ([]taskengine.Task, error) {
		return []taskengine.Task{syntheticTask("fake", "account-a", "uncertain"), syntheticTask("fake", "account-b", "failed")}, nil
	}, inspect: func(_ context.Context, task taskengine.Task) (taskengine.Observation, error) {
		if task.Key == "failed" {
			return taskengine.Observation{}, errors.New("synthetic-read-failure")
		}
		return phase("ready"), nil
	}, execute: func(context.Context, taskengine.Task) (taskengine.Observation, error) {
		return taskengine.Observation{}, errors.New("synthetic-write-outcome-unknown")
	}}
	registerBehavior(t, e, "fake", a)
	r := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
	if r.Status != taskengine.StatusUncertain {
		t.Errorf("aggregate=%s, want uncertain while any mutation is unconfirmed", r.Status)
	}
}
