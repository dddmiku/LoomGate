package taskengine_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/taskengine"
)

func TestDuplicateScheduleAccountsAreRejectedAtEveryWriteBoundary(t *testing.T) {
	for _, boundary := range []string{"save", "default-import", "legacy-import"} {
		t.Run(boundary, func(t *testing.T) {
			e := openBehaviorEngine(t, taskengine.Options{})
			registerBehavior(t, e, "fake", singleAdapter(syntheticTask("fake", "account-a", "task-a")))
			plan := taskengine.Schedule{ID: "fixture-plan", ProviderID: "fake", TaskKey: "task-a", Enabled: true, Hours: []int{9}, Timezone: "UTC", AccountIDs: []string{"account-a", "account-a"}}
			var err error
			switch boundary {
			case "save":
				_, err = e.SaveSchedule(plan)
			case "default-import":
				_, err = e.ImportDefaultSchedulesOnce("fixture-v1", []taskengine.Schedule{plan})
			case "legacy-import":
				_, err = e.ImportLegacySchedulesOnce([]taskengine.Schedule{plan})
			}
			if !errors.Is(err, taskengine.ErrInvalid) {
				t.Fatalf("duplicate scope was accepted at %s: %v", boundary, err)
			}
			if len(e.Schedules()) != 0 || len(e.Runs()) != 0 {
				t.Fatal("rejected scope changed durable schedules or dispatched tasks")
			}
		})
	}
}

func TestDuplicateScheduleAccountsAreRejectedWhenReadingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	registerBehavior(t, e, "fake", singleAdapter(syntheticTask("fake", "account-a", "task-a")))
	_, err := e.SaveSchedule(taskengine.Schedule{ID: "fixture-plan", ProviderID: "fake", TaskKey: "task-a", Hours: []int{9}, Timezone: "UTC", AccountIDs: []string{"account-a"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state["schedules"].(map[string]any)["fixture-plan"].(map[string]any)["account_ids"] = []string{"account-a", "account-a"}
	raw, _ = json.Marshal(state)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := taskengine.Open(taskengine.Options{Path: path}); err == nil {
		_ = reopened.Close()
		t.Fatal("invalid duplicate scope survived durable read validation")
	}
}
