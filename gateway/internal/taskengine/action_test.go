package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type actionFixture struct {
	task     Task
	action   func(Task) (Observation, error)
	verified Stage
	verify   func(Task) (Observation, error)
	calls    int
}

func (a *actionFixture) Discover(context.Context, Scope) ([]Task, error) { return []Task{a.task}, nil }
func (a *actionFixture) Inspect(context.Context, Task) (Observation, error) {
	return Observation{Phase: "claimable"}, nil
}
func (a *actionFixture) Execute(context.Context, Task) (Observation, error) {
	return Observation{}, errors.New("unused")
}
func (a *actionFixture) Claim(_ context.Context, t Task) (Observation, error) {
	a.calls++
	return a.action(t)
}
func (a *actionFixture) Verify(_ context.Context, t Task) (Observation, error) {
	a.verified = t.AcceptedStage
	if a.verify != nil {
		return a.verify(t)
	}
	return Observation{Phase: "claimable"}, nil
}
func actionEngine(t *testing.T, path string, a *actionFixture) *Engine {
	t.Helper()
	e, err := Open(Options{Path: path, PollMin: time.Millisecond, PollMax: time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Register("synthetic", a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}
func actionRun(t *testing.T, e *Engine) Run {
	t.Helper()
	r, err := e.Submit(Request{ProviderID: "synthetic", TaskKey: "reward", AccountIDs: []string{"account"}})
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		for _, run := range e.Runs() {
			if run.ID == r.ID && run.FinishedAt != nil {
				return run
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("run not finished")
	return Run{}
}
func actionTask() Task {
	return Task{Definition: Definition{ProviderID: "synthetic", Key: "reward", PeriodKey: "period", SideEffect: true}, Target: Target{AccountID: "account"}}
}

func TestActionEvidenceNeverInferredFromErrorOrPhase(t *testing.T) {
	for _, phase := range []string{"pending", "manual", "blocked", "done"} {
		t.Run(phase, func(t *testing.T) {
			a := &actionFixture{task: actionTask(), action: func(Task) (Observation, error) { return Observation{Phase: phase}, nil }}
			e := actionEngine(t, filepath.Join(t.TempDir(), "tasks.json"), a)
			defer e.Close()
			if actionRun(t, e).Status != StatusUncertain || a.verified != "" {
				t.Fatal("nil error or phase manufactured receipt")
			}
		})
	}
}
func TestExplicitNoSubmissionReleasesOnlyThisAttemptAcrossRestart(t *testing.T) {
	for _, phase := range []string{"manual", "blocked", "done", "pending"} {
		t.Run(phase, func(t *testing.T) {
			a := &actionFixture{task: actionTask(), action: func(task Task) (Observation, error) {
				return NotSubmitted(task, StageClaim, Observation{Phase: phase}), nil
			}}
			path := filepath.Join(t.TempDir(), "tasks.json")
			e := actionEngine(t, path, a)
			result := actionRun(t, e)
			if result.Status == StatusSucceeded || result.Status == StatusUncertain || len(e.state.Uncertain) != 0 || len(e.state.Receipts) != 0 {
				t.Fatal("no-op created success or permanent uncertainty")
			}
			e.Close()
			e = actionEngine(t, path, a)
			defer e.Close()
			actionRun(t, e)
			if a.calls != 2 {
				t.Fatal("proven no-submission blocked later explicit run")
			}
		})
	}
}
func TestAcceptedEvidenceBindsAccountPeriodActionAndAttempt(t *testing.T) {
	for _, mismatch := range []string{"account", "period", "key", "stage", "attempt"} {
		t.Run(mismatch, func(t *testing.T) {
			a := &actionFixture{task: actionTask(), action: func(task Task) (Observation, error) {
				stage := StageClaim
				switch mismatch {
				case "account":
					task.AccountID = "foreign"
				case "period":
					task.PeriodKey = "other"
				case "key":
					task.Key = "other"
				case "stage":
					stage = StageExecute
				case "attempt":
					task.actionAttempt = "old"
				}
				return Accepted(task, stage, Observation{Phase: "pending"}), nil
			}}
			e := actionEngine(t, filepath.Join(t.TempDir(), "tasks.json"), a)
			defer e.Close()
			if actionRun(t, e).Status != StatusUncertain || a.verified != "" || len(e.state.Receipts) != 0 {
				t.Fatal("foreign evidence accepted")
			}
		})
	}
}
func TestV1AcceptedStageAndCompletionCacheRequireReconciliation(t *testing.T) {
	task := actionTask()
	task.AcceptedStage = StageClaim
	key := taskKey(task)
	old := persisted{Version: 1, Schedules: map[string]Schedule{}, Runs: []Run{}, Completed: map[string]time.Time{"synthetic-old": time.Now()}, Uncertain: map[string]Stage{key: StageClaim}, PendingTasks: map[string]Task{key: task}}
	path := filepath.Join(t.TempDir(), "tasks.json")
	raw, _ := json.Marshal(old)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a := &actionFixture{task: task, action: func(Task) (Observation, error) { t.Fatal("legacy unknown replayed"); return Observation{}, nil }}
	e := actionEngine(t, path, a)
	defer e.Close()
	if e.state.Version != 2 || len(e.state.Completed) != 0 || len(e.state.LegacyCompleted) != 1 || e.state.PendingTasks[key].AcceptedStage != "" || len(e.state.Uncertain) != 1 {
		t.Fatal("v1 trust was silently upgraded")
	}
	if actionRun(t, e).Status != StatusUncertain {
		t.Fatal("v1 unknown was cleared")
	}
}
func TestAcceptedButCanceledMutationKeepsBoundReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	called := make(chan struct{})
	release := make(chan struct{})
	a := &actionFixture{task: actionTask(), action: func(task Task) (Observation, error) {
		close(called)
		<-release
		return Accepted(task, StageClaim, Observation{Phase: "pending"}), nil
	}}
	e := actionEngine(t, path, a)
	run, err := e.Submit(Request{ProviderID: "synthetic", TaskKey: "reward"})
	if err != nil {
		t.Fatal(err)
	}
	<-called
	e.Cancel(run.ID)
	close(release)
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if rows := e.Runs(); len(rows) == 1 && rows[0].FinishedAt != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if rows := e.Runs(); len(rows) != 1 || rows[0].Status != StatusUncertain {
		t.Fatal("canceled accepted request claimed success")
	}
	e.Close()
	e = actionEngine(t, path, a)
	defer e.Close()
	pending := e.state.PendingTasks[taskKey(a.task)]
	if receiptStage(pending, e.state.Receipts[taskKey(a.task)]) != StageClaim {
		t.Fatal("cancellation lost actual acceptance evidence")
	}
}

func TestNoSubmissionAfterCallerCancellationDoesNotLeaveUnknown(t *testing.T) {
	called, release := make(chan struct{}), make(chan struct{})
	a := &actionFixture{task: actionTask(), action: func(task Task) (Observation, error) {
		close(called)
		<-release
		return NotSubmitted(task, StageClaim, Observation{Phase: "manual"}), context.Canceled
	}}
	e := actionEngine(t, filepath.Join(t.TempDir(), "tasks.json"), a)
	run, err := e.Submit(Request{ProviderID: "synthetic", TaskKey: "reward"})
	if err != nil {
		t.Fatal(err)
	}
	<-called
	e.Cancel(run.ID)
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if rows := e.Runs(); len(rows) == 1 && rows[0].FinishedAt != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.state.Uncertain) != 0 || len(e.state.PendingTasks) != 0 || len(e.state.Receipts) != 0 {
		t.Fatal("proven canceled before submission left an unknown write")
	}
}

func TestReceiptTamperingAndV1UnknownWithoutMetadataFailClosed(t *testing.T) {
	task := actionTask()
	task.actionAttempt = "0123456789abcdef0123456789abcdef"
	key := taskKey(task)
	state := newState()
	state.PendingTasks[key] = task
	state.Uncertain[key] = StageClaim
	state.Receipts[key] = actionReceipt{Attempt: task.actionAttempt, Stage: StageClaim, Binding: "wrong-binding"}
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := writeState(path, state); err != nil {
		t.Fatal(err)
	}
	if e, err := Open(Options{Path: path}); err == nil {
		e.Close()
		t.Fatal("foreign receipt was trusted")
	}
	state = newState()
	state.Version = 1
	state.Uncertain[key] = StageClaim
	state.PendingTasks = nil
	if err := writeState(path, state); err != nil {
		t.Fatal(err)
	}
	a := &actionFixture{task: task, action: func(Task) (Observation, error) { t.Fatal("old unknown replayed"); return Observation{}, nil }}
	e := actionEngine(t, path, a)
	if actionRun(t, e).Status != StatusUncertain || len(e.state.Receipts) != 0 {
		t.Fatal("old unknown was upgraded to accepted")
	}
}
