package taskengine_test

import (
	"context"
	"testing"
	"time"
	"workbuddy2api/internal/taskengine"
)

func TestTaskBudgetOptInAndCancel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allow   bool
		seconds int
		want    time.Duration
	}{{"default", false, 660, 3 * time.Second}, {"work", true, 660, 660 * time.Second}, {"invalid", true, 901, 3 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			e := openBehaviorEngine(t, taskengine.Options{Timeout: 3 * time.Second, AllowTaskTimeout: tc.allow})
			task := syntheticTask("budget", "account", "work")
			task.TimeoutSeconds = tc.seconds
			a := singleAdapter(task)
			entered := make(chan time.Duration, 1)
			a.inspect = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					entered <- 0
				} else {
					entered <- time.Until(deadline)
				}
				<-ctx.Done()
				return taskengine.Observation{}, ctx.Err()
			}
			registerBehavior(t, e, "budget", a)
			run := submitBehavior(t, e, taskengine.Request{ProviderID: "budget"})
			select {
			case budget := <-entered:
				if budget > tc.want || budget < tc.want-time.Second {
					t.Fatal("wrong task budget")
				}
			case <-time.After(20 * time.Second):
				t.Fatal("adapter not entered")
			}
			if !e.Cancel(run.ID) {
				t.Fatal("work not cancelable")
			}
			if done := awaitBehaviorRun(t, e, run.ID); done.Status != taskengine.StatusCanceled {
				t.Fatal("cancel lost")
			}
		})
	}
}
