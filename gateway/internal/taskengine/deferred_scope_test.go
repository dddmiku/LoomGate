package taskengine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

type deferredScopeEvidence struct {
	Uncertain    map[string]json.RawMessage `json:"uncertain"`
	PendingTasks map[string]json.RawMessage `json:"pending_tasks"`
	Attempts     map[string]json.RawMessage `json:"attempts"`
}

func readDeferredScopeEvidence(t *testing.T, path string) deferredScopeEvidence {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state deferredScopeEvidence
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestDeferredBroadPlanUnknownAccountDoesNotBlockAnotherAccount(t *testing.T) {
	for _, scope := range []struct {
		name string
		ids  []string
	}{
		{name: "all-accounts"},
		{name: "multiple-accounts", ids: []string{"account-a", "account-b"}},
	} {
		t.Run(scope.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tasks.json")
			var aExecutes, bExecutes, reservations atomic.Int32
			base := &behaviorAdapter{}
			base.discover = func(_ context.Context, requested taskengine.Scope) ([]taskengine.Task, error) {
				var tasks []taskengine.Task
				for _, account := range []string{"account-a", "account-b"} {
					included := len(requested.AccountIDs) == 0
					for _, id := range requested.AccountIDs {
						included = included || id == account
					}
					if !included {
						continue
					}
					task := syntheticTask("fake", account, "checkin")
					task.OncePerDay = true
					task.PeriodKey = "synthetic-business-day"
					task.Stages = []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify}
					tasks = append(tasks, task)
				}
				return tasks, nil
			}
			base.execute = func(_ context.Context, task taskengine.Task) (taskengine.Observation, error) {
				if task.AccountID == "account-a" {
					aExecutes.Add(1)
					return taskengine.UnknownAction(task, taskengine.StageExecute, phase("pending")), errors.New("synthetic response lost")
				}
				bExecutes.Add(1)
				return taskengine.Accepted(task, taskengine.StageExecute, phase("pending")), nil
			}
			base.verify = func(_ context.Context, task taskengine.Task) (taskengine.Observation, error) {
				if task.AccountID == "account-a" {
					return phase("pending"), nil
				}
				return phase("done"), nil
			}
			adapter := &deferredAdapter{behaviorAdapter: base}
			adapter.take = func(_ context.Context, _ time.Time, plans []taskengine.Schedule) ([]taskengine.Request, error) {
				if len(plans) != 1 || plans[0].ID != "fake-plan" || !reflect.DeepEqual(plans[0].AccountIDs, scope.ids) {
					t.Errorf("pending account hid or changed the broad plan: %+v", plans)
					return nil, nil
				}
				reservations.Add(1)
				return []taskengine.Request{{ProviderID: "fake", TaskKey: "checkin", ScheduleID: "fake-plan", AccountIDs: []string{"account-b"}, ExpectedPeriodKey: "synthetic-business-day"}}, nil
			}
			e := openBehaviorEngine(t, taskengine.Options{Path: path})
			if err := e.Register("fake", adapter); err != nil {
				t.Fatal(err)
			}
			original := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", TaskKey: "checkin", AccountIDs: []string{"account-a"}}).ID)
			if original.Status != taskengine.StatusUncertain || aExecutes.Load() != 1 {
				t.Fatalf("account A did not create a real unknown execution: status=%s executes=%d", original.Status, aExecutes.Load())
			}
			// Close only after the genuine unknown action. Reopening makes the
			// retained evidence independent of worker cleanup timing and proves
			// the all/multi-account rule also survives durable recovery.
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			before := readDeferredScopeEvidence(t, path)
			if len(before.Uncertain) != 1 || len(before.PendingTasks) != 1 || len(before.Attempts) != 1 {
				t.Fatal("unknown action did not retain uncertainty, pending task and attempt")
			}
			for key, raw := range before.PendingTasks {
				var pending taskengine.Task
				if err := json.Unmarshal(raw, &pending); err != nil {
					t.Fatal(err)
				}
				if pending.AccountID != "account-a" || pending.Key != "checkin" || before.Uncertain[key] == nil || before.Attempts[key] == nil {
					t.Fatal("retained evidence does not belong to account A's original checkin")
				}
			}
			e = openBehaviorEngine(t, taskengine.Options{Path: path})
			if err := e.Register("fake", adapter); err != nil {
				t.Fatal(err)
			}
			plan, err := e.SaveSchedule(taskengine.Schedule{ID: "fake-plan", ProviderID: "fake", TaskKey: "checkin", Enabled: true, Hours: []int{0}, Minute: 5, Timezone: "Asia/Shanghai", AccountIDs: scope.ids, OncePerDay: true})
			if err != nil || plan.NextRun == nil {
				t.Fatalf("could not save the broad recovery plan: %v", err)
			}
			// Keep the ordinary dispatch in the future: only the deferred path
			// may create B's run, rather than a broad scheduled execution.
			if err = e.Tick(plan.NextRun.Add(-time.Nanosecond)); err != nil {
				t.Fatal(err)
			}
			if reservations.Load() != 1 || len(e.Runs()) != 2 {
				t.Fatalf("account A blocked B's recovery: reservations=%d runs=%d", reservations.Load(), len(e.Runs()))
			}
			var retry taskengine.Run
			for _, run := range e.Runs() {
				if run.ID != original.ID {
					retry = run
				}
			}
			retry = awaitBehaviorRun(t, e, retry.ID)
			if retry.Status != taskengine.StatusSucceeded || retry.Trigger != "retry" || retry.ScheduleID != plan.ID || !reflect.DeepEqual(retry.AccountIDs, []string{"account-b"}) {
				t.Fatalf("account B did not finish through the exact deferred scope: %+v", retry)
			}
			if aExecutes.Load() != 1 || bExecutes.Load() != 1 {
				t.Fatalf("recovery replayed A or did not execute B once: A=%d B=%d", aExecutes.Load(), bExecutes.Load())
			}
			if err = e.Close(); err != nil {
				t.Fatal(err)
			}
			if after := readDeferredScopeEvidence(t, path); !reflect.DeepEqual(before, after) {
				t.Fatal("B's recovery changed A's unresolved uncertainty, pending task or attempt evidence")
			}
		})
	}
}
