package taskengine_test

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/taskengine"
)

func TestOutputCanonicalizationSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	a := singleAdapter(syntheticTask("fake", "account-a", "resources"))
	a.inspect = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
		return phase("done"), taskengine.Output(ctx, strings.Repeat("查", 200))
	}
	registerBehavior(t, e, "fake", a)
	r := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", TaskKey: "resources", AccountIDs: []string{"account-a"}}).ID)
	if r.Status != taskengine.StatusSucceeded {
		t.Fatal(r.Status)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := taskengine.Open(taskengine.Options{Path: path})
	if err != nil {
		t.Fatalf("valid multi-byte output prevented reopening: %v", err)
	}
	defer restored.Close()
	runs := restored.Runs()
	if len(runs) != 1 || !reflect.DeepEqual(r.Output, runs[0].Output) {
		t.Fatal("persisted output changed on reopen")
	}
}
