package taskproviders

// 任务：盲盒抽奖（每日一次）
//
// 只抽一次，且必须有奖品回执 + 官方机会余额严格减少两项证据。无机会是本次
// 未提交的跳过，不写成「今日已完成」而屏蔽后来获得的机会。丢失响应或重启后
// 只核验，不重抽。

import (
	"context"
	"errors"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

func newLotteryModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" || a.options.SpendVault == nil {
		return nil, nil
	}
	return &lotteryModule{spendRewardModule: spendRewardModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "lottery", Key: "lottery", Realms: []string{"cn"}},
			definition: taskengine.Definition{
				Key:         "lottery",
				Name:        "盲盒抽奖",
				Description: "读取官方抽奖机会，机会大于零时只抽一次，并用官方回读确认；无机会或活动未开启不提交。",
				OncePerDay:  true,
				SideEffect:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify},
			},
		},
		up:    a.Upstream,
		vault: a.options.SpendVault,
	}}, nil
}

var lotteryPlan = &DefaultPlan{ID: "legacy-lottery", TaskKey: "lottery", Hours: []int{12}, Minute: 15, LegacyEnabled: boolValue(true)}

type lotteryModule struct{ spendRewardModule }

func (m *lotteryModule) inspectRead(ctx context.Context, a *auth.Auth) (taskengine.Observation, error) {
	if a.Realm() != "cn" || m.up == nil {
		return taskengine.Observation{}, errors.New("lottery_unavailable")
	}
	chances, err := m.up.TaskLotteryChances(ctx, a)
	if err != nil {
		return taskengine.Observation{}, err
	}
	if chances == 0 {
		// No submission happened. Returning done here would persist a false
		// completion and hide a chance granted later the same day.
		return observation("skipped", "当前没有抽奖机会；本次只读跳过，未提交。"), nil
	}
	return taskengine.Observation{Phase: "ready", Total: int64(chances), Summary: "官方可用抽奖机会；执行只抽一次。"}, nil
}

func (m *lotteryModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	summary, ok, err := m.settledToday(a, "lottery")
	if err != nil {
		return taskengine.Observation{}, err
	}
	if ok {
		return observation("done", summary), nil
	}
	j, _, err := m.loadSpend(a, "lottery")
	if err == nil && j.Period == m.currentPeriod(a) && (j.State == "attempting" || j.State == "unknown" || j.State == "unresolved") {
		return observation("pending", "本次抽奖已有未确认尝试；只核验，不重新抽取。"), nil
	}
	if err != nil && !errors.Is(err, accountvault.ErrNotFound) {
		return taskengine.Observation{}, err
	}
	return m.inspectRead(ctx, a)
}

func (m *lotteryModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	state, err := m.inspectRead(ctx, a)
	if err != nil || state.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, state), err
	}
	reference := taskengine.ActionReference(t, taskengine.StageExecute)
	journal, rev, err := m.beginSpend(a, "lottery", m.currentPeriod(a), "execute", reference, int(state.Total))
	if err != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "已有抽奖尝试未核验，未再次提交。")), err
	}
	result, _, err := m.up.TaskLotteryDraw(ctx, a, journal.ClientToken)
	if err != nil {
		if upstream.IsLotteryNoChance(err) || upstream.IsLotteryDisabled(err) {
			// Explicit provider refusal proves nothing was consumed. Release
			// the provisional marker so a later chance can still be used.
			if finishErr := m.finishSpend(a, journal, rev, "not_submitted", "官方拒绝：机会不足或活动未开启；未产生抽奖。", journal.Before); finishErr != nil {
				return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "拒绝结果未能安全保存。")), finishErr
			}
			return taskengine.NotSubmitted(t, taskengine.StageExecute, observation("skipped", "官方机会已用尽或活动未开启；未产生抽奖。")), nil
		}
		_ = m.finishSpend(a, journal, rev, "unknown", "抽奖结果未知，保留待核验，不重试。", journal.Before)
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "抽奖结果未知，保留待核验，不重试。")), err
	}
	if result == nil || result.PrizeCode == "" && result.PrizeName == "" && result.CreditAmount <= 0 {
		_ = m.finishSpend(a, journal, rev, "unknown", "抽奖响应缺少奖品回执，保留待核验，不重试。", journal.Before)
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "抽奖响应缺少可绑定奖品，保留待核验，不重试。")), errors.New("lottery_receipt_missing")
	}
	chances, readErr := m.up.TaskLotteryChances(ctx, a)
	if readErr != nil {
		_ = m.finishSpend(a, journal, rev, "unknown", "抽奖已返回奖品但机会余额未能回读，保留待核验。", journal.Before)
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "抽奖已返回奖品但机会余额未能回读，保留待核验。")), readErr
	}
	if chances >= journal.Before {
		_ = m.finishSpend(a, journal, rev, "unknown", "抽奖有奖品回执但机会余额未减少，保留待核验。", chances)
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "抽奖有奖品回执但机会余额未减少，保留待核验。")), errors.New("lottery_state_unconfirmed")
	}
	summary := "官方返回抽奖结果：" + firstNonEmpty(result.PrizeName, result.PrizeCode)
	if result.PrizeType == "physical" {
		summary += "（实物奖，需到官方成长中心填写收件信息）"
	}
	if err = m.finishSpend(a, journal, rev, "accepted", summary, chances); err != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "抽奖已成功但结果未能安全保存，保留待核验。")), err
	}
	return taskengine.Accepted(t, taskengine.StageExecute, taskengine.Observation{Phase: "done", Summary: summary}), nil
}

func (m *lotteryModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	return discoverSpend(m, ctx, a)
}

func (m *lotteryModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
