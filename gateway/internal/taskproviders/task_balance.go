package taskproviders

// 任务：余额刷新（WorkBuddy 国内/国际）
//
// 纯读任务：只查积分与到期分桶并更新账号池，不签到、不生成内容。它是唯一
// 允许随包默认启用的计划，因为它没有上游写副作用。

import (
	"context"
	"errors"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
)

func newBalanceModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" {
		return nil, nil
	}
	return &balanceModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "balance", Key: "balance", Realms: []string{"cn", "global"}},
			definition: taskengine.Definition{
				Key:         "balance",
				Name:        "余额刷新",
				Description: "只读查询积分与到期分桶，更新网关账号池；不签到、不生成内容。",
				SideEffect:  false,
				Idempotent:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect},
			},
		},
		owner: a,
	}, nil
}

var balancePlan = &DefaultPlan{ID: "legacy-balance", TaskKey: "balance", Hours: []int{14}, StartEnabled: true, LegacyEnabled: boolValue(true)}

type balanceModule struct {
	moduleBase
	owner *Accounts
}

func (m *balanceModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	if m.owner.Upstream == nil {
		return taskengine.Observation{}, errors.New("balance_unavailable")
	}
	remain, buckets, err := m.owner.Upstream.UserResourceDetailedContext(ctx, a, m.owner.ExpiringSoon)
	if err != nil {
		return taskengine.Observation{}, err
	}
	if ctx.Err() != nil {
		return taskengine.Observation{}, ctx.Err()
	}
	if _, err = m.owner.account(a.UID); err != nil {
		return taskengine.Observation{}, err
	}
	m.owner.Pool.SetCreditsDetailed(a.UID, remain, buckets.Expiring)
	m.owner.Pool.ReenableIfCredits(a.UID, remain)
	return taskengine.Observation{Phase: "done", Current: remain}, nil
}

func (m *balanceModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
