package taskengine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"workbuddy2api/internal/taskengine"
)

func TestTaskLogStatisticsCountAccountsInsteadOfSuccessfulSteps(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	ids := []string{}
	tasks := []taskengine.Task{}
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("account-%d", i)
		ids = append(ids, id)
		task := syntheticTask("fake", id, "checkin")
		task.OncePerDay, task.PeriodKey = true, "synthetic-business-day"
		tasks = append(tasks, task)
	}
	a := &behaviorAdapter{discover: func(context.Context, taskengine.Scope) ([]taskengine.Task, error) { return tasks, nil }}
	a.inspect = func(_ context.Context, task taskengine.Task) (taskengine.Observation, error) {
		if task.AccountID == "account-5" || task.AccountID == "account-6" {
			return phase("ready"), nil
		}
		return phase("done"), nil
	}
	a.execute = func(_ context.Context, task taskengine.Task) (taskengine.Observation, error) {
		return taskengine.Rejected(task, taskengine.StageExecute, 9074, phase("manual")), nil
	}
	registerBehavior(t, e, "fake", a)
	run := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", TaskKey: "checkin", AccountIDs: ids}).ID)
	if run.Status != taskengine.StatusManual {
		t.Fatalf("unexpected fixture outcome: %s", run.Status)
	}
	log, err := e.TaskLogFor("fake", "checkin")
	if err != nil {
		t.Fatal(err)
	}
	output := strings.Join(log.Lines, "\n")
	for _, want := range []string{"本轮统计：账号 6｜已核验 4｜失败 0｜需人工 2｜待核验 0｜跳过 0", "其中官方明确拒绝：2 个账号（9074 × 2）"} {
		if !strings.Contains(output, want) {
			t.Errorf("log lacks accurate account statistics %q", want)
		}
	}
}
