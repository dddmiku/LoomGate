package taskproviders

// 任务：Trae 余额与到期桶（只读）
//
// 独立于签到：9074 退避期间仍能刷新积分与到期分桶，驱动「优先消耗快过期
// 积分」选号。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream/trae"
)

func newTraeBalanceModule(a *Accounts) (Module, error) {
	if a.Provider != "trae" {
		return nil, nil
	}
	client := a.options.TraeClient
	if client == nil {
		client = &trae.TaskClient{}
	}
	now := a.options.Now
	if now == nil {
		now = time.Now
	}
	return &traeBalanceModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "balance", Key: "balance", Realms: []string{"trae"}},
			definition: taskengine.Definition{
				Key:         "balance",
				Name:        "余额刷新",
				Description: "只读查询积分与到期分桶，更新网关账号池；不签到、不生成内容。",
				SideEffect:  false,
				Idempotent:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect},
			},
		},
		owner: a, api: client, now: now,
	}, nil
}

// Trae 余额是只读任务，可以随包默认启用；旧版没有独立的 Trae 余额计划，
// 所以不记录 legacy switch。
var traeBalancePlan = &DefaultPlan{ID: "legacy-trae-balance", TaskKey: "balance", Hours: []int{14}, StartEnabled: true}

type traeBalanceModule struct {
	moduleBase
	owner *Accounts
	api   TraeTaskAPI
	now   func() time.Time
}

func (m *traeBalanceModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	credits, err := m.api.Credits(ctx, a)
	if err != nil {
		return taskengine.Observation{}, err
	}
	if math.IsNaN(credits.Remaining) || math.IsInf(credits.Remaining, 0) || credits.Remaining < 0 || credits.Remaining >= float64(math.MaxInt64) {
		return taskengine.Observation{}, errors.New("trae_credit_range_invalid")
	}
	if ctx.Err() != nil {
		return taskengine.Observation{}, ctx.Err()
	}
	if _, err = m.owner.account(a.UID); err != nil {
		return taskengine.Observation{}, err
	}
	remain := int64(credits.Remaining)
	expiring, err := trae.TaskExpiry(credits, m.owner.ExpiringSoon, m.now())
	if err != nil {
		return taskengine.Observation{}, err
	}
	m.owner.Pool.SetCreditsDetailed(a.UID, remain, expiring)
	m.owner.Pool.ReenableIfCredits(a.UID, remain)
	return taskengine.Observation{Phase: "done", Current: remain, Summary: fmt.Sprintf("剩余 %d 积分；配置窗口内将到期 %d 积分。已更新账号池。", remain, expiring)}, nil
}

func (m *traeBalanceModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
