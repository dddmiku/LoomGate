package main

import (
	"context"
	"fmt"
	"workbuddy2api/internal/taskengine"
)

// This adapter never makes network requests. The real engine supplies storage,
// locking, dispatch, verification, cancellation and restart recovery to the UI test.
type syntheticTasks struct {
	provider string
	fixture  *fixtures
}

func (s *syntheticTasks) Discover(ctx context.Context, scope taskengine.Scope) ([]taskengine.Task, error) {
	if s.fixture.mode.Load() == 3 {
		return nil, fmt.Errorf("synthetic_discovery_unavailable")
	}
	account := "a-fixture"
	if len(scope.AccountIDs) > 0 {
		found := false
		for _, id := range scope.AccountIDs {
			if id == account {
				found = true
			}
		}
		if !found {
			return []taskengine.Task{}, nil
		}
	}
	out := []taskengine.Task{}
	for _, row := range []struct {
		key, name, description string
		manual                 bool
	}{
		{"fixture-check", "合成核验任务", "在隔离环境完成一次执行，并由服务端复核。", false},
		{"fixture-manual", "提交所需材料", "请在官方工作界面选择需要处理的文档，再确认提交。", true},
		{"fixture-pending", "等待上游状态", "保持等待状态，用于验证取消和页面同步。", false},
	} {
		out = append(out, taskengine.Task{Definition: taskengine.Definition{Key: row.key, ProviderID: s.provider, Name: row.name, Description: row.description, ManualOnly: row.manual, SideEffect: !row.manual, Stages: []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify}}, Target: taskengine.Target{AccountID: account}})
	}
	return out, nil
}
func (s *syntheticTasks) Inspect(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	phase := "ready"
	if t.Key == "fixture-pending" {
		phase = "pending"
	}
	if t.ManualOnly {
		phase = "manual"
	}
	return taskengine.Observation{Phase: phase}, nil
}
func (s *syntheticTasks) Execute(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	s.fixture.mutations.Add(1)
	return taskengine.Observation{Phase: "pending"}, nil
}
func (s *syntheticTasks) Claim(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return taskengine.Observation{}, fmt.Errorf("unexpected_claim")
}
func (s *syntheticTasks) Verify(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	if t.Key == "fixture-pending" {
		return taskengine.Observation{Phase: "pending"}, nil
	}
	return taskengine.Observation{Phase: "done"}, nil
}
