package taskengine

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDrainPreservesUnknownPendingAttemptAndWorkCursorWithoutReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	task := actionTask()
	key := taskKey(task)
	attempt := "00000000000000000000000000000001"
	bound := task
	bound.actionAttempt = attempt
	s := newState()
	s.Uncertain[key] = StageClaim
	s.PendingTasks[key] = task
	s.Attempts[key] = actionReceipt{Attempt: attempt, Stage: StageClaim, Binding: actionBinding(bound, StageClaim)}
	s.Completed["retained-completed"] = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := Task{Definition: Definition{ProviderID: "held", Key: "done"}, Target: Target{AccountID: "other"}}
	second := first
	second.Key = "later"
	s.Runs = []Run{{ID: "held-run", ProviderID: "held", TaskKey: "*", AccountIDs: []string{"other"}, Status: StatusQueued}}
	s.Work["held-run"] = runWork{Tasks: []Task{first, second}, Next: 1, Status: StatusSkipped}
	if err := writeState(path, s); err != nil {
		t.Fatal(err)
	}
	e, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := readState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Completed, after.Completed) || !reflect.DeepEqual(s.Uncertain, after.Uncertain) || !reflect.DeepEqual(s.PendingTasks, after.PendingTasks) || !reflect.DeepEqual(s.Attempts, after.Attempts) || after.Work["held-run"].Next != 1 {
		t.Fatal("drain changed completion, unknown evidence, pending attempt, or saved work cursor")
	}
	a := &actionFixture{task: task, action: func(Task) (Observation, error) {
		t.Error("unknown mutation was replayed after the update drain")
		return Observation{}, nil
	}}
	reopened := actionEngine(t, path, a)
	if run := actionRun(t, reopened); run.Status != StatusUncertain || a.calls != 0 {
		t.Fatal("previous unknown mutation was replayed or falsely completed")
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
	after, err = readState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Uncertain) != 1 || len(after.PendingTasks) != 1 || len(after.Attempts) != 1 || after.Work["held-run"].Next != 1 {
		t.Fatal("restart lost the unknown attempt or pending cursor")
	}
}
