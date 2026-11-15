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
	accounts, err := a.Vault.List()
	if err != nil {
		return nil, err
	}
	out := []taskengine.Task{}
	for _, account := range accounts {
		if account.Provider != "doubao" || !account.Enabled {
			continue
		}
		if len(scope.AccountIDs) > 0 {
			found := false
			for _, id := range scope.AccountIDs {
				if id == account.ID {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		activity, err := a.Client.Activities(ctx, account.ID)
		if err != nil {
			return nil, err
		}
		tasks := append(append([]doubao.Task{}, activity.Tasks...), activity.Milestones...)
		for _, t := range tasks {
			reward := ""
			if t.Daily != nil {
				reward = fmt.Sprintf("%d 额度", t.Daily.Reward.Amount)
			} else {
				for _, r := range t.Rewards {
					reward += fmt.Sprintf("%d %s ", r.Amount, r.Type)
				}
			}
			description := t.Description
			if description == "" {
				description = t.Subtitle
			}
			if t.Daily == nil {
				description += " 请在官方豆包工作界面完成对应工作，完成后刷新并核实。"
			}
			out = append(out, taskengine.Task{Definition: taskengine.Definition{Key: t.Group + ":" + t.Code, ProviderID: "doubao", Name: t.Title, Description: description, Reward: strings.TrimSpace(reward), SideEffect: true, Stages: []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify}}, Target: taskengine.Target{AccountID: account.ID, Realm: "doubao"}})
		}
	}
	return out, nil
}
func (a *Doubao) find(ctx context.Context, t taskengine.Task) (doubao.Task, error) {
	activity, err := a.Client.Activities(ctx, t.AccountID)
	if err != nil {
		return doubao.Task{}, err
	}
	for _, x := range append(append([]doubao.Task{}, activity.Tasks...), activity.Milestones...) {
		if x.Group+":"+x.Code == t.Key {
			return x, nil
		}
	}
	return doubao.Task{}, errors.New("doubao_task_expired")
}
func (a *Doubao) Inspect(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	current, err := a.find(ctx, t)
	if err != nil {
		return taskengine.Observation{}, err
	}
	phase := "manual"
	if current.Status == 0 {
		phase = "blocked"
	}
	if current.Status == 3 {
		phase = "done"
	}
	if current.Daily != nil {
		if current.Daily.Checked {
			phase = "done"
		} else {
			phase = "ready"
		}
	}
	return taskengine.Observation{Phase: phase}, nil
}
func (a *Doubao) Execute(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	current, err := a.find(ctx, t)
	if err != nil {
		return taskengine.Observation{}, err
	}
	if current.Daily == nil {
		return taskengine.Observation{Phase: "manual"}, nil
	}
	if current.Daily.Checked {
		return taskengine.Observation{Phase: "done"}, nil
	}
	err = a.Client.Checkin(ctx, t.AccountID, current.Group)
	return taskengine.Observation{Phase: "pending"}, err
}
func (a *Doubao) Claim(context.Context, taskengine.Task) (taskengine.Observation, error) {
	return taskengine.Observation{Phase: "manual"}, nil
}
func (a *Doubao) Verify(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return a.Inspect(ctx, t)
}
