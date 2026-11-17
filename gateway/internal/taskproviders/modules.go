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
		if item.TaskCode == "" {
			continue
		}
		d := m.definition
		d.Key = "growth/" + item.TaskCode
		d.Name = item.Title
		if d.Name == "" {
			d.Name = m.definition.Name
		}
		d.Description = item.Description
		if d.Description == "" {
			d.Description = m.definition.Description
		}
		d.Reward = fmt.Sprintf("%d 积分 · %d 能量", item.RewardCredit, item.RewardEnergy)
		out = append(out, d)
	}
	return out, nil
}
func (m *growthModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	items, err := m.up.TaskCatalog(ctx, a)
	if err != nil {
		return taskengine.Observation{}, err
	}
	for _, x := range items {
		if "growth/"+x.TaskCode != t.Key {
			continue
		}
		phase := "manual"
		if x.State() == "claimed" {
			phase = "done"
		} else if x.State() == "completed" {
			phase = "claimable"
		}
		if x.Locked {
			phase = "blocked"
		}
		return taskengine.Observation{Phase: phase, Current: int64(x.Progress.Current), Total: int64(x.Progress.Target)}, nil
	}
	return taskengine.Observation{Phase: "blocked"}, nil
}
func (m *growthModule) Claim(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	current, err := m.Inspect(ctx, a, t)
	if err != nil || current.Phase != "claimable" {
		return taskengine.NotSubmitted(t, taskengine.StageClaim, current), err
	}
	err = m.up.TaskClaim(ctx, a, strings.TrimPrefix(t.Key, "growth/"))
	return taskengine.UnknownAction(t, taskengine.StageClaim, taskengine.Observation{Phase: "pending"}), err
}
func (m *growthModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
