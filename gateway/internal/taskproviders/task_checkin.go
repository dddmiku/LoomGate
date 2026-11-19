package taskproviders

// 任务：每日签到（WorkBuddy 国内）
//
// 这个文件拥有该任务的全部：定义、实现、默认定时。删任务=删本文件+删注册表一行。

import (
	"context"
	"errors"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

func newCheckinModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" {
		return nil, nil
	}
	return &checkinModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "checkin", Key: "checkin", Realms: []string{"cn"}},
			definition: taskengine.Definition{
				Key:         "checkin",
				Name:        "每日签到",
				Description: "检查官方签到状态，执行后再次读取服务端状态。",
				OncePerDay:  true,
				SideEffect:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify},
			},
		},
		up: a.Upstream,
	}, nil
}

var checkinPlan = &DefaultPlan{ID: "legacy-checkin", TaskKey: "checkin", Hours: []int{9, 21}, LegacyEnabled: boolValue(true)}

type checkinModule struct {
	moduleBase
	up *upstream.Client
}

func (m *checkinModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	if a.Realm() != "cn" || m.up == nil {
		return taskengine.Observation{}, errors.New("checkin_unavailable")
	}
	phase := "blocked"
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
	return taskengine.Observation{Phase: phase}, nil
}

func (m *checkinModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	// Recheck under the engine account lock, including externally completed signins.
	s, err := m.Inspect(ctx, a, t)
	if err != nil || s.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, s), err
	}
	err = m.up.TaskCheckin(ctx, a)
	if upstream.IsAlreadyCheckin(err) {
		err = nil
	}
	return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending"}), err
}

func (m *checkinModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
