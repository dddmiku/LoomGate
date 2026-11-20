package taskproviders

import (
	"context"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
)

// checkinRefreshModule performs a read-only credential preflight and delegates
// only an expiring credential to keepalive's durable one-shot CAS refresh path.
type checkinRefreshModule struct {
	moduleBase
	keepalive *keepaliveModule
}

func newCheckinRefreshModule(a *Accounts) (Module, error) {
	inner, err := newKeepaliveModule(a)
	if err != nil {
		return nil, err
	}
	var keepalive *keepaliveModule
	if inner != nil {
		keepalive, _ = inner.(*keepaliveModule)
	}
	return &checkinRefreshModule{
		moduleBase: moduleBase{
			spec:       ModuleSpec{ID: "checkin_refresh", Key: "credential-preflight", InternalOnly: true, Realms: []string{"cn", "global"}, RecoverSession: true},
			definition: taskengine.Definition{Key: "credential-preflight", Name: "签到前凭据预检", Description: "核验凭据文件；仅在进入 10 分钟安全窗口时才尝试持久化续期。", SideEffect: true, Stages: []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify}},
		},
		keepalive: keepalive,
	}, nil
}

var checkinRefreshPlan *DefaultPlan // deliberately no standalone schedule

func (m *checkinRefreshModule) Discover(ctx context.Context, a *auth.Auth) ([]taskengine.Definition, error) {
	d := m.definition
	d.PeriodKey = a.CredentialRevision()
	return []taskengine.Definition{d}, ctx.Err()
}

func (m *checkinRefreshModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	if err := ctx.Err(); err != nil {
		return taskengine.Observation{}, err
	}
	if t.PendingVerification {
		if m.keepalive == nil {
			return observation("pending", "上次预刷新结果未确认；缺少安全存储，保持核验状态。"), nil
		}
		return m.keepalive.inspect(ctx, a, t, true)
	}
	if a.FilePath == "" {
		return observation("blocked", "凭据文件或访问会话缺失，未执行签到。"), nil
	}
	persisted, err := a.PersistedRevision()
	if err != nil {
		return observation("blocked", "安全凭据文件不可读取，未执行签到。"), nil
	}
	if persisted != a.CredentialRevision() || t.PeriodKey != a.CredentialRevision() {
		return observation("blocked", "账号凭据文件与当前账号状态不一致，未执行签到。"), nil
	}
	if m.keepalive != nil {
		record, _, recordErr := m.keepalive.load(a)
		if recordErr != nil {
			return observation("blocked", "历史凭据续期状态不可读取，未执行签到。"), nil
		}
		if record.Before != "" && !record.Verified && record.State != "rejected" {
			return observation("blocked", "历史凭据续期结果尚未核验，未执行签到。"), nil
		}
	}
	if !a.NeedsRefresh(10 * time.Minute) {
		return observation("done", "官方凭据仍在 10 分钟安全窗口外；只读预检通过，未刷新。"), nil
	}
	if strings.TrimSpace(a.RefreshToken) == "" || m.keepalive == nil {
		return observation("blocked", "凭据进入 10 分钟安全窗口，但没有可用的安全续期存储；未执行签到。"), nil
	}
	state, err := m.keepalive.inspect(ctx, a, t, false)
	if err != nil || state.Phase != "ready" {
		return state, err
	}
	return observation("ready", "凭据进入 10 分钟安全窗口，将按持久化尝试流程续期并复核。"), nil
}

func (m *checkinRefreshModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	state, err := m.Inspect(ctx, a, t)
	if err != nil || state.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, state), err
	}
	return m.keepalive.Execute(ctx, a, t)
}

func (m *checkinRefreshModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	if m.keepalive == nil {
		return observation("blocked", "安全续期存储不可用。"), nil
	}
	return m.keepalive.Verify(ctx, a, t)
}

var _ Module = (*checkinRefreshModule)(nil)
