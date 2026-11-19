package taskproviders

// 任务：Trae 每日签到
//
// Trae 是独立站点（api.trae.cn）：业务日按 CST 判定，9074 是设备维度时段性
// 限流，退避持久化在 task-provider-state 里，跨重启生效。余额查询在另一个
// 任务文件里，限流期间仍可独立执行。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream/trae"
)

// TraeTaskAPI is the narrow Trae surface the checkin/balance task files need.
type TraeTaskAPI interface {
	Status(context.Context, *auth.Auth) (trae.CheckinStatus, error)
	Claim(context.Context, *auth.Auth) (trae.CheckinStatus, error)
	Credits(context.Context, *auth.Auth) (trae.Credits, error)
}

func newTraeCheckinModule(a *Accounts) (Module, error) {
	if a.Provider != "trae" {
		return nil, nil
	}
	store := a.options.TraeStore
	if store == nil {
		var err error
		store, err = NewTraeTaskStore(nil)
		if err != nil {
			return nil, err
		}
	}
	client := a.options.TraeClient
	if client == nil {
		client = &trae.TaskClient{}
	}
	now := a.options.Now
	if now == nil {
		now = time.Now
	}
	return &traeCheckinModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "checkin", Key: "checkin", Realms: []string{"trae"}},
			definition: taskengine.Definition{
				Key:         "checkin",
				Name:        "每日签到",
				Description: "检查官方签到状态，执行后再次读取服务端状态。",
				OncePerDay:  true,
				SideEffect:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify},
			},
		},
		api: client, store: store, now: now, owner: a,
	}, nil
}

// 00:05 CST：必须在日切之后，重试循环才不会先跨日把账号签掉。
var traeCheckinPlan = &DefaultPlan{ID: "legacy-trae-check", TaskKey: "checkin", Hours: []int{0}, Minute: 5, LegacyEnabled: boolValue(true)}

type traeCheckinModule struct {
	moduleBase
	api   TraeTaskAPI
	store *TraeTaskStore
	now   func() time.Time
	owner *Accounts
}

func (m *traeCheckinModule) AcquireTask(ctx context.Context, a *auth.Auth, t taskengine.Task) (func(), error) {
	// FIFO channel admission bounds concurrent claim preparation to one. No
	// engine worker or write marker is held while waiting for local pacing.
	select {
	case m.store.claimGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() { <-m.store.claimGate }
	stored := m.store.read(a.UID)
	if stored.CheckedDay == t.PeriodKey || t.PeriodKey != m.day() || m.now().Before(stored.RetryAt) {
		return release, nil
	}
	m.store.mu.Lock()
	delay := m.store.state.NextClaim.Sub(m.now())
	m.store.mu.Unlock()
	if delay > 3*time.Second {
		release()
		return nil, errors.New("trae_task_clock_invalid")
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			release()
			return nil, ctx.Err()
		}
	}
	return release, nil
}

func (m *traeCheckinModule) day() string {
	return m.now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
}

func (m *traeCheckinModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	d := m.definition
	d.PeriodKey = m.day()
	o, e := m.Inspect(ctx, a, taskengine.Task{Definition: d})
	return []ModuleTask{{d, o}}, e
}

func (m *traeCheckinModule) backoff(a *auth.Auth, code int) error {
	return m.store.change(a.UID, func(s *traeTaskState, _ *traeTaskStateData) error {
		s.Failures = min(s.Failures+1, 1000)
		s.Code = code
		s.RetryAt = m.now().Add(time.Minute << min(s.Failures-1, 3))
		return nil
	})
}

func (m *traeCheckinModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	if t.PeriodKey != m.day() {
		return observation("blocked", "签到业务日已变化，请保留旧操作并核验。"), nil
	}
	stored := m.store.read(a.UID)
	if stored.CheckedDay == t.PeriodKey {
		return observation("done", "已通过官方状态确认此业务日签到。"), nil
	}
	if m.now().Before(stored.RetryAt) {
		return observation("manual", fmt.Sprintf("官方限流退避中，%s 后可再次核验；余额查询仍可独立执行。", stored.RetryAt.Format(time.RFC3339))), nil
	}
	state, err := m.api.Status(ctx, a)
	if err != nil {
		if trae.IsRateLimited(err) {
			if err = m.backoff(a, 9074); err != nil {
				return taskengine.Observation{}, err
			}
			return observation("manual", "官方返回 9074，已持久化退避。"), nil
		}
		return taskengine.Observation{}, err
	}
	if state.CheckedIn {
		if err = m.store.change(a.UID, func(s *traeTaskState, _ *traeTaskStateData) error {
			s.CheckedDay = t.PeriodKey
			s.RetryAt = time.Time{}
			s.Failures = 0
			s.Code = 0
			return nil
		}); err != nil {
			return taskengine.Observation{}, err
		}
		return observation("done", "官方状态确认本账号今日已签到。"), nil
	}
	if !state.Enabled {
		return observation("manual", "官方当前未开放签到。"), nil
	}
	return observation("ready", "可提交一次签到，并再次读取官方账号状态。"), nil
}

var errTraeClaimGap = errors.New("trae_task_claim_cooldown")

func (m *traeCheckinModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	o, err := m.Inspect(ctx, a, t)
	if err != nil || o.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, o), err
	}
	if ctx.Err() != nil {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, o), ctx.Err()
	}
	err = m.store.change(a.UID, func(_ *traeTaskState, state *traeTaskStateData) error {
		if m.now().Before(state.NextClaim) {
			return errTraeClaimGap
		}
		state.NextClaim = m.now().Add(3 * time.Second)
		return nil
	})
	if errors.Is(err, errTraeClaimGap) {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, observation("manual", "签到提交间隔尚未结束，请稍后核验。")), nil
	}
	if err != nil {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, o), err
	}
	current, err := m.owner.account(a.UID)
	if err != nil || current.AccessToken != a.AccessToken || current.RefreshToken != a.RefreshToken || t.PeriodKey != m.day() {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, observation("blocked", "账号或业务日发生变化，请重新核验。")), err
	}
	_, err = m.api.Claim(ctx, a)
	if err != nil {
		// Only typed business refusals permit releasing this attempt marker.
		// Network failures, malformed envelopes and other rejections stay unknown.
		if trae.IsRateLimited(err) {
			if saveErr := m.backoff(a, 9074); saveErr != nil {
				return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "退避状态保存失败，保留待复核。")), saveErr
			}
			return taskengine.Rejected(t, taskengine.StageExecute, 9074, observation("manual", "官方明确拒绝本次签到（9074），已保存退避；到期后先查状态。")), nil
		}
		if trae.IsDeviceAlreadyChecked(err) {
			saveErr := m.store.change(a.UID, func(s *traeTaskState, _ *traeTaskStateData) error {
				day, _ := time.ParseInLocation("2006-01-02", m.day(), time.FixedZone("CST", 8*3600))
				s.RetryAt = day.AddDate(0, 0, 1)
				s.Code = 9095
				return nil
			})
			if saveErr != nil {
				return taskengine.UnknownAction(t, taskengine.StageExecute, o), saveErr
			}
			return taskengine.Rejected(t, taskengine.StageExecute, 9095, observation("manual", "官方提示此设备已签到；不能视为本账号已完成，请在官方客户端核验。")), nil
		}
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "提交未确认，请等待官方状态核验。")), err
	}
	return taskengine.Accepted(t, taskengine.StageExecute, observation("pending", "官方接受签到，仍需账号状态复核。")), nil
}

func (m *traeCheckinModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
