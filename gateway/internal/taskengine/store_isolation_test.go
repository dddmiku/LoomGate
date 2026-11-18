package taskengine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCorruptStoreIsIsolatedWhileTamperedStoreStaysFailClosed 区分两类损坏：
//   - 语法损坏（不是 JSON）→ 隔离到固定 .corrupt 槽位并以空状态启动，
//     否则整个网关会因为一个坏文件而无法打开控制台自救；
//   - 语义篡改（JSON 合法但凭据绑定不匹配）→ 必须继续失败关闭，
//     绝不能因为"能启动"就信任伪造的回执。
func TestCorruptStoreIsIsolatedWhileTamperedStoreStaysFailClosed(t *testing.T) {
	t.Run("syntax_corruption_is_isolated", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "task-engine.json")
		if err := os.WriteFile(path, []byte("{not valid json"), 0600); err != nil {
			t.Fatal(err)
		}
		e, err := Open(Options{Path: path})
		if err != nil {
			t.Fatalf("syntax corruption blocked startup: %v", err)
		}
		defer e.Close()
		if len(e.Schedules()) != 0 || len(e.Runs()) != 0 {
			t.Fatal("corrupt store produced phantom state")
		}
		backup, err := os.ReadFile(path + ".corrupt")
		if err != nil || string(backup) != "{not valid json" {
			t.Fatalf("original corrupt bytes were not preserved: %v", err)
		}
	})
	t.Run("tampered_receipt_stays_fail_closed", func(t *testing.T) {
		task := actionTask()
		task.actionAttempt = "0123456789abcdef0123456789abcdef"
		key := taskKey(task)
		state := newState()
		state.PendingTasks[key] = task
		state.Uncertain[key] = StageClaim
		state.Receipts[key] = actionReceipt{Attempt: task.actionAttempt, Stage: StageClaim, Binding: "wrong-binding"}
		path := filepath.Join(t.TempDir(), "tasks.json")
		if err := writeState(path, state); err != nil {
			t.Fatal(err)
		}
		if e, err := Open(Options{Path: path}); err == nil {
			e.Close()
			t.Fatal("tampered receipt was trusted after isolation change")
		}
	})
}
