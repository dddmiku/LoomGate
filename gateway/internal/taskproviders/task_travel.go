package taskproviders

// 任务：Buddy 旅行（领养 / 派出 / 领奖）
//
// 地点、业务日与旅行记录全部来自官方响应；每次只处理一个明确动作，再回读
// 结果。协议与首次领养必须由用户在官方成长中心完成，不代填、不伪造对话。

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

func newTravelModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" {
		return nil, nil
	}
	return &travelModule{
		moduleBase: moduleBase{
			spec: ModuleSpec{ID: "travel", Key: "travel/", Prefix: true, Realms: []string{"cn"}},
			definition: taskengine.Definition{
				Key:         "travel/",
				Name:        "Buddy 旅行",
				Description: "地点、业务日与旅行记录来自官方响应；每次只处理一个明确动作，再回读结果。",
				SideEffect:  true,
				Stages:      []taskengine.Stage{taskengine.StageInspect, taskengine.StageExecute, taskengine.StageClaim, taskengine.StageVerify},
			},
		},
		up:    a.Upstream,
		owner: a,
	}, nil
}

var travelPlan = &DefaultPlan{ID: "legacy-travel", TaskKey: "travel/", Hours: []int{15}, LegacyEnabled: boolValue(true)}

type travelModule struct {
	moduleBase
	up    *upstream.Client
	owner *Accounts
}

func (m *travelModule) AcquireTask(ctx context.Context, a *auth.Auth, t taskengine.Task) (func(), error) {
	if m.owner == nil {
		return func() {}, nil
	}
	return m.owner.acquireLegacyAccountPacing(ctx, t.AccountID)
}

type travelSnapshot struct {
	buddy     *upstream.Buddy
	travel    upstream.TravelState
	locations []upstream.GrowthTravelLocation
	day       string
}

func (m *travelModule) read(ctx context.Context, a *auth.Auth) (s travelSnapshot, err error) {
	taskProgress(ctx, "正在读取 Buddy 档案与旅行状态。")
	s.buddy, err = m.up.TaskBuddy(ctx, a)
	if err != nil || s.buddy == nil {
		return
	}
	s.travel, err = m.up.TaskTravel(ctx, a)
	if err != nil {
		return
	}
	hm, e := m.up.TaskHeatmap(ctx, a)
	if e != nil {
		return s, e
	}
	if _, e = businessDate(hm.Today.Date); e != nil {
		return s, e
	}
	s.day = hm.Today.Date
	if s.travel.State == "idle" && !s.travel.DailyLimitReached {
		cfg, e := m.up.TaskTravelConfig(ctx, a)
		if e != nil {
			return s, e
		}
		s.locations = cfg.Locations
	}
	taskProgress(ctx, fmt.Sprintf("官方旅行状态：%s；当前记录 %d，业务日 %s。", s.travel.State, s.travel.RecordID, s.day))
	return
}

func (m *travelModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	s, err := m.read(ctx, a)
	if err != nil {
		return nil, err
	}
	if s.buddy == nil {
		d := m.definition
		d.Key = "travel/adopt"
		d.Name = "首次领养 Buddy"
		d.ManualOnly = true
		return []ModuleTask{{d, observation("manual", "请在官方成长中心完成真实对话门槛、确认协议并领养；不会代填协议或上报假对话。")}}, nil
	}
	out := []ModuleTask{}
	for _, action := range []string{"claim", "depart"} {
		d := m.definition
		d.Key = "travel/" + action
		d.PeriodKey = s.day
		d.OncePerDay = true
		if action == "claim" {
			d.Name = "领取旅行奖励"
			d.PeriodKey += "/" + strconv.FormatInt(s.travel.RecordID, 10)
			d.Reward = fmt.Sprintf("%d 积分（按上游回读核验）", s.travel.RewardCredit)
		} else {
			d.Name = "派出 Buddy 旅行"
		}
		t := taskengine.Task{Definition: d}
		out = append(out, ModuleTask{d, m.state(t, s)})
	}
	return out, nil
}

func (m *travelModule) state(t taskengine.Task, s travelSnapshot) taskengine.Observation {
	if s.buddy == nil {
		return observation("manual", "请先在官方成长中心完成协议与首次领养。")
	}
	if t.Key == "travel/adopt" {
		return observation("done", "官方已存在 Buddy 档案。")
	}
	if t.Key == "travel/depart" {
		if t.PeriodKey != s.day {
			return observation("blocked", "服务端业务日已变化，先核对旧执行结果。")
		}
		if s.travel.DailyLimitReached && (s.travel.State == "traveling" || s.travel.State == "arrived" || s.travel.State == "idle") {
			return observation("done", "服务端确认今日派出名额已使用。")
		}
		if s.travel.State != "idle" {
			return observation("manual", "当前旅行尚未结算，先查看旅行或领取状态。")
		}
		if len(s.locations) == 0 {
			return observation("blocked", "官方没有返回可用旅行地点。")
		}
		return observation("ready", fmt.Sprintf("将派往官方目录首个可用地点：%s（%s）。", s.locations[0].Name, s.locations[0].DurationText()))
	}
	parts := strings.Split(t.PeriodKey, "/")
	if len(parts) != 2 || parts[0] != s.day {
		return observation("blocked", "原旅行记录所属业务日已变化，保留待核验。")
	}
	record, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || record <= 0 {
		return observation("manual", "当前没有可领取的旅行记录。")
	}
	if s.travel.RecordID == record {
		if s.travel.State == "arrived" {
			return observation("claimable", "官方旅行已到站，将领取此条记录后复核。")
		}
		if s.travel.State == "traveling" {
			return observation("manual", "旅行仍在途中；计划将在后续时刻重新读取，不等待数小时。")
		}
	}
	if s.travel.State == "idle" && s.travel.RecordID == 0 && t.AcceptedStage == taskengine.StageClaim {
		return observation("done", "服务端确认原旅行记录已结算并回到空闲。")
	}
	if s.travel.State == "idle" && s.travel.RecordID == 0 {
		return observation("manual", "原记录已不在待领列表，但缺少已接受的领取回执，继续保留待核验。")
	}
	return observation("blocked", "旅行记录已变化，不能用另一条记录确认原操作。")
}

func (m *travelModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	s, e := m.read(ctx, a)
	if e != nil {
		return taskengine.Observation{}, e
	}
	return m.state(t, s), nil
}

func (m *travelModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	s, e := m.read(ctx, a)
	if e != nil {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{}), e
	}
	state := m.state(t, s)
	if t.Key != "travel/depart" || state.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, state), nil
	}
	taskProgress(ctx, fmt.Sprintf("正在派往官方动态目录地点：%s。", s.locations[0].Name))
	e = m.up.TaskTravelDepart(ctx, a, s.locations[0].ID)
	if e != nil {
		taskProgress(ctx, "派出结果未确认；等待读取原记录状态，不更换记录重试。")
	}
	return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "派出已提交，等待状态复核。")), e
}

func (m *travelModule) Claim(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	s, e := m.read(ctx, a)
	if e != nil {
		return taskengine.NotSubmitted(t, taskengine.StageClaim, taskengine.Observation{}), e
	}
	state := m.state(t, s)
	if t.Key != "travel/claim" || state.Phase != "claimable" {
		return taskengine.NotSubmitted(t, taskengine.StageClaim, state), nil
	}
	taskProgress(ctx, "正在领取原旅行记录奖励。")
	reward, err := m.up.TaskTravelClaim(ctx, a, s.travel.RecordID)
	result := observation("pending", "领取已提交，回执尚未确认；保留原记录待核验。")
	if err == nil {
		result.Summary = fmt.Sprintf("官方回执奖励：+%d 积分，等待原旅行记录结算复核。", reward)
		taskProgress(ctx, result.Summary)
		return taskengine.Accepted(t, taskengine.StageClaim, result), nil
	}
	taskProgress(ctx, "旅行奖励回执未确认，保留原记录待核验。")
	return taskengine.UnknownAction(t, taskengine.StageClaim, result), err
}

func (m *travelModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
