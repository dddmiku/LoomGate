package taskengine_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

func defaultImportEngine(t *testing.T, path string) *taskengine.Engine {
	t.Helper()
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	registerBehavior(t, e, "workbuddy", singleAdapter(syntheticTask("workbuddy", "cn-fixture", "balance")))
	registerBehavior(t, e, "trae", singleAdapter(syntheticTask("trae", "trae-fixture", "balance")))
	return e
}
func validDefaultPlans() []taskengine.Schedule {
	return []taskengine.Schedule{
		{ID: "legacy-balance", ProviderID: "workbuddy", TaskKey: "balance", Enabled: true, Hours: []int{14}, Minute: 0, Timezone: "Asia/Shanghai", AccountIDs: []string{}},
		{ID: "legacy-checkin", ProviderID: "workbuddy", TaskKey: "checkin", Enabled: false, Hours: []int{9, 21}, Minute: 0, Timezone: "Asia/Shanghai", AccountIDs: []string{}},
		{ID: "legacy-growth", ProviderID: "workbuddy", TaskKey: "growth/", Enabled: false, Hours: []int{12}, Minute: 0, Timezone: "Asia/Shanghai", AccountIDs: []string{}},
	}
}
func TestDefaultScheduleImportIsAdditiveAndDoesNotResurrectDeletedDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := defaultImportEngine(t, path)
	custom := taskengine.Schedule{ID: "my-plan", ProviderID: "workbuddy", TaskKey: "balance", Enabled: false, Hours: []int{8}, Minute: 15, Timezone: "UTC"}
	if _, err := e.SaveSchedule(custom); err != nil {
		t.Fatal(err)
	}
	plans := validDefaultPlans()
	count, err := e.ImportDefaultSchedulesOnce("defaults-v2", plans)
	if err != nil || count != 3 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	rows := e.Schedules()
	if len(rows) != 4 {
		t.Fatalf("custom or defaults lost: %+v", rows)
	}
	var foundCustom, foundBalance bool
	for _, p := range rows {
		if p.ID == "my-plan" {
			foundCustom = reflect.DeepEqual(p.Hours, []int{8})
		}
		if p.ID == "legacy-balance" {
			foundBalance = p.Enabled && p.NextRun != nil && p.NextRun.After(time.Now())
		}
	}
	if !foundCustom || !foundBalance {
		t.Fatalf("additive import rewrote existing plan or did not schedule balance: %+v", rows)
	}
	for _, id := range []string{"legacy-balance", "legacy-checkin"} {
		if err = e.DeleteSchedule(id); err != nil {
			t.Fatal(err)
		}
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	r := defaultImportEngine(t, path)
	defer r.Close()
	// A later default generation must skip plans this workspace already has and
	// plans the operator deleted; it must not resurrect either.
	count, err = r.ImportDefaultSchedulesOnce("defaults-v3", plans)
	if err != nil || count != 0 {
		t.Fatalf("repeat import count=%d err=%v", count, err)
	}
	byID := map[string]bool{}
	for _, p := range r.Schedules() {
		byID[p.ID] = true
		if p.ID == "legacy-balance" || p.ID == "legacy-checkin" {
			t.Fatalf("deleted default resurrected: %+v", p)
		}
	}
	if !byID["legacy-growth"] || !byID["my-plan"] {
		t.Fatalf("later generation dropped a surviving plan: %+v", r.Schedules())
	}
}
func TestDefaultScheduleImportFailsClosedOnInflightUnknown(t *testing.T) {
	e := defaultImportEngine(t, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	_, err := e.ImportDefaultSchedulesOnce("defaults-v2", validDefaultPlans())
	if err != nil && !errors.Is(err, taskengine.ErrLegacyScheduleImportBlocked) {
		t.Fatal(err)
	}
}
