package taskengine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

func TestOutputIsReadableBeforeStageReturnsAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	a := singleAdapter(syntheticTask("fake", "account-a", "growth/live"))
	a.inspect = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
		if err := taskengine.Output(ctx, "正在读取成长目录。\n目录已读取，正在核对领取条件。"); err != nil {
			return taskengine.Observation{}, err
		}
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return phase("done"), nil
	}
	registerBehavior(t, e, "fake", a)
	r := submitBehavior(t, e, taskengine.Request{ProviderID: "fake", TaskKey: "growth/"})
	awaitBehaviorSignal(t, started)
	log, err := e.TaskLog("growth/")
	if err != nil || !log.Running || !strings.Contains(strings.Join(log.Lines, "\n"), "目录已读取，正在核对领取条件。") {
		t.Fatal("stage is blocked but its real progress output is not readable")
	}
	if len(e.Runs()[0].Steps) != 0 {
		t.Fatal("fixture stage returned before live output was checked")
	}
	if !e.Cancel(r.ID) {
		t.Fatal("fixture run was not cancellable")
	}
	awaitBehaviorRun(t, e, r.ID)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openBehaviorEngine(t, taskengine.Options{Path: path})
	log, err = reopened.TaskLog("growth/")
	if err != nil || !strings.Contains(strings.Join(log.Lines, "\n"), "正在读取成长目录。") {
		t.Fatal("persisted live output was lost on restart")
	}
}

func TestOutputRejectsLateStageWritersAndRedactsBeforePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	var captured context.Context
	a := singleAdapter(syntheticTask("fake", "account-a", "checkin"))
	a.inspect = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
		captured = ctx
		err := taskengine.Output(ctx, "正在核验账号。 token=TEST_ONLY_OUTPUT_SECRET\r\n查询已完成。\x1b[2J")
		return phase("done"), err
	}
	registerBehavior(t, e, "fake", a)
	r := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
	if err := taskengine.Output(captured, "LATE_OUTPUT_SENTINEL"); err == nil {
		t.Fatal("returned stage still accepts progress from a detached writer")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "正在核验账号。") || strings.Contains(string(data), "TEST_ONLY_OUTPUT_SECRET") || strings.Contains(string(data), "LATE_OUTPUT_SENTINEL") || strings.Contains(string(data), `\u001b`) {
		t.Fatal("live output was missing or persisted sensitive/control text")
	}
	if r.Status != taskengine.StatusSucceeded {
		t.Fatal("informational output changed fixture task completion")
	}
}

func TestOutputIsBoundedWithoutChangingTaskEvidence(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	a := singleAdapter(syntheticTask("fake", "account-a", "growth/bounded"))
	a.inspect = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
		if err := taskengine.Output(ctx, strings.Repeat("oversized", 4096)); err == nil {
			t.Error("oversized progress should be rejected")
		}
		return phase("done"), nil
	}
	registerBehavior(t, e, "fake", a)
	r := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
	if r.Status != taskengine.StatusSucceeded {
		t.Fatal("oversized progress was accepted instead of rejected before persistence")
	}
}

func TestOutputClosedStageCannotAppendWhileNextStageRuns(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	var captured context.Context
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	a := singleAdapter(syntheticTask("fake", "account-a", "growth/late"))
	a.inspect = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
		captured = ctx
		return phase("ready"), nil
	}
	a.execute = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return phase("pending"), nil
	}
	registerBehavior(t, e, "fake", a)
	r := submitBehavior(t, e, taskengine.Request{ProviderID: "fake"})
	awaitBehaviorSignal(t, entered)
	if captured.Err() != nil {
		t.Fatal("fixture must keep the shared task context alive")
	}
	if err := taskengine.Output(captured, "DETACHED_STAGE_SENTINEL"); err == nil {
		t.Fatal("old stage appended output while another stage owns the run")
	}
	e.Cancel(r.ID)
	awaitBehaviorRun(t, e, r.ID)
}

func TestOutputRetentionIsBoundedAndTamperedAccountIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path, Timeout: 15 * time.Second})
	a := singleAdapter(syntheticTask("fake", "account-a", "resources"))
	a.inspect = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
		for i := 0; i < 20; i++ {
			if err := taskengine.Output(ctx, strings.Repeat("合成资源查询进度。\n", 32)); err != nil {
				return taskengine.Observation{}, err
			}
		}
		return phase("done"), nil
	}
	registerBehavior(t, e, "fake", a)
	r := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", AccountIDs: []string{"account-a"}}).ID)
	if r.Status != taskengine.StatusSucceeded || len(r.Output) == 0 || len(r.Output) > 512 || r.OutputDropped == 0 {
		t.Fatal("diagnostic retention is not bounded or lost its truncation signal")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	var runs []taskengine.Run
	if err := json.Unmarshal(state["runs"], &runs); err != nil {
		t.Fatal(err)
	}
	runs[0].Output[0].AccountID = "another-account"
	state["runs"], err = json.Marshal(runs)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := taskengine.Open(taskengine.Options{Path: path}); err == nil {
		reopened.Close()
		t.Fatal("persisted output accepted an account outside its run scope")
	}
}

func TestOutputCannotConfirmAnUnknownMutation(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{Timeout: 60 * time.Millisecond})
	a := singleAdapter(syntheticTask("fake", "account-a", "lottery"))
	a.execute = func(ctx context.Context, _ taskengine.Task) (taskengine.Observation, error) {
		return phase("pending"), taskengine.Output(ctx, "已提交抽奖，领取结果仍待核验。")
	}
	a.verify = func(context.Context, taskengine.Task) (taskengine.Observation, error) { return phase("pending"), nil }
	registerBehavior(t, e, "fake", a)
	r := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake"}).ID)
	log, _ := e.TaskLog("lottery")
	if r.Status != taskengine.StatusUncertain || !strings.Contains(strings.Join(log.Lines, "\n"), "已提交抽奖，领取结果仍待核验。") {
		t.Fatal("progress output was lost or confused with action completion evidence")
	}
}

func TestTaskLogProviderQueryCannotReturnAnotherProvidersTask(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	for _, provider := range []string{"fake-a", "fake-b"} {
		a := singleAdapter(syntheticTask(provider, "account-a", "checkin"))
		a.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) {
			return taskengine.Observation{Phase: "done", Summary: provider + " fixture output"}, nil
		}
		registerBehavior(t, e, provider, a)
		awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: provider, TaskKey: "checkin"}).ID)
	}
	w := workbenchHTTP(t, e, http.MethodGet, "/workbench/task/log?provider=fake-a&key=checkin", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fake-a fixture output") || strings.Contains(w.Body.String(), "fake-b fixture output") {
		t.Fatal("same task key leaked the most recent run of another provider")
	}
}

func TestTaskLogIncludesDistinctDiscoveredTaskNames(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	first, second := syntheticTask("fake", "account-a", "growth/one"), syntheticTask("fake", "account-a", "growth/two")
	first.Name, second.Name = "合成成长任务甲", "合成成长任务乙"
	a := singleAdapter(first)
	a.discover = func(context.Context, taskengine.Scope) ([]taskengine.Task, error) {
		return []taskengine.Task{first, second}, nil
	}
	a.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) {
		return taskengine.Observation{Phase: "done", Summary: "官方确认奖励已领取。"}, nil
	}
	registerBehavior(t, e, "fake", a)
	awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", TaskKey: "growth/"}).ID)
	log, _ := e.TaskLog("growth/")
	text := strings.Join(log.Lines, "\n")
	if !strings.Contains(text, first.Name) || !strings.Contains(text, second.Name) {
		t.Fatal("different growth tasks remain indistinguishable in output")
	}
}
