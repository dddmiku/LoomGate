package taskengine

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// TaskLogResult 是一次「任务输出日志」查询的结果。
//
// 与旧调度器保持同一语义：执行中返回当前输出，空闲时返回最近一轮。
// 当前运行的模块输出按行持久化；历史只有 Steps 的运行仍可读，但不伪造实时输出。
// 日志与动作回执独立，任何输出文本都不构成任务成功证明。
type TaskLogResult struct {
	Lines   []string
	Running bool
	EndedAt time.Time
	// Message 说明为什么没有行可显示（从未执行、任务名不匹配等）。
	Message string
}

// TaskLog 返回某个任务最近一次（或正在进行的）执行输出。
func (e *Engine) TaskLog(key string) (TaskLogResult, error) {
	return e.TaskLogFor("", key)
}

// TaskLogFor isolates identically named task families across providers. The old
// unscoped API remains readable for legacy callers; the console always scopes it.
func (e *Engine) TaskLogFor(provider, key string) (TaskLogResult, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 128 || provider != "" && !IDPattern.MatchString(provider) {
		return TaskLogResult{}, ErrInvalid
	}
	runs := e.Runs()
	running := -1
	latest := -1
	for i := range runs {
		if !runIncludesLogKey(runs[i], key) || provider != "" && runs[i].ProviderID != provider {
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
	// A broad run may contain this task, but its sibling task output is not this
	// task's log. Filter only the presentation copy, never the durable run/work.
	steps := []Step{}
	for _, step := range r.Steps {
		if key == r.TaskKey || step.TaskKey == "" || TaskKeyMatches(key, step.TaskKey) {
			steps = append(steps, step)
		}
	}
	r.Steps = steps
	output := []OutputLine{}
	for _, line := range r.Output {
		if key == r.TaskKey || TaskKeyMatches(key, line.TaskKey) {
			output = append(output, line)
		}
	}
	r.Output = output
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
	StatusSucceeded: "本次操作已核验",
	StatusFailed:    "失败",
	StatusCanceled:  "已取消",
	StatusUncertain: "结果未确认",
	StatusSkipped:   "已跳过",
	StatusManual:    "需要人工",
}

// LogStatusText 是日志结尾状态文案的只读入口（供跨包回归固定口径）。
func LogStatusText(status Status) string { return logStatus(status) }

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
	if len(id) >= 7 && len(id) <= 15 {
		numeric := true
		for _, r := range id {
			if r < '0' || r > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			left, right := 3, 4
			if len(id) < 11 {
				left, right = 1, 1
			}
			return id[:left] + strings.Repeat("*", len(id)-left-right) + id[len(id)-right:]
		}
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
	if name := safeOutputText(s.TaskName, 256); name != "" {
		b.WriteString("「" + name + "」 ")
	} else if key := safeOutputText(s.TaskKey, 256); key != "" {
		b.WriteString("「" + key + "」 ")
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
	if n := len(r.AccountIDs); n > 0 {
		lines = append(lines, fmt.Sprintf("账号范围：%d 个账号；每行一个账号，按完成顺序排列。", n))
	}
	if r.Status == StatusQueued {
		lines = append(lines, "等待执行槽位；开始后会继续追加输出。")
		return lines
	}
	type entry struct {
		at   time.Time
		line string
	}
	entries := []entry{}
	for _, s := range r.Steps {
		at := s.StartedAt
		if s.FinishedAt != nil {
			at = *s.FinishedAt
		}
		s.StartedAt = at
		entries = append(entries, entry{at, stepLine(stamp, s)})
	}
	for _, o := range r.Output {
		prefix := stamp(o.At) + "  [" + shortAccount(o.AccountID) + "] "
		if name := safeOutputText(o.TaskName, 256); name != "" {
			prefix += "「" + name + "」 "
		} else {
			prefix += "「" + safeOutputText(o.TaskKey, 256) + "」 "
		}
		entries = append(entries, entry{o.At, prefix + safeOutputText(o.Text, maxOutputLine)})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	if r.OutputDropped > 0 {
		lines = append(lines, fmt.Sprintf("较早的 %d 行进度已超出保留上限；操作回执和待核验状态保留。", r.OutputDropped))
	}
	for _, row := range entries {
		lines = append(lines, row.line)
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
