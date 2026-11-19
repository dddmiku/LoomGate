package taskproviders

// 任务：补偿积分领取（一次性）
//
// 活动期间与积分全部来自官方响应，不写死日期。周期键由活动截止时间与积分
// 共同生成，活动变化后不能用新活动确认旧领取。

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

func newCompensationModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" {
		return nil, nil
	}
	return &compensationModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "compensation", Key: "compensation", Realms: []string{"cn"}},
			definition: taskengine.Definition{
				Key:         "compensation",
				Name:        "补偿领取",
				Description: "检查当前补偿活动和领取状态，领取后再次确认结果。",
				SideEffect:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect, taskengine.StageClaim, taskengine.StageVerify},
			},
		},
		up: a.Upstream,
	}, nil
}

// 补偿是一次性手动任务：旧版没有开关记录，也从不自动启用。引擎的计划必须
// 有一个时刻，所以沿用内置清单里的 12:30，保持 enabled=false 不派发。
var compensationPlan = &DefaultPlan{ID: "legacy-compensation", TaskKey: "compensation", Hours: []int{12}, Minute: 30}

type compensationModule struct {
	moduleBase
	up *upstream.Client
}

func compensationPeriod(v upstream.TaskCompensation) string {
	return fmt.Sprintf("offer-%x", sha256.Sum256([]byte(v.End+"\x00"+strconv.FormatInt(v.Credit, 10))))
}

func (m *compensationModule) state(t taskengine.Task, v upstream.TaskCompensation) taskengine.Observation {
	if t.PeriodKey != compensationPeriod(v) {
		return observation("blocked", "补偿活动已变化，不能用新活动确认旧领取。")
	}
	if v.Claimed {
		return observation("done", "官方确认此项补偿已领取。")
	}
	if !v.Active {
		return observation("manual", "当前无进行中的补偿活动。")
	}
	return observation("claimable", "官方确认活动进行中且尚未领取；提交后再次查询领取状态。")
}

func (m *compensationModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	v, e := m.up.TaskCompensation(ctx, a)
	if e != nil {
		return nil, e
	}
	d := m.definition
	d.PeriodKey = compensationPeriod(v)
	d.Reward = fmt.Sprintf("%d 积分", v.Credit)
	return []ModuleTask{{d, m.state(taskengine.Task{Definition: d}, v)}}, nil
}

func (m *compensationModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	v, e := m.up.TaskCompensation(ctx, a)
	if e != nil {
		return taskengine.Observation{}, e
	}
	return m.state(t, v), nil
}

func (m *compensationModule) Claim(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	state, e := m.Inspect(ctx, a, t)
	if e != nil || state.Phase != "claimable" {
		return taskengine.NotSubmitted(t, taskengine.StageClaim, state), e
	}
	e = m.up.TaskCompensationClaim(ctx, a)
	return taskengine.UnknownAction(t, taskengine.StageClaim, observation("pending", "补偿领取已提交，等待 claimed 回读。")), e
}

func (m *compensationModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
