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
	taskProgress(ctx, "正在读取官方签到状态。")
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
	if phase == "done" {
		taskProgress(ctx, "官方确认今日已签到。")
	} else if phase == "ready" {
		taskProgress(ctx, "官方签到可用，未重复提交。")
	} else {
		taskProgress(ctx, "官方签到当前不可用，未提交。")
	}
	return taskengine.Observation{Phase: phase, Summary: map[string]string{"done": "官方确认今日已签到。", "ready": "官方签到可用。", "blocked": "官方签到当前不可用。"}[phase]}, nil
}

func (m *checkinModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	// Recheck under the engine account lock, including externally completed signins.
	s, err := m.Inspect(ctx, a, t)
	if err != nil || s.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, s), err
	}
	taskProgress(ctx, "正在提交一次每日签到请求；最终以官方状态复核为准。")
	err = m.up.TaskCheckin(ctx, a)
	if upstream.IsAlreadyCheckin(err) {
		taskProgress(ctx, "官方提示今日已签到，开始只读状态复核。")
		err = nil
	}
	if err != nil {
		taskProgress(ctx, "签到请求未确认，保留待核验；不会自动重试。")
	} else {
		taskProgress(ctx, "签到请求已返回，等待官方签到状态复核。")
	}
	return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "签到请求已发送，等待官方状态复核。"}), err
}

func (m *checkinModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
