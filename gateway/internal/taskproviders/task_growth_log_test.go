package taskproviders

import (
	"strings"
	"testing"

	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

// 用户实测反馈：成长任务的「接单」日志写成「官方确认任务已接单。」，运行结尾又写
// 「成功」，于是完全看不出任务本身是否完成——接单只是让官方开始追踪，真实工作
// 仍要在官方客户端做。
//
// 这两条锁定日志口径：
//  1. 已接单/进行中仍算接单动作成功（可重复核验），但摘要必须明确「任务未完成」；
//  2. 已完成/已领取才允许说任务完成。
func TestGrowthAcceptSummarySeparatesAcceptedFromCompleted(t *testing.T) {
	accepted := growthAcceptState(upstream.GrowthTask{AcceptStatus: "accepted"})
	if accepted.Phase != "done" {
		t.Fatalf("accepted must stay a terminal no-op, got phase %q", accepted.Phase)
	}
	if !strings.Contains(accepted.Summary, "未完成") {
		t.Fatalf("accepted summary must say the task itself is not finished: %q", accepted.Summary)
	}
	inProgress := growthAcceptState(upstream.GrowthTask{AcceptStatus: "in_progress"})
	if inProgress.Phase != "done" || !strings.Contains(inProgress.Summary, "未完成") {
		t.Fatalf("in_progress must keep the not-finished wording: %+v", inProgress)
	}
	completed := growthAcceptState(upstream.GrowthTask{AcceptStatus: "completed"})
	if completed.Phase != "done" || strings.Contains(completed.Summary, "未完成") {
		t.Fatalf("completed task must not claim it is unfinished: %+v", completed)
	}
}

// 接单任务再次运行时只读核验，不应把「没有新提交」说成「已提交」。
func TestGrowthAcceptReadyKeepsTheWorkInstruction(t *testing.T) {
	ready := growthAcceptState(upstream.GrowthTask{AcceptStatus: "not_accepted"})
	if ready.Phase != "ready" || !strings.Contains(ready.Summary, "官方客户端") {
		t.Fatalf("ready summary must keep the client instruction: %+v", ready)
	}
}

// 日志结尾必须能区分「已核验完成」和「动作成功但任务仍需人工」两种运行。
func TestRunStatusTextIsExplicit(t *testing.T) {
	if got := taskengine.LogStatusText(taskengine.StatusSucceeded); got != "本次操作已核验" {
		t.Fatalf("succeeded status text = %q", got)
	}
}
