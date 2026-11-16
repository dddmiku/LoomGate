package taskproviders

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

func observation(phase, summary string) taskengine.Observation {
	return taskengine.Observation{Phase: phase, Summary: summary}
}
func businessDate(v string) (time.Time, error) {
	if len(v) != 10 {
		return time.Time{}, errors.New("business_date_invalid")
	}
	d, e := time.Parse("2006-01-02", v)
	if e != nil {
		return d, errors.New("business_date_invalid")
	}
	return d, nil
}
func lifecycleBase(def taskengine.Definition, id, key string, prefix bool) moduleBase {
	return moduleBase{spec: ModuleSpec{ID: id, Key: key, Prefix: prefix, Realms: []string{"cn"}}, definition: def}
}
func newLifecycleModules(up *upstream.Client, defs map[string]taskengine.Definition) []Module {
	return []Module{
		&travelModule{moduleBase: lifecycleBase(defs["travel"], "travel", "travel/", true), up: up},
		&redemptionModule{moduleBase: lifecycleBase(defs["redemption"], "redemption", "redeem/", true), up: up},
		&makeupModule{moduleBase: lifecycleBase(defs["makeup"], "makeup", "makeup", false), up: up},
		&resourcesModule{moduleBase: lifecycleBase(defs["resources"], "resources", "resources", false), up: up},
		&compensationModule{moduleBase: lifecycleBase(defs["compensation"], "compensation", "compensation", false), up: up},
	}
}

type travelModule struct {
	moduleBase
	up *upstream.Client
}
type travelSnapshot struct {
	buddy     *upstream.Buddy
	travel    upstream.TravelState
	locations []upstream.GrowthTravelLocation
	day       string
}

func (m *travelModule) read(ctx context.Context, a *auth.Auth) (s travelSnapshot, err error) {
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
		return taskengine.Observation{}, e
	}
	state := m.state(t, s)
	if t.Key != "travel/depart" || state.Phase != "ready" {
		return state, nil
	}
	e = m.up.TaskTravelDepart(ctx, a, s.locations[0].ID)
	return observation("pending", "派出已提交，等待状态复核。"), e
}
func (m *travelModule) Claim(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	s, e := m.read(ctx, a)
	if e != nil {
		return taskengine.Observation{}, e
	}
	state := m.state(t, s)
	if t.Key != "travel/claim" || state.Phase != "claimable" {
		return state, nil
	}
	e = m.up.TaskTravelClaim(ctx, a, s.travel.RecordID)
	return observation("pending", "领取已提交，等待原记录结算。"), e
}
func (m *travelModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}

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
		return state, e
	}
	// Stable for this account, upstream month and tier; never regenerated on retry.
	operation := fmt.Sprintf("loomgate-%x", sha256.Sum256([]byte(a.UID+"\x00"+t.Key+"\x00"+t.PeriodKey)))
	e = m.up.TaskRedeem(ctx, a, strings.TrimPrefix(t.Key, "redeem/"), operation)
	return observation("pending", "兑换已提交，等待官方档位状态确认。"), e
}
func (m *redemptionModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}

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
		return state, e
	}
	e = m.up.TaskMakeup(ctx, a, t.PeriodKey)
	return observation("pending", "补登已提交，等待目标日期回读。"), e
}
func (m *makeupModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}

type resourcesModule struct {
	moduleBase
	up *upstream.Client
}

func (m *resourcesModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	r, e := m.up.TaskResources(ctx, a)
	if e != nil {
		return taskengine.Observation{}, e
	}
	st, e := m.up.TaskStreak(ctx, a)
	if e != nil {
		return taskengine.Observation{}, e
	}
	return observation("done", fmt.Sprintf("能量 %d；抽奖机会 %d；可开 Buddy %d；单次上限 %d；连登 %d 天。只读查询，未消耗。", r.Energy, r.Chances, r.Quota.Affordable, r.Quota.MaxOpenCount, st.Streak.Days)), nil
}
func (m *resourcesModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	d := m.definition
	s, e := m.Inspect(ctx, a, taskengine.Task{Definition: d})
	return []ModuleTask{{d, s}}, e
}
func (m *resourcesModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}

type compensationModule struct {
	moduleBase
	up *upstream.Client
}

func compensationPeriod(v upstream.TaskCompensation) string {
	return fmt.Sprintf("offer-%x", sha256.Sum256([]byte(v.End+"\x00"+strconv.FormatInt(v.Credit, 10))))
}
func (m *compensationModule) state(t taskengine.Task, v upstream.TaskCompensation) taskengine.Observation {
	if t.PeriodKey != compensationPeriod(v) {
		return observation("blocked", "补偿活动已变化，不能用新活动确认旧领取。")
	}
	if v.Claimed {
		return observation("done", "官方确认此项补偿已领取。")
	}
	if !v.Active {
		return observation("manual", "当前无进行中的补偿活动。")
	}
	return observation("claimable", "官方确认活动进行中且尚未领取；提交后再次查询领取状态。")
}
func (m *compensationModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	v, e := m.up.TaskCompensation(ctx, a)
	if e != nil {
		return nil, e
	}
	d := m.definition
	d.PeriodKey = compensationPeriod(v)
	d.Reward = fmt.Sprintf("%d 积分", v.Credit)
	return []ModuleTask{{d, m.state(taskengine.Task{Definition: d}, v)}}, nil
}
func (m *compensationModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	v, e := m.up.TaskCompensation(ctx, a)
	if e != nil {
		return taskengine.Observation{}, e
	}
	return m.state(t, v), nil
}
func (m *compensationModule) Claim(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	state, e := m.Inspect(ctx, a, t)
	if e != nil || state.Phase != "claimable" {
		return state, e
	}
	e = m.up.TaskCompensationClaim(ctx, a)
	return observation("pending", "补偿领取已提交，等待 claimed 回读。"), e
}
func (m *compensationModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
