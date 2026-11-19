package taskengine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

// 任务日志必须来自真实执行记录，并按旧版脚本的写法渲染成一行一句人话：
// 时间 + 账号标签 + 摘要。阶段/状态不再作为列宽固定出现在每行（用户明确
// 反馈那种「检查状态 | 账号 uuid | 成功」的表格看不懂）。
func TestTaskLogRendersRunSteps(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	task := syntheticTask("fake", "account-a", "growth/accept/1")
	task.SideEffect = false
	a := singleAdapter(task)
	a.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) {
		return taskengine.Observation{Phase: "done", Summary: "官方状态已确认完成。"}, nil
	}
	registerBehavior(t, e, "fake", a)
	r := awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", TaskKey: "growth/"}).ID)
	res, err := e.TaskLog("growth/")
	if err != nil {
		t.Fatal(err)
	}
	if res.Running {
		t.Fatal("finished run reported as running")
	}
	if res.EndedAt.IsZero() {
		t.Error("ended time not reported")
	}
	text := strings.Join(res.Lines, "\n")
	for _, want := range []string{"开始执行", "结束", "官方状态已确认完成。", "[account]"} {
		if !strings.Contains(text, want) {
			t.Errorf("log missing %q: %s", want, text)
		}
	}
	// The old version never printed a stage column or a full account uuid;
	// both are what made the newer dialog unreadable.
	for _, unwanted := range []string{"检查状态", "账号 account-a", " | "} {
		if strings.Contains(text, unwanted) {
			t.Errorf("log reintroduced %q: %s", unwanted, text)
		}
	}
	if r.Status != taskengine.StatusSucceeded {
		t.Fatalf("fixture run did not succeed: %s", r.Status)
	}
}

// 模块前缀计划键与具体任务键双向匹配：计划行是 "growth/"，执行记录可能是
// "growth/accept/<码>"。匹配不上会让计划行永远看不到自己的日志。
func TestTaskLogMatchesModulePrefixInBothDirections(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	task := syntheticTask("fake", "account-a", "growth/accept/20261101")
	task.SideEffect = false
	registerBehavior(t, e, "fake", singleAdapter(task))
	awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", TaskKey: "growth/accept/20261101"}).ID)
	res, err := e.TaskLog("growth/")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) == 0 {
		t.Fatal("module plan did not find its child task log")
	}
}

// 从未执行过的任务返回可读说明，而不是错误或空数组：面板据此写「还没有运行记录」。
func TestTaskLogExplainsAnUnrunTask(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	res, err := e.TaskLog("checkin")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 0 || res.Message == "" {
		t.Fatalf("unrun task produced %v / %q", res.Lines, res.Message)
	}
	if res.Running {
		t.Fatal("unrun task reported as running")
	}
}

// 未知或过长的键必须被拒绝，不能退化成「返回全部」或读越界。
func TestTaskLogRejectsInvalidKeys(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	if _, err := e.TaskLog(""); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := e.TaskLog(strings.Repeat("x", 129)); err == nil {
		t.Fatal("oversized key accepted")
	}
}

// 最近一次执行优先：同一任务跑两次时，返回第二次记录，而不是第一次的残留。
func TestTaskLogReturnsTheMostRecentRun(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	task := syntheticTask("fake", "account-a", "checkin")
	task.SideEffect = false
	task.Once, task.OncePerDay = false, false
	n := 0
	a := singleAdapter(task)
	a.inspect = func(context.Context, taskengine.Task) (taskengine.Observation, error) {
		n++
		summary := "第一次执行。"
		if n > 1 {
			summary = "第二次执行。"
		}
		return taskengine.Observation{Phase: "done", Summary: summary}, nil
	}
	registerBehavior(t, e, "fake", a)
	awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", TaskKey: "checkin"}).ID)
	// 确保两次 started_at 可区分（引擎的时间戳有纳秒精度，这里只是稳妥）。
	time.Sleep(2 * time.Millisecond)
	awaitBehaviorRun(t, e, submitBehavior(t, e, taskengine.Request{ProviderID: "fake", TaskKey: "checkin"}).ID)
	res, err := e.TaskLog("checkin")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(res.Lines, "\n")
	if !strings.Contains(text, "第二次执行。") {
		t.Fatalf("log did not return the most recent run:\n%s", text)
	}
}
