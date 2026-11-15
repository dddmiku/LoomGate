// Package taskproviders translates upstream business state into taskengine
// observations. Provider IDs and endpoint details never enter the scheduler.
package taskproviders

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

type Accounts struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	Provider string
}

func (a *Accounts) account(t taskengine.Task) (*auth.Auth, error) {
	s, ok := a.Pool.Status(t.AccountID)
	if !ok || s.Disabled {
		return nil, errors.New("account_unavailable")
	}
	auth := a.Pool.AuthByUID(t.AccountID)
	if auth == nil {
		return nil, errors.New("account_unavailable")
	}
	if (a.Provider == "trae" && auth.Realm() != "trae") || (a.Provider == "workbuddy" && !auth.IsWorkBuddy()) {
		return nil, errors.New("provider_mismatch")
	}
	return auth, nil
}
func (a *Accounts) Discover(ctx context.Context, scope taskengine.Scope) ([]taskengine.Task, error) {
	tasks := []taskengine.Task{}
	loc, _ := time.LoadLocation("Asia/Shanghai")
	for _, s := range a.Pool.List() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if s.Disabled {
			continue
		}
		if len(scope.AccountIDs) > 0 {
			found := false
			for _, id := range scope.AccountIDs {
				if id == s.UID {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		if (a.Provider == "trae" && s.Realm != "trae") || (a.Provider == "workbuddy" && s.Realm != "cn" && s.Realm != "global") {
			continue
		}
		target := taskengine.Target{AccountID: s.UID, Realm: s.Realm}
		tasks = append(tasks, taskengine.Task{Definition: taskengine.Definition{Key: "checkin", ProviderID: a.Provider, Name: "每日签到", OncePerDay: true, PeriodKey: time.Now().In(loc).Format("2006-01-02"), SideEffect: true, Stages: []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify}}, Target: target})
		if a.Provider != "workbuddy" || s.Realm != "cn" {
			continue
		}
		auth := a.Pool.AuthByUID(s.UID)
		catalog, err := a.Upstream.TaskCatalog(ctx, auth)
		if err != nil {
			return nil, err
		}
		for _, item := range catalog {
			if item.TaskCode == "" {
				continue
			}
			tasks = append(tasks, taskengine.Task{Definition: taskengine.Definition{Key: "growth/" + item.TaskCode, ProviderID: a.Provider, Name: item.Title, Description: item.Description, SideEffect: true, Reward: fmt.Sprintf("%d 积分 · %d 能量", item.RewardCredit, item.RewardEnergy), Stages: []taskengine.Stage{taskengine.StageInspect, taskengine.StageClaim, taskengine.StageVerify}}, Target: target})
		}
	}
	return tasks, nil
}
func (a *Accounts) Inspect(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	account, err := a.account(t)
	if err != nil {
		return taskengine.Observation{}, err
	}
	phase := "blocked"
	if t.Key == "checkin" {
		if a.Provider == "trae" {
			s, err := trae.CheckinStatusOf(ctx, account)
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
			s, err := a.Upstream.TaskCheckinStatus(ctx, account)
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
	if a.Provider == "workbuddy" && strings.HasPrefix(t.Key, "growth/") {
		catalog, err := a.Upstream.TaskCatalog(ctx, account)
		if err != nil {
			return taskengine.Observation{}, err
		}
		for _, x := range catalog {
			if "growth/"+x.TaskCode != t.Key {
				continue
			}
			phase = "manual"
			if x.Locked {
				phase = "blocked"
			}
			if x.State() == "claimed" {
				phase = "done"
			}
			if x.State() == "completed" {
				phase = "claimable"
			}
			return taskengine.Observation{Phase: phase, Current: int64(x.Progress.Current), Total: int64(x.Progress.Target), Summary: "请在 WorkBuddy 官方客户端完成任务要求后刷新状态"}, nil
		}
	}
	return taskengine.Observation{Phase: "blocked"}, nil
}
func (a *Accounts) Execute(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	account, err := a.account(t)
	if err != nil {
		return taskengine.Observation{}, err
	}
	if t.Key != "checkin" {
		return taskengine.Observation{Phase: "manual"}, nil
	}
	if a.Provider == "trae" {
		_, err = trae.ClaimCheckin(ctx, account, 0)
	} else {
		err = a.Upstream.TaskCheckin(ctx, account)
		if upstream.IsAlreadyCheckin(err) {
			err = nil
		}
	}
	return taskengine.Observation{Phase: "pending"}, err
}
func (a *Accounts) Claim(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	account, err := a.account(t)
	if err != nil {
		return taskengine.Observation{}, err
	}
	if a.Provider != "workbuddy" || !strings.HasPrefix(t.Key, "growth/") {
		return taskengine.Observation{}, errors.New("claim_unsupported")
	}
	err = a.Upstream.TaskClaim(ctx, account, strings.TrimPrefix(t.Key, "growth/"))
	return taskengine.Observation{Phase: "pending"}, err
}
func (a *Accounts) Verify(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return a.Inspect(ctx, t)
}
