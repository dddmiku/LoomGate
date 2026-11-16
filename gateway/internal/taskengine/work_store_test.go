package taskengine

import (
	"path/filepath"
	"testing"
	"time"
)

func TestV2BoundReceiptsRemainTrustedWhileAddingWorkRecovery(t *testing.T) {
	task := actionTask()
	task.actionAttempt = "0123456789abcdef0123456789abcdef"
	key := taskKey(task)
	task.AcceptedStage = StageClaim
	old := newState()
	old.Version = 2
	old.Work = nil
	old.Uncertain[key] = StageClaim
	old.PendingTasks[key] = task
	old.Receipts[key] = actionReceipt{Attempt: task.actionAttempt, Stage: StageClaim, Binding: actionBinding(task, StageClaim)}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeState(path, old); err != nil {
		t.Fatal(err)
	}
	restored, err := readState(path)
	if err != nil || restored.Version != 3 || restored.PendingTasks[key].AcceptedStage != StageClaim || len(restored.Receipts) != 1 || len(restored.Uncertain) != 1 {
		t.Fatal("v2 evidence lost during work format migration")
	}
}
func TestWorkRecoveryRejectsMalformedScopeCursorAndConfirmation(t *testing.T) {
	for _, mode := range []string{"cursor", "scope", "confirmation", "status", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			task := actionTask()
			s := newState()
			s.Runs = []Run{{ID: "run", ProviderID: task.ProviderID, AccountIDs: []string{task.AccountID}, Status: StatusQueued}}
			work := runWork{Tasks: []Task{task}, Status: StatusSkipped}
			switch mode {
			case "cursor":
				work.Next = 2
			case "scope":
				work.Tasks[0].AccountID = "foreign"
			case "confirmation":
				work.Confirmed = map[string]bool{"invalid": true}
			case "status":
				work.Status = "invalid"
			case "orphan":
				now := time.Now()
				s.Runs[0].FinishedAt = &now
			}
			s.Work["run"] = work
			path := filepath.Join(t.TempDir(), "state.json")
			if err := writeState(path, s); err != nil {
				t.Fatal(err)
			}
			if _, err := readState(path); err == nil {
				t.Fatal("malformed durable work accepted")
			}
		})
	}
}
