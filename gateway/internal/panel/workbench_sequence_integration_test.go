package panel

import (
	"context"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

type sequencePanelAdapter struct {
	entered  chan struct{}
	release  chan struct{}
	accepted atomic.Bool
	writes   atomic.Int32
	reads    atomic.Int32
}

func (a *sequencePanelAdapter) Discover(context.Context, taskengine.Scope) ([]taskengine.Task, error) {
	return []taskengine.Task{
		{Definition: taskengine.Definition{ProviderID: "fixture", Key: "submit", Name: "合成提交", SideEffect: true}, Target: taskengine.Target{AccountID: "account-fixture"}},
		{Definition: taskengine.Definition{ProviderID: "fixture", Key: "balance", Name: "合成余额", Idempotent: true}, Target: taskengine.Target{AccountID: "account-fixture"}},
	}, nil
}
func (a *sequencePanelAdapter) Inspect(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	if t.Key == "balance" {
		a.reads.Add(1)
		_ = taskengine.Output(ctx, "合成余额读取完成。")
		return taskengine.Observation{Phase: "done"}, nil
	}
	if a.accepted.Load() {
		return taskengine.Observation{Phase: "done"}, nil
	}
	return taskengine.Observation{Phase: "ready"}, nil
}
func (a *sequencePanelAdapter) Execute(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	a.writes.Add(1)
	_ = taskengine.Output(ctx, "合成操作正在执行，输出先于阶段返回。")
	close(a.entered)
	select {
	case <-a.release:
	case <-ctx.Done():
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending"}), ctx.Err()
	}
	a.accepted.Store(true)
	return taskengine.Accepted(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending"}), nil
}
func (a *sequencePanelAdapter) Claim(context.Context, taskengine.Task) (taskengine.Observation, error) {
	panic("unexpected claim")
}
func (a *sequencePanelAdapter) Verify(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return a.Inspect(ctx, t)
}

func sequencePanelLogText(value any) string {
	items, _ := value.([]any)
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, jsonString(item))
	}
	return strings.Join(lines, "\n")
}

func TestWorkbenchSequenceUsesRealPanelAndEngineHandlers(t *testing.T) {
	h := newHarness(t)
	h.server.opts.Console = true
	doc := h.writeCredentials()
	path := filepath.Join(t.TempDir(), "engine.json")
	engine, err := taskengine.Open(taskengine.Options{Path: path, Timeout: 2 * time.Second, PollMin: time.Millisecond, PollMax: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close() }()
	adapter := &sequencePanelAdapter{entered: make(chan struct{}), release: make(chan struct{})}
	if err = engine.Register("fixture", adapter); err != nil {
		t.Fatal(err)
	}
	h.server.gateway.Management.Handler = engine.Handler()
	payload := map[string]any{"id": "fixture-sequence-plan", "provider_id": "fixture", "task_key": "fixture-sequence", "task_sequence": []string{"submit", "balance"},
		"account_ids": []string{"account-fixture"}, "enabled": false, "hours": []int{9}, "minute": 0, "timezone": "UTC"}
	response := h.postJSON(doc, "/api/workbench/schedule", payload)
	if response.Code != http.StatusOK {
		t.Fatalf("sequence save status=%d", response.Code)
	}
	if plans := engine.Schedules(); len(plans) != 1 || !reflect.DeepEqual(plans[0].TaskSequence, []string{"submit", "balance"}) {
		t.Fatal("panel stripped the sequence while saving")
	}
	response = h.postJSON(doc, "/api/workbench/run", map[string]any{"provider_id": "fixture", "task_key": "fixture-sequence", "task_sequence": []string{"submit", "balance"}, "account_ids": []string{"account-fixture"}})
	if response.Code != http.StatusAccepted {
		t.Fatalf("sequence run status=%d", response.Code)
	}
	id := jsonString(jsonObject(decode(t, response)["run"])["id"])
	if id == "" {
		t.Fatal("panel lost the accepted run identity")
	}
	select {
	case <-adapter.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("sequence never entered its adapter")
	}
	header := map[string]string{"Cookie": CookieName + "=" + h.token(doc)}
	log := decode(t, h.do(http.MethodGet, "/api/task/log?provider=fixture&key=fixture-sequence", "", header))
	if log["running"] != true || !strings.Contains(sequencePanelLogText(log["lines"]), "输出先于阶段返回") {
		t.Fatal("real handlers did not expose the live sequence output")
	}
	close(adapter.release)
	deadline := time.Now().Add(5 * time.Second)
	finished := false
	for time.Now().Before(deadline) {
		for _, run := range engine.Runs() {
			if run.ID == id && run.FinishedAt != nil {
				if run.Status != taskengine.StatusSucceeded {
					t.Fatalf("sequence ended %s", run.Status)
				}
				finished = true
			}
		}
		if finished {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !finished || adapter.writes.Load() != 1 || adapter.reads.Load() != 1 {
		t.Fatal("handler sequence did not finish exactly one write and one read")
	}
	if err = engine.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err = taskengine.Open(taskengine.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	h.server.gateway.Management.Handler = engine.Handler()
	log = decode(t, h.do(http.MethodGet, "/api/task/log?provider=fixture&key=fixture-sequence", "", header))
	if log["running"] != false || !strings.Contains(sequencePanelLogText(log["lines"]), "合成余额读取完成") {
		t.Fatal("real handlers lost sequence output after restart")
	}
	if len(h.transport.recorded()) != 0 {
		t.Fatal("in-process handler test unexpectedly fell back to network transport")
	}
}
