package taskproviders

// 任务：Buddy 能量盲盒（每日一次）
//
// 按官方额度最多开一次，开完回读额度。开箱响应必须带回执且额度严格减少才能
// 记为已接受；实物奖仍需用户到官方成长中心填写收件信息。

import (
	"context"
	"errors"
	"fmt"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
)

func newBuddyBoxModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" || a.options.SpendVault == nil {
		return nil, nil
	}
	return &buddyBoxModule{spendRewardModule: spendRewardModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "buddy_box", Key: "buddy_box", Realms: []string{"cn"}},
			definition: taskengine.Definition{
				Key:         "buddy_box",
				Name:        "Buddy 能量盲盒",
				Description: "读取官方能量与单次上限，按官方额度最多开一次，然后回读额度；实物奖需用户到官方成长中心填写收件信息。",
				OncePerDay:  true,
				SideEffect:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify},
			},
		},
		up:    a.Upstream,
		vault: a.options.SpendVault,
	}}, nil
}

var buddyBoxPlan = &DefaultPlan{ID: "legacy-buddy-box", TaskKey: "buddy_box", Hours: []int{12}, Minute: 20, LegacyEnabled: boolValue(true)}

type buddyBoxModule struct{ spendRewardModule }

func (m *buddyBoxModule) inspectRead(ctx context.Context, a *auth.Auth) (taskengine.Observation, error) {
	if a.Realm() != "cn" || m.up == nil {
		return taskengine.Observation{}, errors.New("buddy_box_unavailable")
	}
	quota, err := m.up.TaskBuddyQuota(ctx, a)
	if err != nil {
		return taskengine.Observation{}, err
	}
	if quota.Affordable < 0 || quota.MaxOpenCount < 0 {
		return taskengine.Observation{}, errors.New("buddy_box_state_invalid")
	}
	if quota.Affordable == 0 {
		return observation("skipped", "当前能量不足一次开箱；本次只读跳过，未提交。"), nil
	}
	limit := quota.MaxOpenCount
	if limit <= 0 {
		limit = 1
	}
	// Total is the pre-spend affordable baseline, not the display limit: the
	// post-spend read-back must prove this exact balance decreased.
	return taskengine.Observation{Phase: "ready", Total: int64(quota.Affordable), Summary: fmt.Sprintf("能量可开 %d 个，单次上限 %d；执行只开一次。", quota.Affordable, limit)}, nil
}

func (m *buddyBoxModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	summary, ok, err := m.settledToday(a, "buddy_box")
	if err != nil {
		return taskengine.Observation{}, err
	}
	if ok {
		return observation("done", summary), nil
	}
	j, _, err := m.loadSpend(a, "buddy_box")
	if err == nil && j.Period == m.currentPeriod(a) && (j.State == "attempting" || j.State == "unknown" || j.State == "unresolved") {
		return observation("pending", "本次开箱已有未确认尝试；只核验，不重新开箱。"), nil
	}
	if err != nil && !errors.Is(err, accountvault.ErrNotFound) {
		return taskengine.Observation{}, err
	}
	return m.inspectRead(ctx, a)
}

func (m *buddyBoxModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	state, err := m.inspectRead(ctx, a)
	if err != nil || state.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, state), err
	}
	reference := taskengine.ActionReference(t, taskengine.StageExecute)
	journal, rev, err := m.beginSpend(a, "buddy_box", m.currentPeriod(a), "execute", reference, int(state.Total))
	if err != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "已有开箱尝试未核验，未再次提交。")), err
	}
	name, _, err := m.up.TaskBuddyOpen(ctx, a, 1, journal.ClientToken)
	if err != nil {
		_ = m.finishSpend(a, journal, rev, "unknown", "开箱结果未知，保留待核验，不重试。", journal.Before)
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "开箱结果未知，保留待核验，不重试。")), err
	}
	quota, readErr := m.up.TaskBuddyQuota(ctx, a)
	if readErr != nil {
		_ = m.finishSpend(a, journal, rev, "unknown", "开箱已提交但额度未能回读，保留待核验。", journal.Before)
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "开箱已提交但额度未能回读，保留待核验。")), readErr
	}
	if quota.Affordable >= journal.Before {
		_ = m.finishSpend(a, journal, rev, "unknown", "开箱请求返回但官方额度未减少，保留待核验。", quota.Affordable)
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "开箱请求返回但官方额度未减少，保留待核验。")), errors.New("buddy_box_state_unconfirmed")
	}
	summary := "已按官方额度开一个 Buddy 盲盒"
	if name != "" {
		summary += "：" + name
	}
	if err = m.finishSpend(a, journal, rev, "accepted", summary, quota.Affordable); err != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "开箱已成功但结果未能安全保存，保留待核验。")), err
	}
	return taskengine.Accepted(t, taskengine.StageExecute, taskengine.Observation{Phase: "done", Summary: summary}), nil
}

func (m *buddyBoxModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	return discoverSpend(m, ctx, a)
}

func (m *buddyBoxModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
