package taskproviders

// 任务：成长资源状态（只读）
//
// 读取能量、抽奖机会、Buddy 额度与连登；不抽奖、不开箱、不消耗权益。

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	if m.up == nil {
		return taskengine.Observation{}, errors.New("resources_unavailable")
	}
	parts := []string{}
	failed := false
	read := 0
	if energy, err := m.up.TaskEnergy(ctx, a); err == nil {
		read++
		parts = append(parts, fmt.Sprintf("能量 %d", energy))
		taskProgress(ctx, fmt.Sprintf("能量余额已读取：%d。", energy))
	} else {
		parts = append(parts, "能量未读取")
		taskProgress(ctx, "能量接口未读取成功；继续读取其他资源。")
		failed = true
	}
	if chances, err := m.up.TaskLotteryChances(ctx, a); err == nil {
		read++
		parts = append(parts, fmt.Sprintf("抽奖机会 %d", chances))
		taskProgress(ctx, fmt.Sprintf("抽奖机会已读取：%d。", chances))
	} else {
		parts = append(parts, "抽奖机会未读取")
		taskProgress(ctx, "抽奖机会未读取成功；继续读取其他资源。 ")
		failed = true
	}
	if quota, err := m.up.TaskBuddyQuota(ctx, a); err == nil {
		read++
		parts = append(parts, fmt.Sprintf("可开 Buddy %d（单次上限 %d）", quota.Affordable, quota.MaxOpenCount))
		taskProgress(ctx, fmt.Sprintf("Buddy 额度已读取：可开 %d，单次上限 %d。", quota.Affordable, quota.MaxOpenCount))
	} else {
		parts = append(parts, "Buddy 额度未读取")
		taskProgress(ctx, "Buddy 额度未读取成功；保留其他已读取数据。")
		failed = true
	}
	if streak, err := m.up.TaskStreak(ctx, a); err == nil {
		read++
		parts = append(parts, fmt.Sprintf("连登 %d 天", streak.Streak.Days))
		taskProgress(ctx, fmt.Sprintf("连登状态已读取：%d 天。", streak.Streak.Days))
	} else {
		parts = append(parts, "连登状态未读取")
		taskProgress(ctx, "连登状态未读取成功；保留其他已读取数据。")
		failed = true
	}
	phase := "done"
	if failed {
		phase = "blocked"
		parts = append(parts, "本次仅展示已成功读取字段；失败项保持未知。")
	}
	result := taskengine.Observation{Phase: phase, Summary: strings.Join(parts, "；")}
	if read == 0 {
		return result, errors.New("resources_unavailable")
	}
	return result, nil
}

func (m *resourcesModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	d := m.definition
	s, e := m.Inspect(ctx, a, taskengine.Task{Definition: d})
	return []ModuleTask{{d, s}}, e
}

func (m *resourcesModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
