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

// taskLogLines 把一次执行记录渲染成逐行文本。每行以本地时间开头，和旧版脚本
// 日志的观感一致；阶段、账号、结果、摘要都在同一行，便于直接复制给他人核对。
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
		fmt.Sprintf("%s 开始 · %s · %s", stamp(r.StartedAt), logTrigger(r.Trigger), logStatus(r.Status)),
	}
	if r.FinishedAt != nil {
		lines = append(lines, fmt.Sprintf("%s 结束 · 耗时 %s", stamp(*r.FinishedAt), r.FinishedAt.Sub(r.StartedAt).Round(time.Second)))
	}
	if r.Status == StatusQueued {
		lines = append(lines, "等待执行槽位；开始后会继续追加输出。")
		return lines
	}
	if len(r.Steps) == 0 {
		lines = append(lines, "本次执行还没有步骤记录。")
	}
	steps := append([]Step{}, r.Steps...)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].StartedAt.Before(steps[j].StartedAt) })
	for _, s := range steps {
		parts := []string{stamp(s.StartedAt)}
		if stage, ok := logStageText[s.Stage]; ok {
			parts = append(parts, stage)
		} else if s.Stage != "" {
			parts = append(parts, string(s.Stage))
		}
		if s.AccountID != "" {
			parts = append(parts, "账号 "+s.AccountID)
		}
		parts = append(parts, logStatus(s.Status))
		if s.Summary != "" {
			parts = append(parts, "· "+s.Summary)
		} else if s.ErrorCode != "" {
			parts = append(parts, "· "+s.ErrorCode)
		}
		lines = append(lines, strings.Join(parts, " | "))
	}
	if r.Summary != "" {
		lines = append(lines, "总结 · "+r.Summary)
	}
	// 与执行记录本身的 2000 步上限同量级；日志是给人看的，再多也没有价值。
	if len(lines) > 2000 {
		lines = append(lines[:1999], fmt.Sprintf("…另有 %d 行未显示", len(lines)-1999))
	}
	return lines
}
