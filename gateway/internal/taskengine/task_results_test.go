package taskengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type taskResultsFixture struct {
	tasks   []Task
	unknown bool
}

func (f *taskResultsFixture) Discover(_ context.Context, scope Scope) ([]Task, error) {
	var tasks []Task
	for _, task := range f.tasks {
		if len(scope.AccountIDs) == 0 || contains(scope.AccountIDs, task.AccountID) {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}
func (f *taskResultsFixture) Inspect(context.Context, Task) (Observation, error) {
	return Observation{Phase: "claimable"}, nil
}
func (f *taskResultsFixture) Execute(context.Context, Task) (Observation, error) {
	return Observation{}, errors.New("synthetic unexpected execute")
}
func (f *taskResultsFixture) Claim(_ context.Context, task Task) (Observation, error) {
	if f.unknown {
		return UnknownAction(task, StageClaim, Observation{Phase: "pending"}), errors.New("synthetic response lost")
	}
	if task.AccountID == "account-4" || task.AccountID == "account-5" {
		return Rejected(task, StageClaim, 9074, Observation{Phase: "manual"}), nil
	}
	return Accepted(task, StageClaim, Observation{Phase: "pending"}), nil
}
func (f *taskResultsFixture) Verify(context.Context, Task) (Observation, error) {
	return Observation{Phase: "done"}, nil
}

func resultsTestTask(account string) Task {
	return Task{Definition: Definition{ProviderID: "stats", Key: "checkin", PeriodKey: "synthetic-day", SideEffect: true, OncePerDay: true}, Target: Target{AccountID: account}}
}

func resultsTestEngine(t *testing.T, path string) *Engine {
	t.Helper()
	if path == "" {
		path = filepath.Join(t.TempDir(), "tasks.json")
	}
	e, err := Open(Options{Path: path, PollMin: time.Millisecond, PollMax: time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func resultsWait(t *testing.T, e *Engine, id string) Run {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		for _, run := range e.Runs() {
			if run.ID == id && run.FinishedAt != nil {
				return run
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("synthetic run did not finish")
		case <-ticker.C:
		}
	}
}

func TestTaskResultsRecordSixAccountFinalOutcomes(t *testing.T) {
	e := resultsTestEngine(t, "")
	f := &taskResultsFixture{}
	for i := 0; i < 6; i++ {
		f.tasks = append(f.tasks, resultsTestTask(fmt.Sprintf("account-%d", i)))
	}
	if err := e.Register("stats", f); err != nil {
		t.Fatal(err)
	}
	run, err := e.Submit(Request{ProviderID: "stats", TaskKey: "checkin"})
	if err != nil {
		t.Fatal(err)
	}
	run = resultsWait(t, e, run.ID)
	if !run.TaskResultTracking || len(run.TaskResults) != 6 {
		t.Fatalf("missing final outcomes: tracking=%v count=%d", run.TaskResultTracking, len(run.TaskResults))
	}
	counts := map[Status]int{}
	for _, result := range run.TaskResults {
		counts[result.Status]++
	}
	if counts[StatusSucceeded] != 4 || counts[StatusManual] != 2 || len(counts) != 2 || run.Status != StatusManual {
		t.Fatalf("final outcomes manufactured success or changed business status: %+v run=%s", counts, run.Status)
	}
}

func TestTaskResultsUnknownDoesNotBecomeSuccessOrClearEvidence(t *testing.T) {
	e := resultsTestEngine(t, "")
	task := resultsTestTask("account-0")
	if err := e.Register("stats", &taskResultsFixture{tasks: []Task{task}, unknown: true}); err != nil {
		t.Fatal(err)
	}
	run, err := e.Submit(Request{ProviderID: "stats", TaskKey: "checkin", AccountIDs: []string{task.AccountID}})
	if err != nil {
		t.Fatal(err)
	}
	run = resultsWait(t, e, run.ID)
	if run.Status != StatusUncertain || len(run.TaskResults) != 1 || run.TaskResults[0].Status != StatusUncertain {
		t.Fatalf("unknown task did not remain unknown: %+v", run.TaskResults)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.state.Uncertain) != 1 || len(e.state.PendingTasks) != 1 || len(e.state.Attempts) != 1 {
		t.Fatal("result projection changed protected action evidence")
	}
}

func resultProjectionRun(task Task) Run {
	now := time.Now().UTC()
	return Run{ID: "projection-run", ProviderID: task.ProviderID, TaskKey: task.Key, AccountIDs: []string{task.AccountID}, TaskResultTracking: true, Status: StatusManual, StartedAt: now, FinishedAt: &now}
}

func TestTaskResultsProjectionFailureDoesNotChangeBusinessReturn(t *testing.T) {
	e := resultsTestEngine(t, "")
	task := resultsTestTask("account-0")
	if err := e.commit(func(state *persisted) {
		run := resultProjectionRun(task)
		run.Status, run.FinishedAt = StatusRunning, nil
		state.Runs = append(state.Runs, run)
		if state.Work == nil {
			state.Work = map[string]runWork{}
		}
		state.Work[run.ID] = runWork{Status: StatusSucceeded, Confirmed: map[string]bool{taskKey(task): true}}
	}); err != nil {
		t.Fatal(err)
	}
	// Fail only the additional diagnostic write after a previously confirmed
	// task. The common seam must preserve the original succeeded,nil return.
	if err := os.Mkdir(e.opts.Path+".pending", 0700); err != nil {
		t.Fatal(err)
	}
	status, err := e.runAdmittedTask(context.Background(), "projection-run", task, nil)
	if status != StatusSucceeded || err != nil {
		t.Fatalf("log storage failure changed business return: %s %v", status, err)
	}
	run := e.Runs()[0]
	if len(run.TaskResults) != 0 || !e.state.Work[run.ID].Confirmed[taskKey(task)] {
		t.Fatal("failed projection fabricated results or changed confirmation")
	}
}

func TestTaskResultsUpsertAndUnknownPrioritySurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := resultsTestEngine(t, path)
	task := resultsTestTask("account-0")
	if err := e.commit(func(state *persisted) { state.Runs = append(state.Runs, resultProjectionRun(task)) }); err != nil {
		t.Fatal(err)
	}
	for _, status := range []Status{StatusManual, StatusManual, StatusSucceeded} {
		if err := e.recordTaskResult("projection-run", task, status); err != nil {
			t.Fatal(err)
		}
	}
	if run := e.Runs()[0]; len(run.TaskResults) != 1 || run.TaskResults[0].Status != StatusSucceeded {
		t.Fatal("reconciliation appended duplicate result instances")
	}
	task.actionAttempt = "0123456789abcdef0123456789abcdef"
	task.actionStage = StageClaim
	if err := e.commit(func(state *persisted) {
		key := taskKey(task)
		state.Uncertain[key] = StageClaim
		if state.PendingTasks == nil {
			state.PendingTasks = map[string]Task{}
		}
		if state.Attempts == nil {
			state.Attempts = map[string]actionReceipt{}
		}
		state.PendingTasks[key] = task
		state.Attempts[key] = actionReceipt{Attempt: task.actionAttempt, Stage: StageClaim, Binding: actionBinding(task, StageClaim)}
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.recordTaskResult("projection-run", task, StatusSucceeded); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = resultsTestEngine(t, path)
	run := e.Runs()[0]
	if len(run.TaskResults) != 1 || run.TaskResults[0].Status != StatusUncertain || len(e.state.Uncertain) != 1 || len(e.state.Attempts) != 1 {
		t.Fatal("restart lost unique results or uncertainty priority")
	}
}

func TestTaskResultsLimitDoesNotEvictOrAffectExistingResults(t *testing.T) {
	e := resultsTestEngine(t, "")
	task := resultsTestTask("account-0")
	if err := e.commit(func(state *persisted) {
		run := resultProjectionRun(task)
		for i := 0; i < maxTaskResults; i++ {
			run.TaskResults = append(run.TaskResults, TaskResult{AccountID: task.AccountID, TaskKey: task.Key, PeriodKey: fmt.Sprintf("period-%d", i), Status: StatusUncertain})
		}
		state.Runs = append(state.Runs, run)
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.recordTaskResult("projection-run", task, StatusManual); err != nil {
		t.Fatal(err)
	}
	task.PeriodKey = "period-0"
	if err := e.recordTaskResult("projection-run", task, StatusSucceeded); err != nil {
		t.Fatal(err)
	}
	run := e.Runs()[0]
	if len(run.TaskResults) != maxTaskResults || run.TaskResults[0].Status != StatusSucceeded || run.TaskResults[maxTaskResults-1].Status != StatusUncertain {
		t.Fatal("bound evicted prior results or prevented an existing instance update")
	}
}

func TestTaskResultsStoreValidationAndLegacyCompatibility(t *testing.T) {
	task := resultsTestTask("account-0")
	valid := TaskResult{AccountID: task.AccountID, TaskKey: task.Key, PeriodKey: task.PeriodKey, Status: StatusSucceeded}
	for _, tc := range []struct {
		name      string
		change    func(*Run)
		wantError bool
	}{
		{"legacy", func(run *Run) { run.TaskResultTracking = false; run.TaskResults = nil }, false},
		{"valid", func(*Run) {}, false},
		{"account", func(run *Run) { run.TaskResults[0].AccountID = "account-else" }, true},
		{"task", func(run *Run) { run.TaskResults[0].TaskKey = "balance" }, true},
		{"running", func(run *Run) { run.TaskResults[0].Status = StatusRunning }, true},
		{"unknown-status", func(run *Run) { run.TaskResults[0].Status = "fabricated" }, true},
		{"duplicate", func(run *Run) { run.TaskResults = append(run.TaskResults, valid) }, true},
		{"bound", func(run *Run) {
			for i := 0; i < maxTaskResults; i++ {
				run.TaskResults = append(run.TaskResults, valid)
			}
		}, true},
		{"period", func(run *Run) { run.Trigger = "retry"; run.ExpectedPeriodKey = "another-period" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := newState()
			run := resultProjectionRun(task)
			run.TaskResults = []TaskResult{valid}
			tc.change(&run)
			state.Runs = append(state.Runs, run)
			path := filepath.Join(t.TempDir(), "tasks.json")
			if err := writeState(path, state); err != nil {
				t.Fatal(err)
			}
			_, err := readState(path)
			if (err != nil) != tc.wantError {
				t.Fatalf("store validation mismatch: %v", err)
			}
		})
	}
}

func TestTaskResultsDiagnosticTailDoesNotManufactureCompletedTask(t *testing.T) {
	e := resultsTestEngine(t, "")
	task := resultsTestTask("account-0")
	task.Key, task.OncePerDay, task.SideEffect, task.Idempotent = "balance", false, false, true
	f := &taskResultsFixture{tasks: []Task{task}}
	if err := e.Register("stats", f); err != nil {
		t.Fatal(err)
	}
	run := resultProjectionRun(task)
	run.TaskKey, run.TaskSequence = "checkin-balance", []string{"checkin", "balance"}
	run.Status, run.FinishedAt = StatusRunning, nil
	work := runWork{Status: StatusSkipped, SequenceAccounts: []string{task.AccountID}, SequenceIndex: 1}
	if err := e.commit(func(state *persisted) {
		state.Runs = append(state.Runs, run)
		if state.Work == nil {
			state.Work = map[string]runWork{}
		}
		state.Work[run.ID] = work
	}); err != nil {
		t.Fatal(err)
	}
	e.inspectSequenceTail(context.Background(), run, f, &work, task.AccountID, 1)
	run = e.Runs()[0]
	if len(run.Steps) != 1 || run.Steps[0].Stage != StageInspect || len(run.TaskResults) != 0 {
		t.Fatal("read-only diagnostic tail was promoted into a completed task result")
	}
}
