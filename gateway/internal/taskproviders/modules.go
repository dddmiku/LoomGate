package taskproviders

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

//go:embed definitions.json
var moduleDefinitions []byte

type moduleBase struct {
	spec       ModuleSpec
	definition taskengine.Definition
}

func (m moduleBase) Spec() ModuleSpec { return m.spec }
func (m moduleBase) Discover(ctx context.Context, a *auth.Auth) ([]taskengine.Definition, error) {
	d := m.definition
	if d.OncePerDay {
		location, _ := time.LoadLocation("Asia/Shanghai")
		d.PeriodKey = time.Now().In(location).Format("2006-01-02")
	}
	return []taskengine.Definition{d}, ctx.Err()
}
func (moduleBase) Execute(_ context.Context, _ *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{Phase: "manual"}), nil
}
func (moduleBase) Claim(_ context.Context, _ *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return taskengine.NotSubmitted(t, taskengine.StageClaim, taskengine.Observation{}), errors.New("claim_unsupported")
}

func DefaultModules(a *Accounts) ([]Module, error) {
	var defs map[string]taskengine.Definition
	if json.Unmarshal(moduleDefinitions, &defs) != nil {
		return nil, errors.New("task_definitions_invalid")
	}
	for _, id := range []string{"checkin", "growth", "balance", "travel", "redemption", "makeup", "resources", "compensation", "lottery", "buddy_box"} {
		if defs[id].Name == "" {
			return nil, errors.New("task_definition_missing")
		}
	}
	if a.options.RefreshVault != nil && defs["keepalive"].Name == "" {
		return nil, errors.New("task_definition_missing")
	}
	check := &checkinModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "checkin", Key: "checkin", Realms: []string{"cn"}}, definition: defs["checkin"]}, up: a.Upstream}
	if a.Provider == "trae" {
		modules, err := newTraeModules(a, defs)
		if err != nil {
			return nil, err
		}
		if a.options.RefreshVault != nil {
			return append(modules, newKeepaliveModule(a, defs["keepalive"])), nil
		}
		return modules, nil
	}
	if a.Provider != "workbuddy" {
		return nil, errors.New("provider_unsupported")
	}
	modules := []Module{check, &balanceModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "balance", Key: "balance", Realms: []string{"cn", "global"}}, definition: defs["balance"]}, owner: a}, &growthModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "growth", Key: "growth/", Prefix: true, Realms: []string{"cn"}}, definition: defs["growth"]}, up: a.Upstream}}
	modules = append(modules, newLifecycleModules(a.Upstream, defs)...)
	// Single-shot spends require a durable attempt journal. Without a spend
	// vault, offering the task would permit an unprovable replay after a
	// restart, so the modules are not registered at all.
	if a.options.SpendVault != nil {
		modules = append(modules, newRewardModules(a, defs)...)
	}
	if a.options.RefreshVault != nil {
		modules = append(modules, newKeepaliveModule(a, defs["keepalive"]))
	}
	return modules, nil
}

type checkinModule struct {
	moduleBase
	up         *upstream.Client
	traeStatus func(context.Context, *auth.Auth) (trae.CheckinStatus, error)
	traeClaim  func(context.Context, *auth.Auth) error
}

func (m *checkinModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	phase := "blocked"
	if a.Realm() == "trae" {
		if m.traeStatus == nil {
			return taskengine.Observation{}, errors.New("checkin_unavailable")
		}
		s, err := m.traeStatus(ctx, a)
		if err != nil {
			return taskengine.Observation{}, err
		}
		if s.Enabled {
			phase = "ready"
		}
		if s.CheckedIn {
			phase = "done"
		}
	} else {
		if a.Realm() != "cn" || m.up == nil {
			return taskengine.Observation{}, errors.New("checkin_unavailable")
		}
		s, err := m.up.TaskCheckinStatus(ctx, a)
		if err != nil {
			return taskengine.Observation{}, err
		}
		if s.Active == nil || *s.Active {
			phase = "ready"
		}
		if s.TodayCheckedIn {
			phase = "done"
		}
	}
	return taskengine.Observation{Phase: phase}, nil
}
func (m *checkinModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	// Recheck under the engine account lock, including externally completed signins.
	s, err := m.Inspect(ctx, a, t)
	if err != nil || s.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, s), err
	}
	if a.Realm() == "trae" {
		err = m.traeClaim(ctx, a)
	} else {
		err = m.up.TaskCheckin(ctx, a)
		if upstream.IsAlreadyCheckin(err) {
			err = nil
		}
	}
	return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending"}), err
}
func (m *checkinModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}

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

type growthModule struct {
	moduleBase
	up *upstream.Client
}

func (m *growthModule) Discover(ctx context.Context, a *auth.Auth) ([]taskengine.Definition, error) {
	if m.up == nil {
		return nil, errors.New("growth_unavailable")
	}
	items, err := m.up.TaskCatalog(ctx, a)
	if err != nil {
		return nil, err
	}
	out := []taskengine.Definition{}
	for _, item := range items {
		code := item.TaskID()
		if code == "" || !taskengine.IDPattern.MatchString(code) {
			continue
		}
		base := m.definition
		base.Name = item.Title
		if base.Name == "" {
			base.Name = m.definition.Name
		}
		base.Description = item.Description
		if base.Description == "" {
			base.Description = m.definition.Description
		}
		base.Reward = fmt.Sprintf("%d 积分 · %d 能量", item.RewardCredit, item.RewardEnergy)
		// Accepting is its own verifiable step: the official directory must show
		// accepted/in_progress/completed afterwards. Claiming stays separate and
		// only happens after the human does the real official work.
		accept := base
		accept.Key = "growth/accept/" + code
		accept.Name = "接单：" + base.Name
		accept.Description = "在官方成长中心开始追踪此任务；接单不代表完成，真实工作仍需在官方客户端进行。"
		accept.Stages = []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify}
		accept.Once = false
		accept.OncePerDay = false
		accept.PeriodKey = ""
		out = append(out, accept)

		claim := base
		claim.Key = "growth/" + code
		out = append(out, claim)
	}
	return out, nil
}

func growthCode(t taskengine.Task) (string, bool) {
	if strings.HasPrefix(t.Key, "growth/accept/") {
		code := strings.TrimPrefix(t.Key, "growth/accept/")
		return code, true
	}
	if strings.HasPrefix(t.Key, "growth/") {
		return strings.TrimPrefix(t.Key, "growth/"), false
	}
	return "", false
}

func (m *growthModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	code, accept := growthCode(t)
	items, err := m.up.TaskCatalog(ctx, a)
	if err != nil {
		return taskengine.Observation{}, err
	}
	for _, x := range items {
		if x.TaskID() != code {
			continue
		}
		if accept {
			return growthAcceptState(x), nil
		}
		return growthClaimState(x), nil
	}
	return taskengine.Observation{Phase: "blocked", Summary: "该任务已不在官方目录。"}, nil
}

func growthAcceptState(x upstream.GrowthTask) taskengine.Observation {
	current, total := int64(x.Progress.Current), int64(x.Progress.Target)
	if x.Locked {
		return taskengine.Observation{Phase: "blocked", Summary: "官方将此任务标记为锁定，当前不可接单。", Current: current, Total: total}
	}
	switch x.State() {
	case "not_accepted":
		return taskengine.Observation{Phase: "ready", Summary: "官方任务可接单；接单后仍需在官方客户端完成真实工作。", Current: current, Total: total}
	case "accepted", "in_progress":
		return taskengine.Observation{Phase: "done", Summary: "官方确认任务已接单并开始追踪进度。", Current: current, Total: total}
	case "completed", "claimed":
		return taskengine.Observation{Phase: "done", Summary: "官方确认任务已接单。", Current: current, Total: total}
	}
	return taskengine.Observation{Phase: "blocked", Summary: "官方任务状态无法识别。", Current: current, Total: total}
}

func growthClaimState(x upstream.GrowthTask) taskengine.Observation {
	current, total := int64(x.Progress.Current), int64(x.Progress.Target)
	if x.Locked {
		return taskengine.Observation{Phase: "blocked", Summary: "官方将此任务标记为锁定，当前不可执行。", Current: current, Total: total}
	}
	switch x.State() {
	case "claimed":
		return taskengine.Observation{Phase: "done", Summary: "官方确认该成长任务奖励已领取。", Current: current, Total: total}
	case "completed":
		return taskengine.Observation{Phase: "claimable", Summary: "官方确认任务已完成，可领取后复核。", Current: current, Total: total}
	case "not_accepted":
		return taskengine.Observation{Phase: "manual", Summary: "请先执行“接单”任务，再在官方客户端完成真实工作。", Current: current, Total: total}
	case "accepted", "in_progress":
		if total > 0 && current >= total {
			return taskengine.Observation{Phase: "claimable", Summary: "官方进度已达标，可领取后复核。", Current: current, Total: total}
		}
		return taskengine.Observation{Phase: "manual", Summary: fmt.Sprintf("已接单，官方进度 %d/%d；请完成真实工作。", current, total), Current: current, Total: total}
	}
	return taskengine.Observation{Phase: "blocked", Summary: "官方任务状态无法识别。", Current: current, Total: total}
}

func (m *growthModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	code, accept := growthCode(t)
	if !accept {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{Phase: "manual"}), nil
	}
	current, err := m.Inspect(ctx, a, t)
	if err != nil || current.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, current), err
	}
	results, err := m.up.TaskGrowthAccept(ctx, a, []string{code})
	if err != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "接单结果未知，保留待核验，不重复接单。")), err
	}
	status := results[code]
	if status == "" || status == "error" {
		// An explicit per-code refusal is a documented no-effect result; the
		// next Inspect re-reads the official state before any retry.
		return taskengine.Rejected(t, taskengine.StageExecute, 0, observation("manual", "官方拒绝接单；当前任务不进入执行。")), nil
	}
	return taskengine.Accepted(t, taskengine.StageExecute, observation("pending", "官方已接受接单，等待目录回读确认。")), nil
}

func (m *growthModule) Claim(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	code, accept := growthCode(t)
	if accept {
		return taskengine.NotSubmitted(t, taskengine.StageClaim, taskengine.Observation{Phase: "blocked"}), errors.New("claim_unsupported")
	}
	current, err := m.Inspect(ctx, a, t)
	if err != nil || current.Phase != "claimable" {
		return taskengine.NotSubmitted(t, taskengine.StageClaim, current), err
	}
	err = m.up.TaskClaim(ctx, a, code)
	return taskengine.UnknownAction(t, taskengine.StageClaim, taskengine.Observation{Phase: "pending"}), err
}
func (m *growthModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
