package taskproviders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

// spendRewardModule consumes at most one official lottery chance or Buddy
// energy unit per business period. Its truth is a vault CAS record bound to
// the engine ActionReference, the pre-spend resource snapshot and the stable
// client_token; the in-process map only caches that durable state.
type spendRewardModule struct {
	moduleBase
	up     *upstream.Client
	vault  *accountvault.Vault
	mu     sync.Mutex
	memory map[string]string
}

type spendJournal struct {
	Version     int       `json:"version"`
	Kind        string    `json:"kind"`
	Account     string    `json:"account"`
	Period      string    `json:"period"`
	Reference   string    `json:"reference"`
	Stage       string    `json:"stage"`
	State       string    `json:"state"`
	Before      int       `json:"before"`
	After       int       `json:"after,omitempty"`
	ClientToken string    `json:"client_token"`
	Summary     string    `json:"summary,omitempty"`
	Started     time.Time `json:"started"`
	Updated     time.Time `json:"updated"`
}

func spendVaultID(account, kind string) string {
	sum := sha256.Sum256([]byte(account + "\x00" + kind))
	return "spend-" + hex.EncodeToString(sum[:])
}

func (m *spendRewardModule) loadSpend(a *auth.Auth, kind string) (spendJournal, uint64, error) {
	var value spendJournal
	if m.vault == nil {
		return value, 0, errors.New("spend_store_unavailable")
	}
	meta, raw, err := m.vault.Get(spendVaultID(a.Snapshot().UID, kind))
	if err != nil {
		return value, 0, err
	}
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	if json.Unmarshal(raw, &value) != nil || value.Version != 1 || value.Kind != kind || value.Account != a.Snapshot().UID {
		return value, 0, errors.New("spend_store_invalid")
	}
	return value, meta.Revision, nil
}

func (m *spendRewardModule) saveSpend(a *auth.Auth, value spendJournal, rev uint64) (uint64, error) {
	if m.vault == nil {
		return 0, errors.New("spend_store_unavailable")
	}
	value.Version = 1
	raw, err := json.Marshal(value)
	if err != nil {
		return 0, errors.New("spend_store_invalid")
	}
	meta, err := m.vault.Put(accountvault.Account{ID: spendVaultID(a.Snapshot().UID, value.Kind), Provider: "workbuddy-spend", Enabled: true}, raw, rev)
	return meta.Revision, err
}

func (m *spendRewardModule) currentPeriod(a *auth.Auth) string {
	return time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
}

func (m *spendRewardModule) settledToday(a *auth.Auth, kind string) (string, bool, error) {
	j, _, err := m.loadSpend(a, kind)
	if err != nil {
		if errors.Is(err, accountvault.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	if j.Period != m.currentPeriod(a) || j.State != "accepted" {
		return "", false, nil
	}
	return j.Summary, true, nil
}

func (m *spendRewardModule) beginSpend(a *auth.Auth, kind, period, stage, reference string, before int) (spendJournal, uint64, error) {
	if reference == "" {
		return spendJournal{}, 0, errors.New("spend_attempt_missing")
	}
	old, rev, err := m.loadSpend(a, kind)
	if err != nil && !errors.Is(err, accountvault.ErrNotFound) {
		return spendJournal{}, 0, err
	}
	if err == nil && old.Period == period {
		switch old.State {
		case "accepted", "unknown", "attempting", "unresolved":
			return old, rev, errors.New("spend_previous_attempt_unresolved")
		case "not_submitted":
			// Explicit provider refusal proved no effect; a later chance is
			// allowed to create a new attempt on top of the same record.
		default:
			return old, rev, errors.New("spend_state_invalid")
		}
	}
	value := spendJournal{Version: 1, Kind: kind, Account: a.Snapshot().UID, Period: period, Reference: reference, Stage: stage, State: "attempting", Before: before, ClientToken: kind + "-" + reference[:32], Started: time.Now().UTC(), Updated: time.Now().UTC()}
	rev, err = m.saveSpend(a, value, rev)
	return value, rev, err
}

func (m *spendRewardModule) finishSpend(a *auth.Auth, value spendJournal, rev uint64, state, summary string, after int) error {
	value.State = state
	value.Summary = summary
	value.After = after
	value.Updated = time.Now().UTC()
	_, err := m.saveSpend(a, value, rev)
	return err
}

func newRewardModules(a *Accounts, defs map[string]taskengine.Definition) []Module {
	return []Module{
		&lotteryModule{spendRewardModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "lottery", Key: "lottery", Realms: []string{"cn"}}, definition: defs["lottery"]}, up: a.Upstream, vault: a.options.SpendVault}},
		&buddyBoxModule{spendRewardModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "buddy_box", Key: "buddy_box", Realms: []string{"cn"}}, definition: defs["buddy_box"]}, up: a.Upstream, vault: a.options.SpendVault}},
	}
}

type lotteryModule struct{ spendRewardModule }

func (m *lotteryModule) inspectRead(ctx context.Context, a *auth.Auth) (taskengine.Observation, error) {
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
		// No submission happened. Returning done here would persist a false
		// completion and hide a chance granted later the same day.
		return observation("skipped", "当前没有抽奖机会；本次只读跳过，未提交。"), nil
	}
	return taskengine.Observation{Phase: "ready", Total: int64(chances), Summary: fmt.Sprintf("官方可用抽奖机会 %d 次；执行只抽一次。", chances)}, nil
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
	resources, readErr := m.up.TaskResources(ctx, a)
	if readErr != nil {
		_ = m.finishSpend(a, journal, rev, "unknown", "抽奖已返回奖品但机会余额未能回读，保留待核验。", journal.Before)
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "抽奖已返回奖品但机会余额未能回读，保留待核验。")), readErr
	}
	if resources.Chances >= journal.Before {
		_ = m.finishSpend(a, journal, rev, "unknown", "抽奖有奖品回执但机会余额未减少，保留待核验。", resources.Chances)
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "抽奖有奖品回执但机会余额未减少，保留待核验。")), errors.New("lottery_state_unconfirmed")
	}
	summary := "官方返回抽奖结果：" + firstNonEmpty(result.PrizeName, result.PrizeCode)
	if result.PrizeType == "physical" {
		summary += "（实物奖，需到官方成长中心填写收件信息）"
	}
	if err = m.finishSpend(a, journal, rev, "accepted", summary, resources.Chances); err != nil {
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

type buddyBoxModule struct{ spendRewardModule }

func (m *buddyBoxModule) inspectRead(ctx context.Context, a *auth.Auth) (taskengine.Observation, error) {
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
	resources, readErr := m.up.TaskResources(ctx, a)
	if readErr != nil {
		_ = m.finishSpend(a, journal, rev, "unknown", "开箱已提交但额度未能回读，保留待核验。", journal.Before)
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "开箱已提交但额度未能回读，保留待核验。")), readErr
	}
	if resources.Quota.Affordable >= journal.Before {
		_ = m.finishSpend(a, journal, rev, "unknown", "开箱请求返回但官方额度未减少，保留待核验。", resources.Quota.Affordable)
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "开箱请求返回但官方额度未减少，保留待核验。")), errors.New("buddy_box_state_unconfirmed")
	}
	summary := "已按官方额度开一个 Buddy 盲盒"
	if name != "" {
		summary += "：" + name
	}
	if err = m.finishSpend(a, journal, rev, "accepted", summary, resources.Quota.Affordable); err != nil {
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

func discoverSpend(m interface {
	Inspect(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error)
	Spec() ModuleSpec
	Discover(context.Context, *auth.Auth) ([]taskengine.Definition, error)
}, ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	state, err := m.Inspect(ctx, a, taskengine.Task{Definition: taskengine.Definition{}})
	if err != nil {
		return nil, err
	}
	defs, err := m.Discover(ctx, a)
	if err != nil || len(defs) != 1 {
		return nil, errors.New("spend_definition_invalid")
	}
	def := defs[0]
	def.SideEffect = true
	def.OncePerDay = true
	def.MaxAttempts = 1
	def.PeriodKey = time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
	def.Stages = []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify}
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
