package taskengine

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// TaskLogResult 是一次「任务输出日志」查询的结果。
//
// 与旧调度器的同名结构保持同一语义：任务正在跑就给当前这一轮（日志还在长），
// 空闲时给最近一次跑完的那一轮。区别只在于行的来源——新引擎没有脚本 stdout，
// 日志由这次 run 的持久化执行记录（每个阶段的账号、状态、安全摘要）渲染成文本，
// 面板因此能像旧版一样看到「这一次到底做了什么」。
type TaskLogResult struct {
	Lines   []string
	Running bool
	EndedAt time.Time
	// Message 说明为什么没有行可显示（从未执行、任务名不匹配等）。
	Message string
}

// TaskLog 返回某个任务最近一次（或正在进行的）执行输出。
func (e *Engine) TaskLog(key string) (TaskLogResult, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 128 {
		return TaskLogResult{}, ErrInvalid
	}
	runs := e.Runs()
	running := -1
	latest := -1
	for i := range runs {
		if !taskLogMatches(key, runs[i].TaskKey) {
			continue
		}
		if latest < 0 {
			latest = i
		}
		if runs[i].Status == StatusRunning || runs[i].Status == StatusQueued {
			running = i
			break
		}
	}
	chosen := latest
	if running >= 0 {
		chosen = running
	}
	if chosen < 0 {
		return TaskLogResult{Message: "这个任务还没有运行记录。点「运行」后，输出会出现在这里。"}, nil
	}
	r := runs[chosen]
	out := TaskLogResult{Running: r.Status == StatusRunning || r.Status == StatusQueued}
	if r.FinishedAt != nil {
		out.EndedAt = *r.FinishedAt
	}
	out.Lines = taskLogLines(r)
	return out, nil
}

// taskLogMatches 接受精确任务键与「模块/」前缀计划键两个方向：计划行用
// "growth/" 表示整个模块，而一次手动执行可能记成 "growth/accept/<码>"。
// 只要一方覆盖另一方，就属于同一条流水线。
func taskLogMatches(pattern, key string) bool {
	if pattern == key {
		return true
	}
	return TaskKeyMatches(pattern, key) || TaskKeyMatches(key, pattern)
}

var logStageText = map[Stage]string{
	StageDiscover: "发现任务",
	StageInspect:  "检查状态",
	StageExecute:  "执行",
	StageClaim:    "领取奖励",
	StageVerify:   "复核",
}

var logStatusText = map[Status]string{
	StatusQueued:    "排队中",
	StatusRunning:   "执行中",
	StatusSucceeded: "成功",
	StatusFailed:    "失败",
	StatusCanceled:  "已取消",
	StatusUncertain: "结果未确认",
	StatusSkipped:   "已跳过",
	StatusManual:    "需要人工",
}

func logStatus(status Status) string {
	if text, ok := logStatusText[status]; ok {
		return text
	}
	return string(status)
}

func logTrigger(trigger string) string {
	switch trigger {
	case "manual":
		return "手动触发"
	case "schedule":
		return "按计划触发"
	}
	if trigger == "" {
		return "触发"
	}
	return trigger
}

// shortAccount is the account label a log line carries, in the shape the old
// task scripts printed: enough to tell accounts apart, not the full identifier.
// A uuid keeps its first segment; an AutoClaw id is a phone number, so it is
// masked instead of printed.
func shortAccount(id string) string {
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, "autoclaw-") {
		phone := strings.TrimPrefix(id, "autoclaw-")
		if len(phone) == 11 {
			return "autoclaw " + phone[:3] + "****" + phone[7:]
		}
		return "autoclaw"
	}
	if len(id) <= 8 {
		return id
	}
	if i := strings.IndexByte(id, '-'); i >= 6 {
		return id[:i]
	}
	return id[:8]
}

// stepLine renders one step the way the old scripts did: a time, an account
// tag, and one human sentence. Stage and status are not columns to read — they
// only appear when they add something the sentence does not already say.
func stepLine(stamp func(time.Time) string, s Step) string {
	var b strings.Builder
	b.WriteString(stamp(s.StartedAt))
	b.WriteString("  ")
	if tag := shortAccount(s.AccountID); tag != "" {
		b.WriteString("[")
		b.WriteString(tag)
		b.WriteString("] ")
	}
	text := strings.TrimSpace(s.Summary)
	switch s.Status {
	case StatusSucceeded:
		// Nothing to announce: the sentence itself says what happened.
	case StatusSkipped:
		if text == "" {
			return b.String() + "本次条件不满足，跳过"
		}
		return b.String() + text + "（已跳过）"
	case StatusManual:
		if text == "" {
			return b.String() + "需要人工处理"
		}
		return b.String() + text + "（需要人工）"
	case StatusUncertain:
		if text == "" {
			text = "操作结果尚未确认，请先核验，不要重复提交"
		}
		return b.String() + text + "（结果未确认）"
	case StatusCanceled:
		return b.String() + "已取消"
	case StatusFailed:
		if text == "" {
			text = "未完成"
		}
		return b.String() + text
	}
	if text == "" {
		// A step with no summary still needs to say something readable; the
		// stage is the only fact left, and only as a fallback.
		if stage, ok := logStageText[s.Stage]; ok {
			text = stage
		} else {
			text = "已完成"
		}
	}
	return b.String() + text
}

// taskLogLines renders one run as the plain output the old task scripts
// produced: a header, then one short sentence per account.
func taskLogLines(r Run) []string {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		location = time.UTC
	}
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return "--:--:--"
		}
		return t.In(location).Format("15:04:05")
	}
	lines := []string{
		fmt.Sprintf("%s 开始执行（%s）", stamp(r.StartedAt), logTrigger(r.Trigger)),
	}
	if r.Status == StatusQueued {
		lines = append(lines, "等待执行槽位；开始后会继续追加输出。")
		return lines
	}
	steps := append([]Step{}, r.Steps...)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].StartedAt.Before(steps[j].StartedAt) })
	for _, s := range steps {
		lines = append(lines, stepLine(stamp, s))
	}
	if r.FinishedAt != nil {
		lines = append(lines, fmt.Sprintf("%s 结束，耗时 %s，%s",
			stamp(*r.FinishedAt), r.FinishedAt.Sub(r.StartedAt).Round(time.Second), logStatus(r.Status)))
	} else if r.Status != StatusRunning {
		lines = append(lines, "结果："+logStatus(r.Status))
	}
	if r.Summary != "" {
		lines = append(lines, r.Summary)
	}
	// 与执行记录本身的 2000 步上限同量级；日志是给人看的，再多也没有价值。
	if len(lines) > 2000 {
		lines = append(lines[:1999], fmt.Sprintf("…另有 %d 行未显示", len(lines)-1999))
	}
	return lines
}
