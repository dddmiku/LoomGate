package taskengine_test

import (
	"context"
	"testing"
	"time"
	"workbuddy2api/internal/taskengine"
)

type gatedAdapter struct {
	taskengine.Adapter
	acquire func(context.Context, taskengine.Task) (func(), error)
}

func (g *gatedAdapter) AcquireTask(ctx context.Context, t taskengine.Task) (func(), error) {
	return g.acquire(ctx, t)
}

func TestWaitingProviderDoesNotHoldWorkerOrBlockUnrelatedProvider(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{Concurrency: 1, Timeout: 50 * time.Millisecond})
	entered := make(chan struct{})
	a := singleAdapter(syntheticTask("waiting", "first", "paced"))
	gate := &gatedAdapter{Adapter: a, acquire: func(ctx context.Context, _ taskengine.Task) (func(), error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	if err := e.Register("waiting", gate); err != nil {
		t.Fatal(err)
	}
	read := singleAdapter(syntheticTask("other", "second", "balance"))
	read.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) { return phase("done"), nil }
	registerBehavior(t, e, "other", read)
	blocked := submitBehavior(t, e, taskengine.Request{ProviderID: "waiting"})
	<-entered
	run := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "other"}).ID)
	if run.Status != taskengine.StatusSucceeded {
		t.Fatal("waiting gate exhausted global worker")
	}
	if !e.Cancel(blocked.ID) {
		t.Fatal("waiting work not cancelable")
	}
	run = awaitBehaviorRun(t, e, blocked.ID)
	if run.Status != taskengine.StatusCanceled || len(run.Steps) != 0 {
		t.Fatal("waiting was treated as submitted work")
	}
}

func TestGateFailureRemainsIsolatedFromFollowingAccount(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	a := &behaviorAdapter{discover: func(context.Context, taskengine.Scope) ([]taskengine.Task, error) {
		return []taskengine.Task{syntheticTask("gated", "first", "read"), syntheticTask("gated", "second", "read")}, nil
	}, inspect: func(context.Context, taskengine.Task) (taskengine.Observation, error) { return phase("done"), nil }}
	second := false
	gate := &gatedAdapter{Adapter: a, acquire: func(_ context.Context, t taskengine.Task) (func(), error) {
		if t.AccountID == "first" {
			panic("synthetic-gate")
		}
		second = true
		return func() {}, nil
	}}
	if err := e.Register("gated", gate); err != nil {
		t.Fatal(err)
	}
	run := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "gated"}).ID)
	if run.Status != taskengine.StatusFailed || !second {
		t.Fatal("gate panic lost following account")
	}
}
