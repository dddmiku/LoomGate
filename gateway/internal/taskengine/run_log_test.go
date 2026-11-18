package taskengine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/taskengine"
)

func TestRunLogPersistsAdapterSummaryAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	task := syntheticTask("fake", "account-a", "balance")
	task.SideEffect = false
	a := singleAdapter(task)
	a.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) {
		return taskengine.Observation{Phase: "done", Summary: "余额已读取，账号池已更新。", Current: 42}, nil
	}
	registerBehavior(t, e, "fake", a)
	r := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", TaskKey: "balance"}).ID)
	if len(r.Steps) != 1 || r.Steps[0].Summary != "余额已读取，账号池已更新。" {
		t.Fatal("execution log lost the adapter result summary")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openBehaviorEngine(t, taskengine.Options{Path: path})
	if rows := reopened.Runs(); len(rows) != 1 || rows[0].Steps[0].Summary != r.Steps[0].Summary {
		t.Fatal("execution log did not survive restart")
	}
}

func TestRunLogRedactsSensitiveValuesAndNeverStoresRawErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	a := singleAdapter(syntheticTask("fake", "account-a", "task-a"))
	a.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) {
		return taskengine.Observation{Phase: "blocked", Summary: "连接未完成，token=TEST_ONLY_SENTINEL_VALUE；请重新登录。"}, errors.New("RAW_ERROR_TEST_SENTINEL")
	}
	registerBehavior(t, e, "fake", a)
	r := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "TEST_ONLY_SENTINEL_VALUE") || strings.Contains(string(b), "RAW_ERROR_TEST_SENTINEL") {
		t.Fatal("sensitive value reached persisted run log")
	}
	if len(r.Steps) != 1 || r.Steps[0].Summary == "" || r.Steps[0].ErrorCode != "upstream_failed" {
		t.Fatal("safe failure reason missing from execution log")
	}
}

func TestRunLogProvidesReadableReasonWhenAdapterHasNoSummary(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	a := singleAdapter(syntheticTask("fake", "account-a", "task-a"))
	a.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) {
		return taskengine.Observation{Phase: "manual"}, nil
	}
	registerBehavior(t, e, "fake", a)
	r := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
	if len(r.Steps) != 1 || r.Steps[0].Summary == "" {
		t.Fatal("empty provider summary left an unreadable execution log")
	}
}
