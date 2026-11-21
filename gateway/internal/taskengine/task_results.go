package taskengine

import (
	"context"
	"errors"
	"unicode/utf8"
)

const maxTaskResults = 1000

type taskResultIdentity struct {
	account, task, period string
}

func resultIdentity(r TaskResult) taskResultIdentity {
	return taskResultIdentity{r.AccountID, r.TaskKey, r.PeriodKey}
}

func terminalTaskResult(status Status) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusUncertain, StatusSkipped, StatusManual:
		return true
	default:
		return false
	}
}

func normalizeTaskResult(status Status, err error) Status {
	if err == nil {
		return status
	}
	status = mergeStatus(status, StatusFailed)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = StatusCanceled
	}
	if errors.Is(err, ErrUncertain) {
		status = StatusUncertain
	}
	return status
}

func validTaskResult(run Run, result TaskResult) bool {
	return IDPattern.MatchString(result.AccountID) && len(result.TaskKey) > 0 && len(result.TaskKey) <= 512 && utf8.ValidString(result.TaskKey) && len(result.PeriodKey) <= 512 && utf8.ValidString(result.PeriodKey) && terminalTaskResult(result.Status) && taskIncluded(run.TaskKey, run.TaskSequence, result.TaskKey) && (len(run.AccountIDs) == 0 || contains(run.AccountIDs, result.AccountID)) && (run.ExpectedPeriodKey == "" || result.PeriodKey == run.ExpectedPeriodKey)
}

// recordTaskResult is best effort and never touches action evidence. The caller
// must return its original business outcome even when this projection cannot be
// written. An existing result can be reconciled; the bound never evicts evidence.
func (e *Engine) recordTaskResult(runID string, task Task, status Status) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, pending := e.state.Uncertain[taskKey(task)]; pending {
		status = StatusUncertain
	}
	if !terminalTaskResult(status) {
		return nil
	}
	for _, run := range e.state.Runs {
		if run.ID != runID {
			continue
		}
		if !run.TaskResultTracking {
			return nil
		}
		result := TaskResult{AccountID: task.AccountID, TaskKey: task.Key, PeriodKey: task.PeriodKey, Status: status}
		if !validTaskResult(run, result) {
			return ErrInvalid
		}
		index := -1
		for i, old := range run.TaskResults {
			if resultIdentity(old) == resultIdentity(result) {
				index = i
				if old == result {
					return nil
				}
				break
			}
		}
		if index < 0 && len(run.TaskResults) >= maxTaskResults {
			return nil
		}
		return e.commit(func(state *persisted) {
			for i := range state.Runs {
				if state.Runs[i].ID == runID {
					if index >= 0 {
						state.Runs[i].TaskResults[index] = result
					} else {
						state.Runs[i].TaskResults = append(state.Runs[i].TaskResults, result)
					}
					return
				}
			}
		})
	}
	return ErrNotFound
}

func validateTaskResults(state persisted) error {
	for _, run := range state.Runs {
		if len(run.TaskResults) > maxTaskResults {
			return errors.New("task_result_limit")
		}
		seen := map[taskResultIdentity]bool{}
		for _, result := range run.TaskResults {
			identity := resultIdentity(result)
			if !validTaskResult(run, result) || seen[identity] {
				return errors.New("task_result_invalid")
			}
			seen[identity] = true
		}
	}
	return nil
}
