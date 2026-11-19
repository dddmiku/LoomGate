package main

import (
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

type scheduleMigrationTransport struct{ calls atomic.Int32 }

func (tr *scheduleMigrationTransport) RoundTrip(*http.Request) (*http.Response, error) {
	tr.calls.Add(1)
	return nil, errors.New("synthetic_upstream_must_not_be_called")
}

func openScheduleMigrationRuntime(t *testing.T, stateDir string) (*workbenchRuntime, *scheduleMigrationTransport) {
	t.Helper()
	for _, key := range []string{"WB2API_RUNTIME_DIR"} {
		t.Setenv(key, "")
	}
	p := pool.New("")
	t.Cleanup(func() { p.Close() })
	transport := &scheduleMigrationTransport{}
	up := upstream.New()
	up.HTTP.Transport = transport
	up.ChatHTTP.Transport = transport
	w, _, err := openWorkbenchWithBuiltinSchedules(stateDir, p, up)
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

// TestBuiltinSchedulesAreTheLegacyFixedList 锁定「任务与定时随包发布」：
// 计划清单来自各任务文件自己的 DefaultPlan，不读旧 config。默认启用的只有
// 纯读任务（WorkBuddy 余额、Trae 余额），所有写任务都导入为停用。
func TestBuiltinSchedulesAreTheLegacyFixedList(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	w, transport := openScheduleMigrationRuntime(t, stateDir)
	plans := w.engine.Schedules()
	byID := map[string]taskengine.Schedule{}
	for _, plan := range plans {
		byID[plan.ID] = plan
		if plan.TaskKey == "*" || plan.TaskKey == "" {
			t.Fatalf("builtin plan broadened its task scope: %+v", plan)
		}
	}
	for _, tc := range []struct {
		id, provider, key string
		hours             []int
		minute            int
		enabled           bool
		legacy            bool
	}{
		{"legacy-checkin", "workbuddy", "checkin", []int{9, 21}, 0, false, true},
		{"legacy-trae-check", "trae", "checkin", []int{0}, 5, false, true},
		{"legacy-balance", "workbuddy", "balance", []int{14}, 0, true, true},
		{"legacy-trae-balance", "trae", "balance", []int{14}, 0, true, false},
		{"legacy-travel", "workbuddy", "travel/", []int{15}, 0, false, true},
		{"legacy-keepalive", "workbuddy", "keepalive", []int{22}, 0, false, true},
		{"legacy-trae-keepalive", "trae", "keepalive", []int{22}, 0, false, true},
		{"legacy-growth", "workbuddy", "growth/", []int{12}, 0, false, true},
		{"legacy-makeup", "workbuddy", "makeup", []int{12}, 5, false, true},
		{"legacy-redeem", "workbuddy", "redeem/", []int{12}, 10, false, true},
		{"legacy-lottery", "workbuddy", "lottery", []int{12}, 15, false, true},
		{"legacy-buddy-box", "workbuddy", "buddy_box", []int{12}, 20, false, true},
		{"legacy-resources", "workbuddy", "resources", []int{12}, 25, false, true},
		{"legacy-compensation", "workbuddy", "compensation", []int{12}, 30, false, false},
	} {
		p, ok := byID[tc.id]
		if !ok || p.ProviderID != tc.provider || p.TaskKey != tc.key || !reflect.DeepEqual(p.Hours, tc.hours) || p.Minute != tc.minute || p.Enabled != tc.enabled {
			t.Errorf("builtin %s missing or changed: %+v", tc.id, p)
		}
		if !ok {
			continue
		}
		if p.Timezone != "Asia/Shanghai" || len(p.AccountIDs) != 0 || p.Enabled != (p.NextRun != nil) {
			t.Errorf("builtin %s lost scope/switch: %+v", tc.id, p)
		}
		if tc.id == "legacy-compensation" || tc.id == "legacy-trae-balance" {
			if p.LegacyEnabled != nil {
				t.Errorf("%s invented a legacy source switch: %+v", tc.id, p)
			}
			continue
		}
		if p.LegacyEnabled == nil || *p.LegacyEnabled != tc.legacy {
			t.Errorf("builtin %s lost the recorded legacy switch: %+v", tc.id, p)
		}
	}
	// 旧版里没有独立可执行入口的伪造上报类任务不得复活。
	for _, id := range []string{"legacy-school", "legacy-cat", "legacy-activity", "legacy-runner"} {
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

// TestBuiltinScheduleImportIsStableAcrossRestarts 锁定「升级只补齐、不重写」：
// 第二次启动不得改写已保存的计划，也不得派发。
func TestBuiltinScheduleImportIsStableAcrossRestarts(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	started := time.Now()
	w, transport := openScheduleMigrationRuntime(t, stateDir)
	plans := w.engine.Schedules()
	enabled := 0
	for _, plan := range plans {
		if plan.Timezone != "Asia/Shanghai" || len(plan.AccountIDs) != 0 || plan.LastRunID != "" || plan.LastDispatched != nil {
			t.Fatalf("import changed source scope or invented runtime history: %+v", plan)
		}
		if plan.Enabled {
			enabled++
			if !strings.HasPrefix(plan.ID, "legacy-balance") && plan.ID != "legacy-trae-balance" {
				t.Fatalf("only read-only balance plans may start enabled: %+v", plan)
			}
			if plan.NextRun == nil || !plan.NextRun.After(started) {
				t.Fatalf("enabled plan has no future next run: %+v", plan)
			}
		} else if plan.NextRun != nil {
			t.Fatalf("disabled imported plan has a next run: %+v", plan)
		}
	}
	if enabled != 2 {
		t.Fatalf("expected exactly the two read-only balance plans enabled, got %d", enabled)
	}
	trae, ok := findSchedule(plans, "legacy-trae-check")
	if !ok || trae.ProviderID != "trae" || trae.TaskKey != "checkin" || trae.Minute != 5 || !reflect.DeepEqual(trae.Hours, []int{0}) {
		t.Fatalf("Trae plan was not mapped to the real task key and CST minute: %+v", trae)
	}
	if len(w.engine.Runs()) != 0 || transport.calls.Load() != 0 {
		t.Fatal("startup import dispatched work or called an upstream")
	}
	closeScheduleMigrationRuntime(w)

	restarted, restartedTransport := openScheduleMigrationRuntime(t, stateDir)
	if !reflect.DeepEqual(restarted.engine.Schedules(), plans) {
		t.Fatal("restart reimported or rewrote persisted plans")
	}
	if len(restarted.engine.Runs()) != 0 || restartedTransport.calls.Load() != 0 {
		t.Fatal("restarting the imported engine dispatched work or called an upstream")
	}
}

// TestBuiltinScheduleOperatorEditWins 锁定「操作员改过的计划不被默认值覆盖」。
func TestBuiltinScheduleOperatorEditWins(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	w, _ := openScheduleMigrationRuntime(t, stateDir)
	edited := taskengine.Schedule{ID: "legacy-growth", ProviderID: "workbuddy", TaskKey: "growth/", Enabled: true, Hours: []int{7}, Minute: 30, Timezone: "UTC", AccountIDs: []string{}}
	if _, err := w.engine.SaveSchedule(edited); err != nil {
		t.Fatal(err)
	}
	closeScheduleMigrationRuntime(w)

	restarted, _ := openScheduleMigrationRuntime(t, stateDir)
	saved, ok := findSchedule(restarted.engine.Schedules(), "legacy-growth")
	if !ok || !reflect.DeepEqual(saved.Hours, []int{7}) || saved.Minute != 30 || saved.Timezone != "UTC" || !saved.Enabled {
		t.Fatalf("operator edit was overwritten by the default import: %+v", saved)
	}
}

func findSchedule(plans []taskengine.Schedule, id string) (taskengine.Schedule, bool) {
	for _, plan := range plans {
		if plan.ID == id {
			return plan, true
		}
	}
	return taskengine.Schedule{}, false
}

// TestLegacyTraeEnabledIsRecordedButPlanStaysDisabled 锁定 Trae 的开关保真：
// 旧配置里 trae_check_enabled=true 时，导入的计划仍必须 disabled（升级不能
// 自动恢复写操作），但 legacy_enabled 必须记录 true——否则控制台会把「用户
// 原本开着」显示成「从未启用」，与本轮 checkin/growth/keepalive 的修复口径不一致。
func TestLegacyTraeEnabledIsRecordedButPlanStaysDisabled(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	w, transport := openScheduleMigrationRuntime(t, stateDir)
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
	w, _ := openScheduleMigrationRuntime(t, stateDir)
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
		// 纯读余额刷新（WorkBuddy 与 Trae）可按契约随包启用；其余写任务一律停用。
		if plan.TaskKey == "balance" {
			continue
		}
		if plan.Enabled || plan.NextRun != nil {
			t.Fatalf("迁移计划不得自动恢复写操作：%+v", plan)
		}
	}
}
