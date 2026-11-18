package main

import (
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/config"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

type scheduleMigrationTransport struct{ calls atomic.Int32 }

func (tr *scheduleMigrationTransport) RoundTrip(*http.Request) (*http.Response, error) {
	tr.calls.Add(1)
	return nil, errors.New("synthetic_upstream_must_not_be_called")
}

func scheduleMigrationConfig() config.Schedule {
	s := config.DefaultSchedule()
	s.CheckinEnabled = false
	s.TraeCheckEnabled = false
	s.TravelEnabled = false
	s.ActivityEnabled = false
	s.KeepaliveEnabled = false
	s.SchoolEnabled = false
	s.CatEnabled = false
	s.GrowthEnabled = false
	return s
}

func openScheduleMigrationRuntime(t *testing.T, stateDir string, schedule config.Schedule) (*workbenchRuntime, *scheduleMigrationTransport) {
	t.Helper()
	for _, key := range []string{"LOOMGATE_DOUBAO_DESKTOP_PYTHON", "LOOMGATE_DOUBAO_DESKTOP_HELPER", "LOOMGATE_DOUBAO_DESKTOP_PORT", "WB2API_RUNTIME_DIR"} {
		t.Setenv(key, "")
	}
	p := pool.New("")
	t.Cleanup(func() { p.Close() })
	transport := &scheduleMigrationTransport{}
	up := upstream.New()
	up.HTTP.Transport = transport
	up.ChatHTTP.Transport = transport
	w, _, err := openWorkbenchWithLegacySchedules(stateDir, p, up, schedule)
	if err != nil {
		t.Fatalf("open synthetic workbench: %v", err)
	}
	t.Cleanup(func() { closeScheduleMigrationRuntime(w) })
	return w, transport
}

func closeScheduleMigrationRuntime(w *workbenchRuntime) {
	_ = w.engine.Close()
	_ = w.vault.Close()
	_ = w.taskVault.Close()
}

func TestScheduleDefaultsCoverSupportedLegacyModulesWithoutDispatch(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	source := config.DefaultSchedule()
	source.GrowthHours = []int{11, 20}
	source.TravelEnabled = false
	w, transport := openScheduleMigrationRuntime(t, stateDir, source)
	plans := w.engine.Schedules()
	byID := map[string]taskengine.Schedule{}
	for _, plan := range plans {
		byID[plan.ID] = plan
		if plan.TaskKey == "*" || plan.TaskKey == "" {
			t.Fatalf("default schedule broadened its task scope: %+v", plan)
		}
	}
	for _, tc := range []struct {
		id, provider, key string
		hours             []int
		minute            int
		enabled           bool
	}{
		{"legacy-checkin", "workbuddy", "checkin", []int{9, 21}, 0, false},
		{"legacy-trae-check", "trae", "checkin", []int{0}, 5, false},
		{"legacy-balance", "workbuddy", "balance", []int{14}, 0, true},
		{"legacy-travel", "workbuddy", "travel/", []int{15}, 0, false},
		{"legacy-keepalive", "workbuddy", "keepalive", []int{22}, 0, false},
		{"legacy-trae-keepalive", "trae", "keepalive", []int{22}, 0, false},
		{"legacy-growth", "workbuddy", "growth/", []int{11, 20}, 0, false},
		{"legacy-makeup", "workbuddy", "makeup", []int{11, 20}, 5, false},
		{"legacy-redeem", "workbuddy", "redeem/", []int{11, 20}, 10, false},
		{"legacy-lottery", "workbuddy", "lottery", []int{11, 20}, 15, false},
		{"legacy-buddy-box", "workbuddy", "buddy_box", []int{11, 20}, 20, false},
		{"legacy-resources", "workbuddy", "resources", []int{11, 20}, 25, false},
		{"legacy-compensation", "workbuddy", "compensation", []int{11, 20}, 30, false},
	} {
		p, ok := byID[tc.id]
		if !ok || p.ProviderID != tc.provider || p.TaskKey != tc.key || !reflect.DeepEqual(p.Hours, tc.hours) || p.Minute != tc.minute || p.Enabled != tc.enabled {
			t.Errorf("default %s missing or changed source intent: %+v", tc.id, p)
		}
		if !ok {
			continue
		}
		if p.Timezone != "Asia/Shanghai" || len(p.AccountIDs) != 0 || p.Enabled != (p.NextRun != nil) {
			t.Errorf("default %s lost source scope/switch: %+v", tc.id, p)
		}
		if p.ID == "legacy-compensation" {
			if p.LegacyEnabled != nil {
				t.Errorf("compensation plan invented a legacy source switch: %+v", p)
			}
			continue
		}
		// travel was disabled in the source config; every other legacy plan was
		// enabled there. The plan switch itself is always off for writes.
		wantLegacy := p.ID != "legacy-travel"
		if p.LegacyEnabled == nil || *p.LegacyEnabled != wantLegacy {
			t.Errorf("default %s lost the recorded legacy switch: %+v", tc.id, p)
		}
	}
	for _, id := range []string{"legacy-school", "legacy-cat", "legacy-activity"} {
		if _, ok := byID[id]; ok {
			t.Errorf("unsupported event plan restored: %s", id)
		}
	}
	if err := w.engine.Tick(time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(w.engine.Runs()) != 0 || transport.calls.Load() != 0 {
		t.Fatal("schedule defaults performed immediate work or called an upstream")
	}
}

func TestLegacyScheduleStartupImportsBalanceWithoutDispatchAndSurvivesRestart(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	source := scheduleMigrationConfig()
	started := time.Now()
	w, transport := openScheduleMigrationRuntime(t, stateDir, source)
	plans := w.engine.Schedules()
	byID := map[string]taskengine.Schedule{}
	enabled := 0
	for _, plan := range plans {
		byID[plan.ID] = plan
		if plan.Timezone != "Asia/Shanghai" || len(plan.AccountIDs) != 0 || plan.LastRunID != "" || plan.LastDispatched != nil || plan.LegacyEnabled == nil && plan.ID != "legacy-compensation" {
			t.Fatalf("import changed source scope or invented runtime history: %+v", plan)
		}
		if plan.Enabled {
			enabled++
			if plan.ID != "legacy-balance" || plan.NextRun == nil || !plan.NextRun.After(started) {
				t.Fatalf("only the read-only balance plan may be enabled, with a future next run: %+v", plan)
			}
		} else if plan.NextRun != nil {
			t.Fatalf("disabled imported plan has a next run: %+v", plan)
		}
	}
	balance, ok := byID["legacy-balance"]
	if !ok || enabled != 1 || balance.ProviderID != "workbuddy" || balance.TaskKey != "balance" || !reflect.DeepEqual(balance.Hours, []int{14}) || balance.Minute != 0 || balance.OncePerDay || !*balance.LegacyEnabled {
		t.Fatalf("legacy 14:00 balance schedule was lost or changed: %+v", balance)
	}
	trae, ok := byID["legacy-trae-check"]
	if !ok || trae.ProviderID != "trae" || trae.TaskKey != "checkin" || trae.Minute != 5 || !reflect.DeepEqual(trae.Hours, []int{0}) || *trae.LegacyEnabled {
		t.Fatalf("Trae legacy name was not mapped to the real task key and CST minute: %+v", trae)
	}
	if len(w.engine.Runs()) != 0 || transport.calls.Load() != 0 {
		t.Fatal("startup import dispatched work or called an upstream")
	}
	closeScheduleMigrationRuntime(w)

	changedSource := scheduleMigrationConfig()
	changedSource.BalanceEnabled = false
	changedSource.BalanceHours = []int{8, 20}
	restarted, restartedTransport := openScheduleMigrationRuntime(t, stateDir, changedSource)
	if !reflect.DeepEqual(restarted.engine.Schedules(), plans) {
		t.Fatal("restart reimported or rewrote persisted plans from the legacy config")
	}
	if len(restarted.engine.Runs()) != 0 || restartedTransport.calls.Load() != 0 {
		t.Fatal("restarting the imported engine dispatched work or called an upstream")
	}
}

// TestLegacyScheduleImportPreservesNonDefaultHoursAndDisabledSwitches 锁定
// 「导入不改写源意图」：非默认小时、显式关闭的开关、缺失小时段都必须原样
// 落到新引擎，且关闭的开关不能被默认值(true)复活成可执行计划。
func TestLegacyScheduleImportPreservesNonDefaultHoursAndDisabledSwitches(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	source := scheduleMigrationConfig()
	source.BalanceHours = []int{7, 19}
	source.BalanceEnabled = false
	source.CheckinHours = []int{6, 18}
	source.CheckinEnabled = true
	source.GrowthHours = []int{23}
	source.GrowthEnabled = false
	source.KeepaliveHours = nil
	w, _ := openScheduleMigrationRuntime(t, stateDir, source)
	byID := map[string]taskengine.Schedule{}
	for _, plan := range w.engine.Schedules() {
		byID[plan.ID] = plan
	}
	if len(byID) != 11 {
		t.Fatalf("expected balance/checkin/growth/travel/trae plus growth-stage plans, got %d plans: %+v", len(byID), byID)
	}
	balance := byID["legacy-balance"]
	if balance.Enabled || balance.LegacyEnabled == nil || *balance.LegacyEnabled || !reflect.DeepEqual(balance.Hours, []int{7, 19}) || balance.Minute != 0 {
		t.Fatalf("disabled balance switch or hours were rewritten: %+v", balance)
	}
	checkin := byID["legacy-checkin"]
	if checkin.Enabled || checkin.LegacyEnabled == nil || !*checkin.LegacyEnabled || !reflect.DeepEqual(checkin.Hours, []int{6, 18}) {
		t.Fatalf("side-effect plan did not preserve source switch/hours: %+v", checkin)
	}
	growth := byID["legacy-growth"]
	if growth.Enabled || *growth.LegacyEnabled || !reflect.DeepEqual(growth.Hours, []int{23}) {
		t.Fatalf("growth plan did not preserve disabled source state: %+v", growth)
	}
	trae := byID["legacy-trae-check"]
	if trae.Enabled || trae.LegacyEnabled == nil || *trae.LegacyEnabled || trae.Minute != 5 || !reflect.DeepEqual(trae.Hours, []int{0}) {
		t.Fatalf("trae plan did not preserve disabled source switch or CST minute: %+v", trae)
	}
	// travel_hours 是旧配置里真实存在、且新引擎有独立模块的流水线。升级时
	// 漏掉它会让旧部署的 Buddy 旅行计划在新引擎里无声消失（不是停用，而是
	// 根本不存在——控制台连「旧版曾启用」都看不到）。
	travel := byID["legacy-travel"]
	if travel.ProviderID != "workbuddy" || travel.TaskKey != "travel/" || travel.Enabled || travel.LegacyEnabled == nil || *travel.LegacyEnabled || !reflect.DeepEqual(travel.Hours, []int{15}) {
		t.Fatalf("travel plan was dropped or rewritten during import: %+v", travel)
	}
	if w.engine.Schedules()[0].NextRun != nil {
		t.Fatal("all imported plans were disabled but a next run was scheduled")
	}
}

// TestLegacyTraeEnabledIsRecordedButPlanStaysDisabled 锁定 Trae 的开关保真：
// 旧配置里 trae_check_enabled=true 时，导入的计划仍必须 disabled（升级不能
// 自动恢复写操作），但 legacy_enabled 必须记录 true——否则控制台会把「用户
// 原本开着」显示成「从未启用」，与本轮 checkin/growth/keepalive 的修复口径不一致。
func TestLegacyTraeEnabledIsRecordedButPlanStaysDisabled(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	source := scheduleMigrationConfig()
	source.TraeCheckEnabled = true
	w, transport := openScheduleMigrationRuntime(t, stateDir, source)
	var trae *taskengine.Schedule
	for i := range w.engine.Schedules() {
		plan := w.engine.Schedules()[i]
		if plan.ID == "legacy-trae-check" {
			trae = &plan
		}
	}
	if trae == nil {
		t.Fatalf("no trae plan imported: %+v", w.engine.Schedules())
	}
	if trae.Enabled || trae.NextRun != nil {
		t.Fatalf("trae plan imported as dispatchable after upgrade: %+v", *trae)
	}
	if trae.LegacyEnabled == nil || !*trae.LegacyEnabled {
		t.Fatalf("enabled legacy trae switch was not preserved: %+v", *trae)
	}
	if len(w.engine.Runs()) != 0 || transport.calls.Load() != 0 {
		t.Fatal("trae import dispatched work or called an upstream")
	}
}

// TestLegacyPlansUseExecutableTaskKeys 锁定「迁移计划真的会执行」这一条：
// 旧 config 只有模块级排程（growth_hours 表示成长中心整条流水线），而新引擎
// DiscoverCatalog 用 r.TaskKey == t.Key 逐字过滤。导入裸模块名时，计划按点
// 触发、发现 0 个任务、以 skipped 结束——看着启用、实际什么都不做。
//
// 本测试同时锁住两件事：
//   - 静态任务键（checkin/balance/keepalive/trae checkin）必须原样保留；
//   - 动态任务键的模块（growth/travel/…）必须导入为通配符，执行时才展开。
func TestLegacyPlansUseExecutableTaskKeys(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	source := scheduleMigrationConfig()
	source.GrowthEnabled = true
	source.GrowthHours = []int{12}
	source.CheckinEnabled = true
	source.CheckinHours = []int{9}
	source.KeepaliveEnabled = true
	source.KeepaliveHours = []int{22}
	source.TravelEnabled = true
	source.TravelHours = []int{15}
	w, _ := openScheduleMigrationRuntime(t, stateDir, source)
	byID := map[string]taskengine.Schedule{}
	for _, plan := range w.engine.Schedules() {
		byID[plan.ID] = plan
	}
	for _, tc := range []struct{ id, wantKey string }{
		{"legacy-checkin", "checkin"},
		{"legacy-balance", "balance"},
		{"legacy-keepalive", "keepalive"},
		{"legacy-trae-check", "checkin"},
		{"legacy-growth", "growth/"},
		{"legacy-travel", "travel/"},
	} {
		plan, ok := byID[tc.id]
		if !ok {
			t.Fatalf("%s 未导入：%+v", tc.id, byID)
		}
		if plan.TaskKey != tc.wantKey {
			t.Fatalf("%s 的 task_key=%q，应为 %q（否则执行时发现不到任何任务）", tc.id, plan.TaskKey, tc.wantKey)
		}
	}
	// 写操作即使旧配置启用也必须导入停用（balance 是纯读，按契约可自动启用）。
	for _, plan := range byID {
		if plan.ID == "legacy-balance" {
			continue
		}
		if plan.Enabled || plan.NextRun != nil {
			t.Fatalf("迁移计划不得自动恢复写操作：%+v", plan)
		}
	}
}

// TestLegacyTaskKeyCoversEveryDynamicModule 锁住映射表覆盖面：
// 任何任务键由发现结果动态生成、且不以模块名作为完整键的模块，都必须映射为
// 该模块的前缀。新加模块时若忘了这一步，会重演「计划静默跳过」。
//
// 不用全局 "*"：那会把签到、余额刷新等无关侧写任务一起带进这条计划，
// 等于悄悄扩大用户安排的执行范围。
func TestLegacyTaskKeyCoversEveryDynamicModule(t *testing.T) {
	for _, module := range []string{"growth", "travel", "redeem"} {
		if got := legacyTaskKey(module); got != module+"/" {
			t.Fatalf("%s 的动态任务键必须导入为模块前缀，实际 %q", module, got)
		}
	}
	// makeup 是唯一「动态周期但完整键」的模块：发现出来的键就是 "makeup"，
	// 周期放在 period_key 里，写成前缀反而一条也匹配不上。
	if got := legacyTaskKey("makeup"); got != "makeup" {
		t.Fatalf("makeup 是完整任务键，必须原样保留，实际 %q", got)
	}
	for _, fixed := range []string{"checkin", "balance", "keepalive"} {
		if got := legacyTaskKey(fixed); got != fixed {
			t.Fatalf("%s 是完整任务键，必须原样保留，实际 %q", fixed, got)
		}
	}
}
