package taskproviders

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

// spendRewardModule is a once-per-day, single-shot consumer of an official
// resource. It always re-reads the quota under the engine lock and never
// retries an unknown submission.
type spendRewardModule struct {
	moduleBase
	up *upstream.Client
	mu sync.Mutex
	// consumed marks this process's successful one-shot spend for the day so
	// verify can confirm without claiming the resource is globally exhausted.
	consumed map[string]string
}

func (m *spendRewardModule) markConsumed(a *auth.Auth, key, summary string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.consumed == nil {
		m.consumed = map[string]string{}
	}
	m.consumed[a.Snapshot().UID+"\x00"+key+"\x00"+time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")] = summary
}

func (m *spendRewardModule) consumedToday(a *auth.Auth, key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	summary, ok := m.consumed[a.Snapshot().UID+"\x00"+key+"\x00"+time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")]
	return summary, ok
}

func newRewardModules(up *upstream.Client, defs map[string]taskengine.Definition) []Module {
	return []Module{
		&lotteryModule{spendRewardModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "lottery", Key: "lottery", Realms: []string{"cn"}}, definition: defs["lottery"]}, up: up}},
		&buddyBoxModule{spendRewardModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "buddy_box", Key: "buddy_box", Realms: []string{"cn"}}, definition: defs["buddy_box"]}, up: up}},
	}
}

type lotteryModule struct{ spendRewardModule }

func (m *lotteryModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	if summary, ok := m.consumedToday(a, "lottery"); ok {
		return observation("done", summary), nil
	}
	if a.Realm() != "cn" || m.up == nil {
		return taskengine.Observation{}, errors.New("lottery_unavailable")
	}
	resources, err := m.up.TaskResources(ctx, a)
	if err != nil {
		return taskengine.Observation{}, err
	}
	chances := resources.Chances
	if chances < 0 {
		return taskengine.Observation{}, errors.New("lottery_state_invalid")
	}
	if chances == 0 {
		return observation("done", "当前没有抽奖机会；未提交抽奖。"), nil
	}
	return taskengine.Observation{Phase: "ready", Total: int64(chances), Summary: fmt.Sprintf("官方可用抽奖机会 %d 次；执行只抽一次。", chances)}, nil
}
func (m *lotteryModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	state, err := m.Inspect(ctx, a, t)
	if err != nil || state.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, state), err
	}
	result, err := m.up.GrowthLotteryDraw(a, "")
	if err != nil {
		if upstream.IsLotteryNoChance(err) || upstream.IsLotteryDisabled(err) {
			return taskengine.NotSubmitted(t, taskengine.StageExecute, observation("done", "官方机会已用尽或活动未开启；未产生抽奖。")), nil
		}
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "抽奖结果未知，保留待核验，不重试。")), err
	}
	// A returned prize is an accepted draw, but the authoritative state is the
	// refreshed chance balance. Never derive success from HTTP alone.
	if _, readErr := m.Inspect(ctx, a, t); readErr != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "抽奖已提交但机会余额未能回读，保留待核验。")), readErr
	}
	if result != nil && (result.PrizeName != "" || result.PrizeCode != "" || result.CreditAmount > 0) {
		summary := "官方返回抽奖结果：" + firstNonEmpty(result.PrizeName, result.PrizeCode)
		if result.PrizeType == "physical" {
			summary += "（实物奖，需到官方成长中心填写收件信息）"
		}
		m.markConsumed(a, "lottery", summary)
		return taskengine.Accepted(t, taskengine.StageExecute, taskengine.Observation{Phase: "done", Summary: summary}), nil
	}
	summary := "抽奖已提交，官方未返回可识别的奖品字段；机会余额回读已刷新。"
	m.markConsumed(a, "lottery", summary)
	return taskengine.Accepted(t, taskengine.StageExecute, taskengine.Observation{Phase: "done", Summary: summary}), nil
}
func (m *lotteryModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	return discoverSpend(m, ctx, a)
}
func (m *lotteryModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}

type buddyBoxModule struct{ spendRewardModule }

func (m *buddyBoxModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	if summary, ok := m.consumedToday(a, "buddy_box"); ok {
		return observation("done", summary), nil
	}
	if a.Realm() != "cn" || m.up == nil {
		return taskengine.Observation{}, errors.New("buddy_box_unavailable")
	}
	resources, err := m.up.TaskResources(ctx, a)
	if err != nil {
		return taskengine.Observation{}, err
	}
	quota := &resources.Quota
	if quota.Affordable < 0 || quota.MaxOpenCount < 0 {
		return taskengine.Observation{}, errors.New("buddy_box_state_invalid")
	}
	if quota.Affordable == 0 {
		return observation("done", "当前能量不足一次开箱；未提交。"), nil
	}
	limit := quota.MaxOpenCount
	if limit <= 0 {
		limit = 1
	}
	return taskengine.Observation{Phase: "ready", Total: int64(min(quota.Affordable, limit)), Summary: fmt.Sprintf("能量可开 %d 个，单次上限 %d；执行只开一次。", quota.Affordable, limit)}, nil
}
func (m *buddyBoxModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	state, err := m.Inspect(ctx, a, t)
	if err != nil || state.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, state), err
	}
	name, err := m.up.GrowthBuddyOpen(a, 1, "")
	if err != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "开箱结果未知，保留待核验，不重试。")), err
	}
	if _, readErr := m.Inspect(ctx, a, t); readErr != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "开箱已提交但额度未能回读，保留待核验。")), readErr
	}
	summary := "已按官方额度开一个 Buddy 盲盒"
	if name != "" {
		summary += "：" + name
	}
	m.markConsumed(a, "buddy_box", summary)
	return taskengine.Accepted(t, taskengine.StageExecute, taskengine.Observation{Phase: "done", Summary: summary}), nil
}
func (m *buddyBoxModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	return discoverSpend(m, ctx, a)
}
func (m *buddyBoxModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}

func discoverSpend(m interface {
	Inspect(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error)
	Spec() ModuleSpec
}, ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	state, err := m.Inspect(ctx, a, taskengine.Task{Definition: taskengine.Definition{}})
	if err != nil {
		return nil, err
	}
	def := taskengine.Definition{Key: m.Spec().Key, Name: m.Spec().ID, SideEffect: true, OncePerDay: true, MaxAttempts: 1, PeriodKey: time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02"), Stages: []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify}}
	return []ModuleTask{{Definition: def, State: state}}, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
