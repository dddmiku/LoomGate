package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func loadInheritedScheduleConfig(t *testing.T, raw string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy-config.json")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func openInheritedScheduleRuntime(t *testing.T, stateDir string, cfg *Config, legacyAccount bool) (*workbenchRuntime, *scheduleMigrationTransport) {
	t.Helper()
	t.Setenv("WB2API_RUNTIME_DIR", "")
	p := pool.New("")
	t.Cleanup(p.Close)
	if legacyAccount {
		p.Add(&auth.Auth{UID: "synthetic-legacy-account", Domain: "codebuddy.cn"})
	}
	transport := &scheduleMigrationTransport{}
	up := upstream.New()
	up.HTTP.Transport, up.ChatHTTP.Transport = transport, transport
	w, _, err := openWorkbenchWithConfiguredSchedules(stateDir, p, up, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeScheduleMigrationRuntime(w) })
	return w, transport
}

func TestLegacyScheduleSectionPresenceIsNotAStoredSetting(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{
		{"missing", `{}`, false},
		{"null", `{"schedule":null}`, false},
		{"empty-object", `{"schedule":{}}`, true},
		{"configured", `{"schedule":{"checkin_enabled":false}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadInheritedScheduleConfig(t, tc.raw)
			if cfg.LegacyScheduleConfigured != tc.want {
				t.Fatalf("source schedule presence=%v want %v", cfg.LegacyScheduleConfigured, tc.want)
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte("LegacyScheduleConfigured")) || bytes.Contains(raw, []byte("legacy_schedule_configured")) {
				t.Fatal("source observation leaked into saved configuration")
			}
		})
	}
}

func TestFirstWorkbenchImportInheritsLegacySwitchesAndHours(t *testing.T) {
	cfg := loadInheritedScheduleConfig(t, `{"schedule":{"checkin_enabled":true,"checkin_hours":[7,19],"trae_check_enabled":true,"trae_check_hours":[1],"balance_enabled":false,"balance_hours":[16],"growth_enabled":false,"growth_hours":[11],"keepalive_enabled":true,"keepalive_hours":[5],"travel_enabled":false,"travel_hours":[6]}}`)
	w, transport := openInheritedScheduleRuntime(t, filepath.Join(t.TempDir(), "data"), cfg, false)
	plans := w.engine.Schedules()
	for _, tc := range []struct {
		id      string
		enabled bool
		hours   []int
		minute  int
	}{
		{"legacy-checkin", true, []int{7, 19}, 0},
		{"legacy-trae-check", true, []int{1}, 5},
		{"legacy-balance", false, []int{16}, 0},
		{"legacy-growth", false, []int{11}, 0},
		{"legacy-keepalive", true, []int{5}, 0},
		{"legacy-trae-keepalive", true, []int{5}, 0},
		{"legacy-travel", false, []int{6}, 0},
	} {
		plan, ok := findSchedule(plans, tc.id)
		if !ok || plan.Enabled != tc.enabled || !reflect.DeepEqual(plan.Hours, tc.hours) || plan.Minute != tc.minute {
			t.Errorf("legacy schedule not inherited: id=%s want_enabled=%v want_hours=%v got=%+v", tc.id, tc.enabled, tc.hours, plan)
		}
		if plan.Timezone != "Asia/Shanghai" || len(plan.AccountIDs) != 0 || plan.OncePerDay || plan.Enabled != (plan.NextRun != nil) {
			t.Errorf("source schedule lost timezone/scope/future-only scheduling: %+v", plan)
		}
		if plan.LegacyEnabled == nil || *plan.LegacyEnabled != tc.enabled {
			t.Errorf("source switch audit does not match actual configuration: %+v", plan)
		}
	}
	for _, id := range []string{"legacy-checkin-sequence", "legacy-growth-sequence", "legacy-makeup", "legacy-redeem", "legacy-lottery", "legacy-buddy-box", "legacy-resources"} {
		plan, ok := findSchedule(plans, id)
		if !ok || plan.Enabled || plan.LegacyEnabled != nil {
			t.Errorf("new task was armed or assigned an invented legacy switch: %+v", plan)
		}
	}
	for _, id := range []string{"legacy-school", "legacy-cat", "legacy-activity"} {
		if _, ok := findSchedule(plans, id); ok {
			t.Errorf("unsupported event plan restored: %s", id)
		}
	}
	if len(w.engine.Runs()) != 0 || transport.calls.Load() != 0 {
		t.Fatal("import dispatched work or queried a provider")
	}
}

func TestLegacyScheduleDefaultsAndNewWorkspaceRemainDistinct(t *testing.T) {
	for _, tc := range []struct {
		name, raw     string
		legacyAccount bool
		inherit       bool
	}{
		{"new-empty", `{}`, false, false},
		{"new-null", `{"schedule":null}`, false, false},
		{"legacy-explicit-empty", `{"schedule":{"checkin_hours":[],"trae_check_hours":null}}`, false, true},
		{"legacy-accounts-and-missing-section", `{}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadInheritedScheduleConfig(t, tc.raw)
			w, transport := openInheritedScheduleRuntime(t, filepath.Join(t.TempDir(), "data"), cfg, tc.legacyAccount)
			plans := w.engine.Schedules()
			checkin, _ := findSchedule(plans, "legacy-checkin")
			trae, _ := findSchedule(plans, "legacy-trae-check")
			if checkin.Enabled != tc.inherit || trae.Enabled != tc.inherit || !reflect.DeepEqual(checkin.Hours, []int{9, 21}) || !reflect.DeepEqual(trae.Hours, []int{0}) {
				t.Fatal("source defaults or new-install defaults changed")
			}
			if !tc.inherit {
				enabled := []string{}
				for _, plan := range plans {
					if plan.Enabled {
						enabled = append(enabled, plan.ID)
					}
				}
				if !reflect.DeepEqual(enabled, []string{"legacy-balance", "legacy-trae-balance"}) {
					t.Fatalf("new workspace armed unexpected tasks: %v", enabled)
				}
			}
			if len(w.engine.Runs()) != 0 || transport.calls.Load() != 0 {
				t.Fatal("startup performed provider work")
			}
		})
	}
}

func TestInheritedDefaultsNeverRewriteSavedPlansOrDeletionMarkers(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	cfg := loadInheritedScheduleConfig(t, `{"schedule":{"checkin_enabled":true}}`)
	w, _ := openInheritedScheduleRuntime(t, stateDir, cfg, false)
	plan, _ := findSchedule(w.engine.Schedules(), "legacy-checkin")
	plan.Enabled, plan.Hours, plan.Minute, plan.Timezone = false, []int{6}, 17, "UTC"
	plan.AccountIDs = []string{"synthetic-selected-account"}
	plan.LegacyEnabled = nil
	if _, err := w.engine.SaveSchedule(plan); err != nil {
		t.Fatal(err)
	}
	if err := w.engine.DeleteSchedule("legacy-travel"); err != nil {
		t.Fatal(err)
	}
	want := w.engine.Schedules()
	path := filepath.Join(stateDir, "task-engine.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	closeScheduleMigrationRuntime(w)
	cfg.Schedule.CheckinHours, cfg.Schedule.CheckinEnabled = []int{4}, true
	reopened, transport := openInheritedScheduleRuntime(t, stateDir, cfg, false)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened.engine.Schedules(), want) || !bytes.Equal(before, after) {
		t.Fatal("startup rewrote saved switches, times, account scope, audit or import/deletion markers")
	}
	if _, ok := findSchedule(reopened.engine.Schedules(), "legacy-travel"); ok {
		t.Fatal("deleted plan was resurrected")
	}
	if len(reopened.engine.Runs()) != 0 || transport.calls.Load() != 0 {
		t.Fatal("restart dispatched work")
	}
}

func TestLegacyDuplicateHoursRetainOneFirePerHour(t *testing.T) {
	cfg := loadInheritedScheduleConfig(t, `{"schedule":{"checkin_enabled":true,"checkin_hours":[19,7,19]}}`)
	w, transport := openInheritedScheduleRuntime(t, filepath.Join(t.TempDir(), "data"), cfg, false)
	plan, ok := findSchedule(w.engine.Schedules(), "legacy-checkin")
	if !ok || !plan.Enabled || !reflect.DeepEqual(plan.Hours, []int{7, 19}) {
		t.Fatalf("valid legacy membership hours blocked the import: %+v", plan)
	}
	if !reflect.DeepEqual(cfg.Schedule.CheckinHours, []int{19, 7, 19}) {
		t.Fatal("import mutated the loaded source configuration")
	}
	if len(w.engine.Runs()) != 0 || transport.calls.Load() != 0 {
		t.Fatal("normalizing source hours dispatched work")
	}
}
