package taskproviders

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream/trae"
)

type TraeTaskAPI interface {
	Status(context.Context, *auth.Auth) (trae.CheckinStatus, error)
	Claim(context.Context, *auth.Auth) (trae.CheckinStatus, error)
	Credits(context.Context, *auth.Auth) (trae.Credits, error)
}
type AccountsOptions struct {
	TraeStore  *TraeTaskStore
	TraeClient TraeTaskAPI
	Now        func() time.Time
}

func newTraeModules(a *Accounts, defs map[string]taskengine.Definition) ([]Module, error) {
	s := a.options.TraeStore
	if s == nil {
		var err error
		s, err = NewTraeTaskStore(nil)
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
	check := &traeCheckinModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "checkin", Key: "checkin", Realms: []string{"trae"}}, definition: defs["checkin"]}, api: client, store: s, now: now}
	balance := &traeBalanceModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "balance", Key: "balance", Realms: []string{"trae"}}, definition: defs["balance"]}, owner: a, api: client, now: now}
	return []Module{check, balance}, nil
}

type traeBalanceModule struct {
	moduleBase
	owner *Accounts
	api   TraeTaskAPI
	now   func() time.Time
}

func (m *traeBalanceModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	credits, err := m.api.Credits(ctx, a)
	if err != nil {
		return taskengine.Observation{}, err
	}
	if math.IsNaN(credits.Remaining) || math.IsInf(credits.Remaining, 0) || credits.Remaining < 0 || credits.Remaining >= float64(math.MaxInt64) {
		return taskengine.Observation{}, errors.New("trae_credit_range_invalid")
	}
	if ctx.Err() != nil {
		return taskengine.Observation{}, ctx.Err()
	}
	if _, err = m.owner.account(a.UID); err != nil {
		return taskengine.Observation{}, err
	}
	remain := int64(credits.Remaining)
	expiring := trae.ExpiringWithin(credits, m.owner.ExpiringSoon, m.now())
	m.owner.Pool.SetCreditsDetailed(a.UID, remain, expiring)
	m.owner.Pool.ReenableIfCredits(a.UID, remain)
	return taskengine.Observation{Phase: "done", Current: remain, Summary: fmt.Sprintf("剩余 %d 积分；配置窗口内将到期 %d 积分。已更新账号池。", remain, expiring)}, nil
}
func (m *traeBalanceModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}

type traeCheckinModule struct {
	moduleBase
	api   TraeTaskAPI
	store *TraeTaskStore
	now   func() time.Time
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
