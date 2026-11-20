package taskengine

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxOutputInput    = 16 << 10
	maxOutputLine     = 1024
	maxOutputLines    = 512
	maxRunOutputBytes = 128 << 10
	maxAllOutputBytes = 2 << 20
)

var errOutputLimit = errors.New("task_output_limit")

// Output writes operator progress for the current task stage. The engine binds
// its destination; modules cannot choose another run, account or task identity.
// Output is informational only and never proves submission or completion.
func Output(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sink, ok := ctx.Value(outputContextKey{}).(func(string) error); ok {
		return sink(text)
	}
	// Modules are also used directly without an engine (for example discovery
	// fixtures); absence of an output observer must not change their business API.
	return nil
}

type outputContextKey struct{}

func outputStageText(stage Stage) string {
	switch stage {
	case StageInspect:
		return "正在读取官方任务状态…"
	case StageExecute:
		return "正在执行当前任务…"
	case StageClaim:
		return "正在领取符合条件的奖励…"
	case StageVerify:
		return "正在从官方状态核验结果…"
	default:
		return "正在处理任务…"
	}
}

func safeOutputText(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	text = safeStepSummary(text, "", "")
	if len(text) > limit {
		text = text[:limit-3]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "…"
	}
	return text
}

func outputSize(line OutputLine) int {
	return len(line.Text) + len(line.TaskKey) + len(line.TaskName) + len(line.AccountID) + 160
}

// Bound diagnostic retention separately from receipts/work cursors. Dropping old
// output can never clear an unknown attempt or evict its execution record.
func trimOutput(s *persisted) {
	total := 0
	for i := range s.Runs {
		r := &s.Runs[i]
		size := 0
		for _, line := range r.Output {
			size += outputSize(line)
		}
		for len(r.Output) > maxOutputLines || size > maxRunOutputBytes {
			size -= outputSize(r.Output[0])
			r.Output = r.Output[1:]
			r.OutputDropped++
		}
		total += size
	}
	for i := range s.Runs {
		r := &s.Runs[i]
		for total > maxAllOutputBytes && len(r.Output) > 0 {
			total -= outputSize(r.Output[0])
			r.Output = r.Output[1:]
			r.OutputDropped++
		}
	}
}

func validateOutput(s persisted) error {
	total := 0
	for _, run := range s.Runs {
		if len(run.Output) > maxOutputLines || run.OutputDropped < 0 {
			return errors.New("task_output_invalid")
		}
		size := 0
		for _, line := range run.Output {
			if line.At.IsZero() || !IDPattern.MatchString(line.AccountID) || line.TaskKey == "" || len(line.TaskKey) > 512 || !taskIncluded(run.TaskKey, run.TaskSequence, line.TaskKey) || len(run.AccountIDs) > 0 && !contains(run.AccountIDs, line.AccountID) || line.Text == "" || len(line.Text) > maxOutputLine || len(line.TaskName) > 256 {
				return errors.New("task_output_invalid")
			}
			switch line.Stage {
			case StageInspect, StageExecute, StageClaim, StageVerify:
			default:
				return errors.New("task_output_invalid")
			}
			if safeOutputText(line.Text, maxOutputLine) != line.Text || safeOutputText(line.TaskName, 256) != line.TaskName {
				return errors.New("task_output_invalid")
			}
			size += outputSize(line)
		}
		if size > maxRunOutputBytes {
			return errors.New("task_output_limit")
		}
		total += size
	}
	if total > maxAllOutputBytes {
		return errOutputLimit
	}
	return nil
}

func (e *Engine) outputContext(ctx context.Context, id string, t Task, stage Stage) (context.Context, func()) {
	active := true // guarded by the engine mutex, including detached writers
	sink := func(text string) error {
		if len(text) > maxOutputInput || strings.Count(text, "\n") > 32 {
			return errOutputLimit
		}
		lines := []string{}
		for _, line := range strings.Split(text, "\n") {
			if safe := safeOutputText(line, maxOutputLine); safe != "" {
				lines = append(lines, safe)
			}
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if !active || e.closed {
			return context.Canceled
		}
		work, exists := e.state.Work[id]
		if !exists || work.CancelRequested {
			return context.Canceled
		}
		index := -1
		for i := range e.state.Runs {
			if r := e.state.Runs[i]; r.ID == id && r.ProviderID == t.ProviderID && r.Status == StatusRunning && r.FinishedAt == nil {
				index = i
				break
			}
		}
		if index < 0 {
			return ErrNotFound
		}
		if len(lines) == 0 {
			return nil
		}
		return e.commit(func(s *persisted) {
			for _, line := range lines {
				s.Runs[index].Output = append(s.Runs[index].Output, OutputLine{At: time.Now().UTC(), AccountID: t.AccountID, TaskKey: t.Key, TaskName: safeOutputText(t.Name, 256), Stage: stage, Text: line})
			}
			trimOutput(s)
		})
	}
	return context.WithValue(ctx, outputContextKey{}, sink), func() {
		e.mu.Lock()
		active = false
		e.mu.Unlock()
	}
}
