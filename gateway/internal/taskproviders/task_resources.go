package taskproviders

// 任务：成长资源状态（只读）
//
// 读取能量、抽奖机会、Buddy 额度与连登；不抽奖、不开箱、不消耗权益。

import (
	"context"
	"fmt"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

func newResourcesModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" {
		return nil, nil
	}
	return &resourcesModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "resources", Key: "resources", Realms: []string{"cn"}},
			definition: taskengine.Definition{
				Key:         "resources",
				Name:        "成长资源状态",
				Description: "读取能量、抽奖机会、Buddy 额度与连登；不抽奖、不开箱、不消耗权益。",
				SideEffect:  false,
				Idempotent:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect},
			},
		},
		up: a.Upstream,
	}, nil
}

var resourcesPlan = &DefaultPlan{ID: "legacy-resources", TaskKey: "resources", Hours: []int{12}, Minute: 25, LegacyEnabled: boolValue(true)}

type resourcesModule struct {
	moduleBase
	up *upstream.Client
}

func (m *resourcesModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	r, e := m.up.TaskResources(ctx, a)
	if e != nil {
		return taskengine.Observation{}, e
	}
	st, e := m.up.TaskStreak(ctx, a)
	if e != nil {
		return taskengine.Observation{}, e
	}
	return observation("done", fmt.Sprintf("能量 %d；抽奖机会 %d；可开 Buddy %d；单次上限 %d；连登 %d 天。只读查询，未消耗。", r.Energy, r.Chances, r.Quota.Affordable, r.Quota.MaxOpenCount, st.Streak.Days)), nil
}

func (m *resourcesModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	d := m.definition
	s, e := m.Inspect(ctx, a, taskengine.Task{Definition: d})
	return []ModuleTask{{d, s}}, e
}

func (m *resourcesModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
