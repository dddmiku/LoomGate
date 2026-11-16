package taskproviders

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
	"workbuddy2api/internal/taskengine"
)

type Doubao struct {
	Vault  *accountvault.Vault
	Client *doubao.Client
}

func (a *Doubao) Discover(ctx context.Context, scope taskengine.Scope) ([]taskengine.Task, error) {
	catalog, err := a.DiscoverCatalog(ctx, scope, "")
	return catalog.Tasks, err
}
func (a *Doubao) DiscoverCatalog(ctx context.Context, scope taskengine.Scope, key string) (taskengine.Catalog, error) {
	accounts, err := a.Vault.List()
	if err != nil {
		return taskengine.Catalog{}, err
	}
	result := taskengine.Catalog{Tasks: []taskengine.Task{}, Issues: []taskengine.DiscoveryIssue{}}
	for _, account := range accounts {
		if account.Provider != "doubao" || !account.Enabled {
			continue
		}
		if len(scope.AccountIDs) > 0 {
			found := false
			for _, id := range scope.AccountIDs {
				found = found || id == account.ID
			}
			if !found {
				continue
			}
		}
		activity, err := a.Client.Activities(ctx, account.ID)
		if err != nil {
			code := "module_discovery_failed"
			var upstream *doubao.APIError
			if errors.As(err, &upstream) {
				switch upstream.Kind {
				case "desktop_activity_unavailable", "desktop_identity_mismatch", "desktop_login_required":
					code = upstream.Kind
				}
			}
			result.Issues = append(result.Issues, taskengine.DiscoveryIssue{ProviderID: "doubao", AccountID: account.ID, ModuleID: "activities", ErrorCode: code})
			continue
		}
		for _, t := range append(append([]doubao.Task{}, activity.Tasks...), activity.Milestones...) {
			definition := taskengine.Definition{Key: t.Group + ":" + t.Code, ProviderID: "doubao", Name: t.Title, Description: t.Description, SideEffect: true, MaxAttempts: 1, Stages: []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageClaim, taskengine.StageVerify}}
			if key != "" && definition.Key != key {
				continue
			}
			if definition.Description == "" {
				definition.Description = t.Subtitle
			}
			if t.Daily != nil {
				definition.Reward = fmt.Sprintf("%d 额度", t.Daily.Reward.Amount)
				definition.OncePerDay = true
				// The upstream flag/date defines the period. No local midnight assumption.
				definition.PeriodKey = "after:" + t.Daily.Date
				if t.Daily.Checked {
					definition.PeriodKey = "checked:" + t.Daily.Date
				}
			} else {
				for _, reward := range t.Rewards {
					unit := "奖励"
					if reward.Type == "quota_grant" {
						unit = "额度"
					}
					definition.Reward += fmt.Sprintf("%d %s ", reward.Amount, unit)
				}
				definition.Reward = strings.TrimSpace(definition.Reward)
				definition.Once = t.Kind == "points_milestone" || t.Type == "select_work_fields" || t.Type == "GenWord" || t.Type == "GenExcel" || t.Type == "GenPPT" || t.Type == "ScheduleSkill"
				if t.Type == "WorkTaskMode" && t.ID != "" {
					definition.OncePerDay = true
					definition.PeriodKey = "instance:" + t.ID
				}
			}
			state := doubaoObservation(t, activity.TotalAmount)
			definition.ManualOnly = t.Daily == nil && t.Kind != "points_milestone" && state.Phase != "done" && state.Phase != "claimable"
			if state.Phase == "manual" {
				definition.Description = strings.TrimSpace(definition.Description + " " + state.Summary)
			}
			result.Tasks = append(result.Tasks, taskengine.Task{Definition: definition, Target: taskengine.Target{AccountID: account.ID, Realm: "doubao"}, State: &state})
		}
	}
	return result, nil
}
func (a *Doubao) find(ctx context.Context, t taskengine.Task) (doubao.Task, doubao.Activity, error) {
	activity, err := a.Client.Activities(ctx, t.AccountID)
	if err != nil {
		return doubao.Task{}, activity, err
	}
	for _, x := range append(append([]doubao.Task{}, activity.Tasks...), activity.Milestones...) {
		if x.Group+":"+x.Code == t.Key {
			return x, activity, nil
		}
	}
	return doubao.Task{}, activity, errors.New("doubao_task_expired")
}
func doubaoObservation(t doubao.Task, total int64) taskengine.Observation {
	o := taskengine.Observation{Phase: "manual", Summary: "请在官方豆包左侧“免费用豆包工作”完成此项真实工作，然后刷新状态。"}
	if t.Status == 3 && t.Daily == nil {
		o.Phase = "done"
		o.Summary = "上游已确认奖励领取完成。"
		return o
	}
	if t.Daily != nil {
		if t.Daily.Checked {
			o.Phase = "done"
			o.Summary = "上游确认今日已签到。"
		} else {
			o.Phase = "ready"
			o.Summary = "上游确认今日尚未签到。"
		}
		return o
	}
	if t.Kind == "points_milestone" {
		o.Current = total
		o.Total = t.Threshold
		if t.Status == 2 || (t.RewardMode == 2 && t.Threshold > 0 && total >= t.Threshold) {
			o.Phase = "claimable"
			o.Summary = "上游累计进度已满足奖励条件，可领取后复核。"
		} else {
			o.Summary = fmt.Sprintf("累计进度 %d / %d；完成其他真实任务后再核验奖励。", total, t.Threshold)
		}
		return o
	}
	if t.Status == 2 {
		if t.ID != "" || t.RewardMode == 2 {
			o.Phase = "claimable"
			o.Summary = "上游任务已达标，可领取后复核。"
		} else {
			o.Summary = "任务显示达标，但缺少可核验的实例标识；请在官方任务页刷新。"
		}
		return o
	}
	if t.Status == 4 {
		o.Phase = "pending"
		o.Summary = "上游工作正在进行，等待完成后核验。"
		return o
	}
	if t.Type == "select_work_fields" {
		o.Summary = "请在官方任务页选择你真实的工作领域，再刷新状态；网关不会代填个人资料。"
	}
	return o
}
func (a *Doubao) Inspect(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	current, activity, err := a.find(ctx, t)
	if err != nil {
		return taskengine.Observation{}, err
	}
	return doubaoObservation(current, activity.TotalAmount), nil
}
func (a *Doubao) Execute(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	current, activity, err := a.find(ctx, t)
	if err != nil {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{}), err
	}
	state := doubaoObservation(current, activity.TotalAmount)
	if state.Phase != "ready" || current.Daily == nil {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, state), nil
	}
	_, err = a.Client.TaskAction(ctx, t.AccountID, "task_checkin", current)
	return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending"}), err
}
func (a *Doubao) Claim(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	current, activity, err := a.find(ctx, t)
	if err != nil {
		return taskengine.NotSubmitted(t, taskengine.StageClaim, taskengine.Observation{}), err
	}
	state := doubaoObservation(current, activity.TotalAmount)
	if state.Phase != "claimable" {
		return taskengine.NotSubmitted(t, taskengine.StageClaim, state), nil
	}
	_, err = a.Client.TaskAction(ctx, t.AccountID, "task_claim", current)
	return taskengine.UnknownAction(t, taskengine.StageClaim, taskengine.Observation{Phase: "pending"}), err
}
func (a *Doubao) Verify(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return a.Inspect(ctx, t)
}
