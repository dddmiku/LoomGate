package taskproviders

// 任务：连登奖励兑换
//
// 档位、状态与月份全部来自官方响应；只有明确 available 且连登天数达标的档位
// 才提交。操作标识对「账号+档位+月份」稳定，重试不会生成新请求。

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

func newRedemptionModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" {
		return nil, nil
	}
	return &redemptionModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "redemption", Key: "redeem/", Prefix: true, Realms: []string{"cn"}},
			definition: taskengine.Definition{
				Key:         "redeem/",
				Name:        "连登奖励兑换",
				Description: "动态读取官方档位和月份，仅领取明确可用的档位，未知状态不写入。",
				SideEffect:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect, taskengine.StageClaim, taskengine.StageVerify},
			},
		},
		up: a.Upstream,
	}, nil
}

var redemptionPlan = &DefaultPlan{ID: "legacy-redeem", TaskKey: "redeem/", Hours: []int{12}, Minute: 10, LegacyEnabled: boolValue(true)}

type redemptionModule struct {
	moduleBase
	up *upstream.Client
}

type redemptionSnapshot struct {
	month  string
	tiers  []upstream.GrowthTierSpec
	states map[string]string
	days   int
}

func (m *redemptionModule) read(ctx context.Context, a *auth.Auth) (s redemptionSnapshot, err error) {
	st, err := m.up.TaskStreak(ctx, a)
	if err != nil {
		return s, err
	}
	hm, err := m.up.TaskHeatmap(ctx, a)
	if err != nil {
		return s, err
	}
	if _, err = businessDate(hm.Today.Date); err != nil {
		return s, err
	}
	s.month = hm.Today.Date[:7]
	s.days = st.Streak.Days
	s.states = map[string]string{}
	if json.Unmarshal(st.Redemption["tiers"], &s.tiers) != nil || s.tiers == nil || len(s.tiers) > 64 {
		return s, errors.New("redemption_directory_missing")
	}
	for _, tier := range s.tiers {
		if !taskengine.IDPattern.MatchString(tier.Tier) || tier.Days <= 0 || tier.Credit < 0 || tier.Energy < 0 || tier.Cards < 0 || tier.Chances < 0 {
			return s, errors.New("redemption_directory_invalid")
		}
		if _, duplicate := s.states[tier.Tier]; duplicate {
			return s, errors.New("redemption_directory_duplicate")
		}
		var state string
		if json.Unmarshal(st.Redemption["tier_"+tier.Tier+"_status"], &state) != nil || state != "available" && state != "claimed" && state != "locked" {
			return s, errors.New("redemption_state_unknown")
		}
		s.states[tier.Tier] = state
	}
	return s, nil
}

func (m *redemptionModule) state(t taskengine.Task, s redemptionSnapshot) taskengine.Observation {
	if t.PeriodKey != s.month {
		return observation("blocked", "官方月份已变化，不能用新月状态确认旧领取。")
	}
	key := strings.TrimPrefix(t.Key, "redeem/")
	state, ok := s.states[key]
	if !ok {
		return observation("blocked", "此兑换档位已不在官方目录。")
	}
	switch state {
	case "claimed":
		return observation("done", "官方确认本月此档位已领取。")
	case "available":
		for _, tier := range s.tiers {
			if tier.Tier == key {
				if s.days < tier.Days {
					return observation("blocked", "官方档位状态与连登进度不一致。")
				}
				return taskengine.Observation{Phase: "claimable", Current: int64(s.days), Total: int64(tier.Days), Summary: "官方档位已解锁，可领取后复核。"}
			}
		}
	}
	return observation("manual", "连登天数未达标，请完成官方实际活动。")
}

func (m *redemptionModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	s, e := m.read(ctx, a)
	if e != nil {
		return nil, e
	}
	out := []ModuleTask{}
	for _, tier := range s.tiers {
		d := m.definition
		d.Key = "redeem/" + tier.Tier
		d.PeriodKey = s.month
		d.Name = fmt.Sprintf("连登 %d 天奖励", tier.Days)
		d.Reward = fmt.Sprintf("%d 积分 · %d 能量 · %d 补登卡 · %d 次机会", tier.Credit, tier.Energy, tier.Cards, tier.Chances)
		out = append(out, ModuleTask{d, m.state(taskengine.Task{Definition: d}, s)})
	}
	return out, nil
}

func (m *redemptionModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	s, e := m.read(ctx, a)
	if e != nil {
		return taskengine.Observation{}, e
	}
	return m.state(t, s), nil
}

func (m *redemptionModule) Claim(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	state, e := m.Inspect(ctx, a, t)
	if e != nil || state.Phase != "claimable" {
		return taskengine.NotSubmitted(t, taskengine.StageClaim, state), e
	}
	// Stable for this account, upstream month and tier; never regenerated on retry.
	operation := fmt.Sprintf("loomgate-%x", sha256.Sum256([]byte(a.UID+"\x00"+t.Key+"\x00"+t.PeriodKey)))
	e = m.up.TaskRedeem(ctx, a, strings.TrimPrefix(t.Key, "redeem/"), operation)
	return taskengine.UnknownAction(t, taskengine.StageClaim, observation("pending", "兑换已提交，等待官方档位状态确认。")), e
}

func (m *redemptionModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
