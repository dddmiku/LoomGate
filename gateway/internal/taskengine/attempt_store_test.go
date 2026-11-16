package taskengine

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestActionReferencePrivatePersistedAndBoundToStage(t *testing.T) {
	task := actionTask()
	task.actionAttempt = "0123456789abcdef0123456789abcdef"
	task.actionStage = StageExecute
	ref := ActionReference(task, StageExecute)
	if len(ref) != 64 || ActionReference(task, StageClaim) != "" {
		t.Fatal("reference not stage bound")
	}
	raw, _ := json.Marshal(task)
	if strings.Contains(string(raw), ref) || strings.Contains(string(raw), task.actionAttempt) {
		t.Fatal("internal reference exposed by task JSON")
	}
	state := newState()
	key := taskKey(task)
	state.PendingTasks[key] = task
	state.Uncertain[key] = StageExecute
	state.Attempts[key] = actionReceipt{Attempt: task.actionAttempt, Stage: StageExecute, Binding: ref}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeState(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := readState(path)
	if err != nil || loaded.Attempts[key].Binding != ref {
		t.Fatal("attempt lost on restart")
	}
	state.Attempts[key] = actionReceipt{Attempt: task.actionAttempt, Stage: StageClaim, Binding: ref}
	writeState(path, state)
	if _, err = readState(path); err == nil {
		t.Fatal("mismatched attempt accepted")
	}
}
func TestV3UnknownCannotInventRefreshAttempt(t *testing.T) {
	task := actionTask()
	s := newState()
	s.Version = 3
	key := taskKey(task)
	s.Uncertain[key] = StageExecute
	s.PendingTasks[key] = task
	path := filepath.Join(t.TempDir(), "state.json")
	writeState(path, s)
	loaded, err := readState(path)
	if err != nil || loaded.Version != 4 || len(loaded.Attempts) != 0 || len(loaded.Uncertain) != 1 {
		t.Fatal("legacy unknown received invented proof")
	}
}
