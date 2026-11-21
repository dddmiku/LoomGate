package taskengine_test

import (
	"context"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

func TestDrainFinishesAcceptedTaskAndRejectsNewRuns(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	task := syntheticTask("fake", "account-a", "once")
	task.Stages = []taskengine.Stage{taskengine.StageExecute, taskengine.StageVerify}
	entered, release := make(chan struct{}), make(chan struct{})
	a := singleAdapter(task)
	a.execute = func(ctx context.Context, task taskengine.Task) (taskengine.Observation, error) {
		close(entered)
		<-release
		if ctx.Err() != nil {
			return taskengine.Observation{}, ctx.Err()
		}
		return taskengine.Accepted(task, taskengine.StageExecute, phase("pending")), nil
	}
	registerBehavior(t, e, "fake", a)
	run := submitBehavior(t, e, taskengine.Request{ProviderID: "fake"})
	<-entered
	e.Quiesce()
	if _, err := e.Submit(taskengine.Request{ProviderID: "fake"}); err == nil {
		t.Fatal("draining engine accepted a new run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Drain(ctx) }()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := awaitBehaviorRun(t, e, run.ID); got.Status != taskengine.StatusSucceeded {
		t.Fatalf("accepted task was canceled instead of drained: %s", got.Status)
	}
}
