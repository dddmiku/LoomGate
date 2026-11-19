package taskproviders

// 任务：日历补登
//
// 目标日期从官方日历推导（服务端当天、上线日、已补记录），一轮最多消耗一张
// 补登卡。没有卡时不消耗其他权益。

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

func newMakeupModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" {
		return nil, nil
	}
	return &makeupModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "makeup", Key: "makeup", Realms: []string{"cn"}},
			definition: taskengine.Definition{
				Key:         "makeup",
				Name:        "日历补登",
				Description: "根据服务端当天、上线日和日历选择最近断点，一轮最多使用一张补登卡。",
				SideEffect:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageVerify},
			},
		},
		up: a.Upstream,
	}, nil
}

var makeupPlan = &DefaultPlan{ID: "legacy-makeup", TaskKey: "makeup", Hours: []int{12}, Minute: 5, LegacyEnabled: boolValue(true)}

type makeupModule struct {
	moduleBase
	up *upstream.Client
}

type makeupSnapshot struct {
	day      string
	cards    int
	eligible []string
	made     map[string]bool
	active   map[string]bool
}

func (m *makeupModule) read(ctx context.Context, a *auth.Auth) (s makeupSnapshot, err error) {
	st, e := m.up.TaskStreak(ctx, a)
	if e != nil {
		return s, e
	}
	hm, e := m.up.TaskHeatmap(ctx, a)
	if e != nil {
		return s, e
	}
	today, e := businessDate(hm.Today.Date)
	if e != nil {
		return s, e
	}
	launch, e := businessDate(st.LaunchDate)
	if e != nil {
		return s, e
	}
	var cards *int
	if len(st.MakeupCards) > 0 && st.MakeupCards[0] == '{' {
		var v struct {
			Balance *int `json:"balance"`
		}
		e = json.Unmarshal(st.MakeupCards, &v)
		cards = v.Balance
	} else {
		e = json.Unmarshal(st.MakeupCards, &cards)
	}
	if e != nil || cards == nil || *cards < 0 || hm.Cells == nil || st.Streak.MakeupDates == nil {
		return s, errors.New("makeup_state_missing")
	}
	s.day = hm.Today.Date
	s.cards = *cards
	s.made = map[string]bool{}
	s.active = map[string]bool{}
	for _, date := range st.Streak.MakeupDates {
		if _, e := businessDate(date); e != nil {
			return s, e
		}
		s.made[date] = true
	}
	seen := map[string]float64{}
	for _, cell := range hm.Cells {
		date, e := businessDate(cell.Date)
		if e != nil || cell.Score < 0 {
			return s, errors.New("makeup_calendar_invalid")
		}
		if old, ok := seen[cell.Date]; ok {
			if old != cell.Score {
				return s, errors.New("makeup_calendar_conflict")
			}
			continue
		}
		seen[cell.Date] = cell.Score
		s.active[cell.Date] = cell.Score > 0
		if cell.Score == 0 && !s.made[cell.Date] && date.Before(today) && !date.Before(launch) && cell.Date[:7] == s.day[:7] {
			s.eligible = append(s.eligible, cell.Date)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(s.eligible)))
	return s, nil
}

func (m *makeupModule) state(t taskengine.Task, s makeupSnapshot) taskengine.Observation {
	if s.made[t.PeriodKey] || s.active[t.PeriodKey] {
		return observation("done", "官方日历确认目标日期已活跃或已补登。")
	}
	for _, date := range s.eligible {
		if date == t.PeriodKey {
			if s.cards > 0 {
				return observation("ready", "将消耗 1 张补登卡补此日期；每次运行最多使用一张。")
			}
			return observation("manual", "当前没有补登卡，不会消耗其他权益。")
		}
	}
	return observation("manual", "目标日期不在官方可补范围，或本月无需补登。")
}

func (m *makeupModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	s, e := m.read(ctx, a)
	if e != nil {
		return nil, e
	}
	d := m.definition
	d.Key = "makeup"
	d.PeriodKey = "none:" + s.day
	if len(s.eligible) > 0 {
		d.PeriodKey = s.eligible[0]
		d.Name = "补登 " + d.PeriodKey
	}
	return []ModuleTask{{d, m.state(taskengine.Task{Definition: d}, s)}}, nil
}

func (m *makeupModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	s, e := m.read(ctx, a)
	if e != nil {
		return taskengine.Observation{}, e
	}
	return m.state(t, s), nil
}

func (m *makeupModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	state, e := m.Inspect(ctx, a, t)
	if e != nil || state.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, state), e
	}
	e = m.up.TaskMakeup(ctx, a, t.PeriodKey)
	return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "补登已提交，等待目标日期回读。")), e
}

func (m *makeupModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
