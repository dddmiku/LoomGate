package taskproviders

// 任务：成长任务（动态目录：接单 + 领奖）
//
// 任务码来自官方目录本身，不写死旧活动。接单与完成分离：接单只证明官方开始
// 追踪，真实工作仍需在官方客户端完成；领奖只领取官方 completed 或进度达标
// 的任务。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

func newGrowthModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" {
		return nil, nil
	}
	return &growthModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "growth", Key: "growth/", Prefix: true, Realms: []string{"cn"}},
			definition: taskengine.Definition{
				Key:         "growth/",
				Name:        "成长任务",
				Description: "任务标识和奖励来自官方目录；未达标需要在官方客户端完成实际工作。",
				SideEffect:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect, taskengine.StageClaim, taskengine.StageVerify},
			},
		},
		up: a.Upstream,
	}, nil
}

var growthPlan = &DefaultPlan{ID: "legacy-growth", TaskKey: "growth/", Hours: []int{12}, LegacyEnabled: boolValue(true)}

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
