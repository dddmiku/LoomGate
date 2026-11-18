package taskengine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

func parseBehaviorTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNextFireUsesCalendarZoneAndDoesNotRepeatDSTHour(t *testing.T) {
	tests := []struct {
		name, zone, after, want string
		hours                   []int
		minute                  int
	}{
		{"shanghai-day-rollover", "Asia/Shanghai", "2026-11-15T15:59:00Z", "2026-11-15T16:05:00Z", []int{0}, 5},
		{"spring-missing-hour", "America/New_York", "2026-03-08T05:00:00Z", "2026-03-09T06:30:00Z", []int{2}, 30},
		{"autumn-first-occurrence", "America/New_York", "2026-11-01T04:00:00Z", "2026-11-01T05:30:00Z", []int{1}, 30},
		{"autumn-after-first-occurrence", "America/New_York", "2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z", []int{1}, 30},
		{"half-hour-DST-gap", "Australia/Lord_Howe", "2026-10-03T13:30:00Z", "2026-10-04T15:15:00Z", []int{2}, 15},
		{"unordered-hours-earliest", "UTC", "2026-11-15T03:00:00Z", "2026-11-15T04:15:00Z", []int{19, 4, 12}, 15},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := taskengine.Schedule{Enabled: true, Timezone: tc.zone, Hours: tc.hours, Minute: tc.minute}
			got := taskengine.NextFire(s, parseBehaviorTime(t, tc.after))
			if !got.Equal(parseBehaviorTime(t, tc.want)) {
				t.Errorf("NextFire=%s, want %s", got.Format(time.RFC3339), tc.want)
			}
		})
	}
}

func TestScheduleUpdateIsImmediatelyDurableAndSnapshotIsDetached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	registerBehavior(t, e, "fake", singleAdapter(syntheticTask("fake", "account-a", "task-a")))
	s := taskengine.Schedule{ID: "morning", ProviderID: "fake", Enabled: true, Hours: []int{9}, Minute: 15, Timezone: "Asia/Shanghai", AccountIDs: []string{"account-a"}}
	if _, err := e.SaveSchedule(s); err != nil {
		t.Fatal(err)
	}
	s.Hours, s.Minute, s.Timezone, s.Enabled = []int{16, 8}, 25, "UTC", false
	s.AccountIDs = []string{"account-b"}
	saved, err := e.SaveSchedule(s)
	if err != nil {
		t.Fatal(err)
	}
	if saved.NextRun != nil {
		t.Error("disabled schedule retained an active next time")
	}
	var disk struct {
		Schedules map[string]taskengine.Schedule `json:"schedules"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	want := disk.Schedules["morning"]
	if want.Enabled || want.Timezone != "UTC" || want.Minute != 25 || !reflect.DeepEqual(want.Hours, []int{8, 16}) || !reflect.DeepEqual(want.AccountIDs, []string{"account-b"}) {
		t.Fatalf("updated configuration was not persisted: %+v", want)
	}
	// Mutating either a caller-owned slice or the returned snapshot must not alter live configuration.
	s.Hours[0], s.AccountIDs[0] = 23, "caller-mutated"
	saved.Hours[1], saved.AccountIDs[0] = 22, "returned-mutated"
	live := e.Schedules()[0]
	if !reflect.DeepEqual(live.Hours, want.Hours) || !reflect.DeepEqual(live.AccountIDs, want.AccountIDs) {
		t.Errorf("caller mutation changed live schedule without persistence: hours=%v accounts=%v", live.Hours, live.AccountIDs)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openBehaviorEngine(t, taskengine.Options{Path: path})
	restored := reopened.Schedules()[0]
	if !reflect.DeepEqual(restored, want) {
		t.Errorf("restart schedule differs from durable configuration: got=%+v want=%+v", restored, want)
	}
}

func TestFailedScheduleWriteDoesNotChangeActiveConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	registerBehavior(t, e, "fake", singleAdapter(syntheticTask("fake", "account-a", "task-a")))
	s := taskengine.Schedule{ID: "morning", ProviderID: "fake", Enabled: true, Hours: []int{9}, Minute: 15, Timezone: "UTC"}
	if _, err := e.SaveSchedule(s); err != nil {
		t.Fatal(err)
	}
	before := e.Schedules()[0]
	// This is a test-owned obstruction to the atomic-write destination, not a user file.
	if err := os.Mkdir(path+".pending", 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path + ".pending") })
	s.Enabled, s.Hours = false, []int{11}
	if _, err := e.SaveSchedule(s); err == nil {
		t.Fatal("expected durable write failure")
	}
	if after := e.Schedules()[0]; !reflect.DeepEqual(after, before) {
		t.Errorf("failed write changed live schedule: before=%+v after=%+v", before, after)
	}
}

func TestRepeatedTickAndRestartDispatchEachSavedInstantOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	a := singleAdapter(syntheticTask("fake", "account-a", "task-a"))
	registerBehavior(t, e, "fake", a)
	s, err := e.SaveSchedule(taskengine.Schedule{ID: "scheduled", ProviderID: "fake", Enabled: true, Hours: []int{9}, Timezone: "UTC"})
	if err != nil || s.NextRun == nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	due := *s.NextRun
	for i := 0; i < 3; i++ {
		if err := e.Tick(due); err != nil {
			t.Fatal(err)
		}
	}
	runs := e.Runs()
	if len(runs) != 1 {
		t.Fatalf("same instant dispatched %d times", len(runs))
	}
	awaitBehaviorRun(t, e, runs[0].ID)
	if runs[0].ScheduleID != "scheduled" || runs[0].Trigger != "schedule" {
		t.Errorf("missing schedule attribution: %+v", runs[0])
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openBehaviorEngine(t, taskengine.Options{Path: path})
	registerBehavior(t, reopened, "fake", a)
	if err := reopened.Tick(due); err != nil {
		t.Fatal(err)
	}
	if len(reopened.Runs()) != 1 {
		t.Fatal("restart repeated the same scheduled instant")
	}
	if err := reopened.Tick(*reopened.Schedules()[0].NextRun); err != nil {
		t.Fatal(err)
	}
	if len(reopened.Runs()) != 2 {
		t.Fatal("next scheduled instant was not dispatched")
	}
}

func TestDispatchFailureRemainsRetryableWithoutLosingTheDueInstant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	a := singleAdapter(syntheticTask("fake", "account-a", "task-a"))
	registerBehavior(t, e, "fake", a)
	s, err := e.SaveSchedule(taskengine.Schedule{ID: "scheduled", ProviderID: "fake", Enabled: true, Hours: []int{9}, Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	due := *s.NextRun
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openBehaviorEngine(t, taskengine.Options{Path: path})
	// A provider may not be registered/ready immediately after process startup.
	if err := reopened.Tick(due); err == nil {
		t.Fatal("expected unavailable-provider dispatch failure")
	}
	registerBehavior(t, reopened, "fake", a)
	if err := reopened.Tick(due); err != nil {
		t.Fatal(err)
	}
	if len(reopened.Runs()) != 1 {
		t.Fatal("failed dispatch advanced the schedule and permanently lost the due instant")
	}
}

func TestOncePerDayScheduleDoesNotDispatchTwiceInOneLocalDay(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	a := singleAdapter(syntheticTask("fake", "account-a", "repeatable-task"))
	registerBehavior(t, e, "fake", a)
	hours := make([]int, 24)
	for i := range hours {
		hours[i] = i
	}
	s, err := e.SaveSchedule(taskengine.Schedule{ID: "daily", ProviderID: "fake", Enabled: true, Hours: hours, Timezone: "Asia/Shanghai", OncePerDay: true})
	if err != nil {
		t.Fatal(err)
	}
	// Pick the next local midnight so there are definitely two configured slots in one local day.
	loc, _ := time.LoadLocation(s.Timezone)
	next := s.NextRun.In(loc)
	day := time.Date(next.Year(), next.Month(), next.Day()+1, 0, 0, 0, 0, loc)
	if err := e.Tick(day); err != nil {
		t.Fatal(err)
	}
	first := e.Runs()
	if len(first) != 1 {
		t.Fatalf("first dispatch count=%d", len(first))
	}
	awaitBehaviorRun(t, e, first[0].ID)
	if err := e.Tick(day.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(e.Runs()) != 1 {
		t.Error("OncePerDay schedule dispatched again in the same configured local day")
	}
}

func TestCatalogRejectsDailyTasksWithoutAuthoritativeBusinessPeriod(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	task := syntheticTask("fake", "account-a", "daily")
	task.OncePerDay = true
	registerBehavior(t, e, "fake", singleAdapter(task))
	if _, err := e.Catalog(context.Background(), "fake", taskengine.Scope{}); err == nil {
		t.Fatal("daily task without upstream business period accepted")
	}
}

func TestLegacyScheduleImportIsOneShotAndOnlyAppliesToEmptyEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	a := singleAdapter(syntheticTask("fake", "account-a", "balance"))
	registerBehavior(t, e, "fake", a)

	candidate := taskengine.Schedule{ID: "legacy-balance", ProviderID: "fake", TaskKey: "balance", Enabled: true, Hours: []int{14}, Minute: 0, Timezone: "Asia/Shanghai", AccountIDs: []string{}}
	applied, err := e.ImportLegacySchedulesOnce([]taskengine.Schedule{candidate})
	if err != nil || !applied {
		t.Fatalf("first import applied=%v err=%v", applied, err)
	}
	plans := e.Schedules()
	if len(plans) != 1 || plans[0].ID != candidate.ID || plans[0].TaskKey != candidate.TaskKey || !plans[0].Enabled || plans[0].Timezone != "Asia/Shanghai" || len(plans[0].AccountIDs) != 0 || plans[0].NextRun == nil || !plans[0].NextRun.After(time.Now()) {
		t.Fatalf("imported plan does not preserve a future active schedule: %+v", plans)
	}
	if len(e.Runs()) != 0 {
		t.Fatal("importing a schedule dispatched task work")
	}
	if err := e.DeleteSchedule(candidate.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := openBehaviorEngine(t, taskengine.Options{Path: path})
	registerBehavior(t, restarted, "fake", a)
	applied, err = restarted.ImportLegacySchedulesOnce([]taskengine.Schedule{candidate})
	if err != nil || applied {
		t.Fatalf("restart import applied=%v err=%v; deleted legacy plan must not be resurrected", applied, err)
	}
	if len(restarted.Schedules()) != 0 || len(restarted.Runs()) != 0 {
		t.Fatalf("one-shot import changed the post-delete state: schedules=%+v runs=%+v", restarted.Schedules(), restarted.Runs())
	}
}

// TestLegacyImportAllowsRunsHistoryButBlocksUserPlansAndInFlightMutations
// 锁定 P2-02 的分离口径：历史执行记录（runs）不再永久阻止导入只读计划；
// 已有用户计划或在途 mutation（pending/receipts/attempts）仍阻止，且标记保持未完成。
func TestLegacyImportAllowsRunsHistoryButBlocksUserPlansAndInFlightMutations(t *testing.T) {
	candidate := taskengine.Schedule{ID: "legacy-balance", ProviderID: "fake", TaskKey: "balance", Enabled: true, Hours: []int{14}, Timezone: "Asia/Shanghai"}
	adapter := singleAdapter(syntheticTask("fake", "account-a", "balance"))

	t.Run("runs_history_only_imports", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		e := openBehaviorEngine(t, taskengine.Options{Path: path})
		registerBehavior(t, e, "fake", adapter)
		if _, err := e.Submit(taskengine.Request{ProviderID: "fake"}); err != nil {
			t.Fatal(err)
		}
		awaitBehaviorRun(t, e, e.Runs()[0].ID)
		applied, err := e.ImportLegacySchedulesOnce([]taskengine.Schedule{candidate})
		if err != nil || !applied {
			t.Fatalf("runs history must not block the read-only import: applied=%v err=%v", applied, err)
		}
	})

	t.Run("existing_schedule_blocks_without_marker", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		e := openBehaviorEngine(t, taskengine.Options{Path: path})
		registerBehavior(t, e, "fake", adapter)
		if _, err := e.SaveSchedule(taskengine.Schedule{ID: "user-plan", ProviderID: "fake", TaskKey: "balance", Enabled: true, Hours: []int{9}, Timezone: "Asia/Shanghai"}); err != nil {
			t.Fatal(err)
		}
		applied, err := e.ImportLegacySchedulesOnce([]taskengine.Schedule{candidate})
		if applied || !errors.Is(err, taskengine.ErrLegacyScheduleImportBlocked) {
			t.Fatalf("user plan must block import: applied=%v err=%v", applied, err)
		}
		if _, err := e.SaveSchedule(taskengine.Schedule{ID: "user-plan", ProviderID: "fake", TaskKey: "balance", Enabled: true, Hours: []int{10}, Timezone: "Asia/Shanghai"}); err != nil {
			t.Fatalf("marker was consumed by a blocked import: %v", err)
		}
	})
}

// TestScheduleEditPreservesLegacySourceFlag 锁定迁移来源标记的存活：
// 旧配置导入的计划带 legacy_enabled 审计事实；用户在控制台改时间或停启用
// 时若前端漏传，后端不能替它编一个新值。这里从引擎层验证：带回原值时编辑
// 后仍在，明确置 nil 时才消失（前端 cleanSchedule 必须原样带回）。
func TestScheduleEditPreservesLegacySourceFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	registerBehavior(t, e, "fake", singleAdapter(syntheticTask("fake", "account-a", "task-a")))
	enabled := true
	s := taskengine.Schedule{ID: "legacy-checkin", ProviderID: "fake", TaskKey: "checkin", Enabled: false, Hours: []int{9}, Minute: 0, Timezone: "Asia/Shanghai", LegacyEnabled: &enabled}
	if _, err := e.SaveSchedule(s); err != nil {
		t.Fatal(err)
	}
	s.Hours, s.Minute, s.Enabled = []int{7, 19}, 30, true
	saved, err := e.SaveSchedule(s)
	if err != nil {
		t.Fatal(err)
	}
	if saved.LegacyEnabled == nil || !*saved.LegacyEnabled {
		t.Fatalf("editing a legacy plan dropped the recorded source switch: %+v", saved)
	}
	edited := saved
	edited.LegacyEnabled = nil
	cleared, err := e.SaveSchedule(edited)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.LegacyEnabled != nil {
		t.Fatalf("engine invented a legacy source flag after it was explicitly cleared: %+v", cleared)
	}
}

// TestMigratedDocumentDoesNotReimportOrResurrectDeletedPlans 验证离线迁移产物
// 与「一次性导入」契约的衔接：迁移工具写的 task-engine.json 带
// legacy_schedule_import_done=true，网关启动时的应用内导入必须
//   - 不把已存在的迁移计划判成「用户数据冲突」而每次启动重试；
//   - 不在用户删掉计划后把它重新写回（与 TestLegacyScheduleImportIsOneShot… 同契约）。
//
// 这里的文档形状来自 tools/migration/legacy_schedule.py（见其 test_migrate.py），
// 只手写等价 JSON，不依赖 Python 运行时。
func TestMigratedDocumentDoesNotReimportOrResurrectDeletedPlans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-engine.json")
	migrated := `{"format":4,"version":4,"schedules":{"legacy-balance":{"id":"legacy-balance","task_key":"balance","provider_id":"fake","enabled":false,"hours":[14],"minute":0,"timezone":"Asia/Shanghai","account_ids":[],"once_per_day":false,"legacy_enabled":true}},"runs":[],"completed":{},"uncertain":{},"pending_tasks":{},"receipts":{},"attempts":{},"work":{},"legacy_schedule_import_done":true}`
	if err := os.WriteFile(path, []byte(migrated), 0600); err != nil {
		t.Fatal(err)
	}
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	a := singleAdapter(syntheticTask("fake", "account-a", "balance"))
	registerBehavior(t, e, "fake", a)
	candidate := taskengine.Schedule{ID: "legacy-checkin", ProviderID: "fake", TaskKey: "checkin", Enabled: false, Hours: []int{9}, Minute: 0, Timezone: "Asia/Shanghai"}
	applied, err := e.ImportLegacySchedulesOnce([]taskengine.Schedule{candidate})
	if err != nil || applied {
		t.Fatalf("migrated workspace must not import again: applied=%v err=%v", applied, err)
	}
	plans := e.Schedules()
	if len(plans) != 1 || plans[0].ID != "legacy-balance" {
		t.Fatalf("startup import modified a migrated workspace: %+v", plans)
	}
	if err := e.DeleteSchedule("legacy-balance"); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := openBehaviorEngine(t, taskengine.Options{Path: path})
	registerBehavior(t, restarted, "fake", a)
	applied, err = restarted.ImportLegacySchedulesOnce([]taskengine.Schedule{candidate})
	if err != nil || applied {
		t.Fatalf("restart import applied=%v err=%v", applied, err)
	}
	if len(restarted.Schedules()) != 0 {
		t.Fatalf("deleted migrated plan was resurrected: %+v", restarted.Schedules())
	}
}
