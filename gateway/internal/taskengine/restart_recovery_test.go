package taskengine

import (
	"path/filepath"
	"testing"
)

// TestRestartCollapsesInterruptedRunsWithoutReplay 锁定崩溃恢复的三条契约：
//  1. 没有 durable work 的 running/queued 运行 → 标为 uncertain 且带
//     interrupted_restart，绝不假装成功，也不自动重发。
//  2. 剩余任务全部已 unknown 的运行 → 不再保留 work（没有可安全重放的步骤）。
//  3. 其余未完成任务 → 退回 queued 由引擎按 work 游标继续，不从头重跑。
func TestRestartCollapsesInterruptedRunsWithoutReplay(t *testing.T) {
	t.Run("no_work_becomes_uncertain", func(t *testing.T) {
		s := newState()
		s.Runs = []Run{{ID: "run-a", ProviderID: "fake", AccountIDs: []string{"a"}, Status: StatusQueued}}
		path := filepath.Join(t.TempDir(), "state.json")
		if err := writeState(path, s); err != nil {
			t.Fatal(err)
		}
		restored, err := readState(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(restored.Runs) != 1 || restored.Runs[0].Status != StatusUncertain || restored.Runs[0].ErrorCode != "interrupted_restart" || restored.Runs[0].FinishedAt == nil {
			t.Fatalf("interrupted run was not collapsed to uncertain: %+v", restored.Runs)
		}
	})

	t.Run("all_remaining_unknown_drops_work", func(t *testing.T) {
		task := actionTask()
		s := newState()
		s.Runs = []Run{{ID: "run-b", ProviderID: task.ProviderID, AccountIDs: []string{task.AccountID}, Status: StatusQueued}}
		s.Work["run-b"] = runWork{Tasks: []Task{task}, Next: 0, Status: StatusSkipped}
		s.Uncertain[taskKey(task)] = StageExecute
		path := filepath.Join(t.TempDir(), "state.json")
		if err := writeState(path, s); err != nil {
			t.Fatal(err)
		}
		restored, err := readState(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, kept := restored.Work["run-b"]; kept {
			t.Fatal("work with only unknown steps was kept for replay")
		}
		if restored.Runs[0].Status != StatusUncertain || restored.Runs[0].ErrorCode != "interrupted_restart" {
			t.Fatalf("run with only unknown steps was not collapsed: %+v", restored.Runs[0])
		}
	})

	t.Run("pending_work_requeues_with_checkpoint", func(t *testing.T) {
		first := actionTask()
		second := actionTask()
		second.Key = "second"
		s := newState()
		s.Runs = []Run{{ID: "run-c", ProviderID: first.ProviderID, AccountIDs: []string{first.AccountID}, Status: StatusQueued}}
		s.Work["run-c"] = runWork{Tasks: []Task{first, second}, Next: 1, Status: StatusSkipped}
		path := filepath.Join(t.TempDir(), "state.json")
		if err := writeState(path, s); err != nil {
			t.Fatal(err)
		}
		restored, err := readState(path)
		if err != nil {
			t.Fatal(err)
		}
		work, kept := restored.Work["run-c"]
		if !kept || work.Next != 1 {
			t.Fatalf("resumable work cursor was lost: %+v", work)
		}
		if restored.Runs[0].Status != StatusQueued || restored.Runs[0].FinishedAt != nil {
			t.Fatalf("resumable run was not requeued: %+v", restored.Runs[0])
		}
	})
}
