package taskengine_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

const leaseHelperMode = "LOOMGATE_TASK_TEST_HELPER_MODE"
const leaseHelperPath = "LOOMGATE_TASK_TEST_HELPER_PATH"
const leaseHelperMarker = "LOOMGATE_TASK_TEST_HELPER_MARKER"

// The helper is always a child copy of this test executable. It uses only synthetic
// tasks and a test-owned directory, and never contacts a provider or production service.
func TestTaskEngineLeaseProcessHelper(t *testing.T) {
	mode := os.Getenv(leaseHelperMode)
	if mode == "" {
		t.Skip("subprocess helper")
	}
	e, err := taskengine.Open(taskengine.Options{Path: os.Getenv(leaseHelperPath), Timeout: time.Hour, PollMin: time.Millisecond})
	if mode == "expect-busy" {
		if !errors.Is(err, taskengine.ErrBusy) {
			if e != nil {
				_ = e.Close()
			}
			t.Fatalf("second process Open error = %v, want ErrBusy", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("helper Open: %v", err)
	}
	defer e.Close()
	if mode == "expect-open" {
		return
	}
	if mode != "crash-during-mutation" {
		t.Fatalf("unknown helper mode %q", mode)
	}
	task := syntheticTask("fake", "account-a", "once")
	task.Once = true
	a := singleAdapter(task)
	a.execute = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
		// Reaching Execute proves the write-ahead uncertainty record was committed.
		if err := os.WriteFile(os.Getenv(leaseHelperMarker), []byte("synthetic-mutation-entered"), 0600); err != nil {
			return taskengine.Observation{}, err
		}
		<-ctx.Done()
		return taskengine.Observation{}, ctx.Err()
	}
	if err := e.Register("fake", a); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(taskengine.Request{ProviderID: "fake"}); err != nil {
		t.Fatal(err)
	}
	<-time.After(time.Hour)
}

func helperCommand(ctx context.Context, mode, path, marker string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTaskEngineLeaseProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), leaseHelperMode+"="+mode, leaseHelperPath+"="+path, leaseHelperMarker+"="+marker)
	return cmd
}

func TestStoreLeaseExcludesOtherInstancesAndProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	other, err := taskengine.Open(taskengine.Options{Path: path})
	if other != nil {
		_ = other.Close()
	}
	if !errors.Is(err, taskengine.ErrBusy) {
		t.Fatalf("second instance error=%v, want ErrBusy", err)
	}
	for _, mode := range []string{"expect-busy", "expect-open"} {
		if mode == "expect-open" {
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		output, err := helperCommand(ctx, mode, path, "").CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s helper failed: %v\n%s", mode, err, output)
		}
	}
}

func TestAbruptProcessDeathRestoresUncertaintyAndDoesNotReplayMutation(t *testing.T) {
	dir := t.TempDir()
	path, marker := filepath.Join(dir, "state.json"), filepath.Join(dir, "entered.marker")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := helperCommand(ctx, "crash-during-mutation", path, marker)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("helper did not enter synthetic mutation before deadline")
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("helper context expired")
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill test helper: %v", err)
	}
	_ = cmd.Wait()
	waited = true
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	recovered := e.Runs()
	if len(recovered) != 1 || recovered[0].Status != taskengine.StatusUncertain || recovered[0].FinishedAt == nil {
		t.Fatalf("interrupted state was not recovered as uncertain: %+v", recovered)
	}
	task := syntheticTask("fake", "account-a", "once")
	task.Once = true
	var authoritativeDone atomic.Bool
	var executions atomic.Int32
	a := singleAdapter(task)
	a.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) {
		if authoritativeDone.Load() {
			return phase("done"), nil
		}
		return phase("pending"), nil
	}
	a.execute = func(context.Context, taskengine.Task) (taskengine.Observation, error) {
		executions.Add(1)
		return phase("pending"), nil
	}
	registerBehavior(t, e, "fake", a)
	retry := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
	if retry.Status != taskengine.StatusUncertain || executions.Load() != 0 {
		t.Fatalf("unknown mutation replayed or lost: status=%s executions=%d", retry.Status, executions.Load())
	}
	authoritativeDone.Store(true)
	reconciled := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
	if reconciled.Status != taskengine.StatusSucceeded || executions.Load() != 0 {
		t.Fatalf("authoritative completion did not reconcile: status=%s executions=%d", reconciled.Status, executions.Load())
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openBehaviorEngine(t, taskengine.Options{Path: path})
	registerBehavior(t, reopened, "fake", a)
	awaitBehaviorRun(t, reopened, submitBehavior(t, reopened, taskengine.Request{ProviderID: "fake"}).ID)
	if executions.Load() != 0 {
		t.Fatal("verified completion was lost after second restart")
	}
}

func TestVerifiedCompletionSurvivesRestartAndRemainsAccountScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	var executions atomic.Int32
	a := &behaviorAdapter{discover: func(_ context.Context, scope taskengine.Scope) ([]taskengine.Task, error) {
		task := syntheticTask("fake", scope.AccountIDs[0], "daily")
		task.OncePerDay, task.PeriodKey = true, "authoritative-day-a"
		return []taskengine.Task{task}, nil
	}, execute: func(context.Context, taskengine.Task) (taskengine.Observation, error) {
		executions.Add(1)
		return phase("pending"), nil
	}}
	registerBehavior(t, e, "fake", a)
	awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", AccountIDs: []string{"account-a"}}).ID)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = openBehaviorEngine(t, taskengine.Options{Path: path})
	registerBehavior(t, e, "fake", a)
	awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", AccountIDs: []string{"account-a"}}).ID)
	if executions.Load() != 1 {
		t.Fatal("completed account executed twice after restart")
	}
	awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", AccountIDs: []string{"account-b"}}).ID)
	if executions.Load() != 2 {
		t.Fatal("one account's completion incorrectly suppressed another account")
	}
}
