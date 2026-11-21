package taskengine

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDeferredRestartCannotRediscoverANewBusinessPeriod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	state := newState()
	state.Runs = []Run{{ID: "reserved-retry", ProviderID: "synthetic", TaskKey: "reward", Trigger: "retry", ExpectedPeriodKey: "previous-day", AccountIDs: []string{"account"}, Status: StatusQueued, StartedAt: time.Now().UTC()}}
	state.Work["reserved-retry"] = runWork{Status: StatusSkipped}
	if err := writeState(path, state); err != nil {
		t.Fatal(err)
	}
	e, err := Open(Options{Path: path, Timeout: time.Second, PollMin: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	task := actionTask()
	task.PeriodKey = "new-day"
	a := &actionFixture{task: task}
	if err = e.Register("synthetic", a); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		runs := e.Runs()
		if len(runs) == 1 && runs[0].FinishedAt != nil {
			if runs[0].ExpectedPeriodKey != "previous-day" || runs[0].Status != StatusSkipped || a.calls != 0 {
				t.Fatalf("restart changed recovery period or wrote new-day action: %+v claims=%d", runs[0], a.calls)
			}
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("resumed retry did not finish")
		case <-tick.C:
		}
	}
}

func TestDeferredCheckpointWithAnotherPeriodFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	state := newState()
	state.Runs = []Run{{ID: "reserved-retry", ProviderID: "synthetic", TaskKey: "reward", Trigger: "retry", ExpectedPeriodKey: "previous-day", AccountIDs: []string{"account"}, Status: StatusQueued, StartedAt: time.Now().UTC()}}
	task := actionTask()
	task.PeriodKey = "new-day"
	state.Work["reserved-retry"] = runWork{Status: StatusSkipped, Tasks: []Task{task}}
	if err := writeState(path, state); err != nil {
		t.Fatal(err)
	}
	if _, err := readState(path); err == nil || err.Error() != "task_work_period_invalid" {
		t.Fatalf("wrong-period checkpoint accepted: %v", err)
	}
}
