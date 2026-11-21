package taskengine

import (
	"context"
	"errors"
	"sort"
	"time"
)

func sequenceAccounts(ctx context.Context, e *Engine, run Run, adapter Adapter) (accounts []string, err error) {
	defer func() {
		if recover() != nil {
			accounts, err = nil, errors.New("adapter_panic")
		}
	}()
	bounded, cancel := context.WithTimeout(ctx, e.opts.Timeout)
	defer cancel()
	if source, ok := adapter.(SequenceAccountAdapter); ok {
		accounts, err = source.SequenceAccounts(bounded, append([]string{}, run.AccountIDs...), append([]string{}, run.TaskSequence...))
	} else {
		catalog, readErr := e.CatalogDetails(bounded, run.ProviderID, Scope{AccountIDs: run.AccountIDs}, "")
		err = readErr
		if err == nil && len(catalog.Issues) > 0 {
			err = errors.New("partial_discovery_failed")
		}
		set := map[string]bool{}
		for _, task := range catalog.Tasks {
			set[task.AccountID] = true
		}
		for id := range set {
			accounts = append(accounts, id)
		}
	}
	if err != nil {
		return nil, err
	}
	if len(accounts) > 500 {
		return nil, ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range accounts {
		if !IDPattern.MatchString(id) || seen[id] || len(run.AccountIDs) > 0 && !contains(run.AccountIDs, id) {
			return nil, ErrInvalid
		}
		seen[id] = true
	}
	if len(run.AccountIDs) > 0 {
		if !sameAccountScope(accounts, run.AccountIDs) {
			return nil, ErrInvalid
		}
		return append([]string{}, run.AccountIDs...), bounded.Err()
	}
	accounts = append([]string{}, accounts...)
	sort.Strings(accounts)
	return accounts, bounded.Err()
}

// The callback changes the cursor and its current-stage proof in the same disk
// commit. runTask may have confirmed a repeatable item since the local snapshot;
// always take that proof from the durable state before applying a transition.
func (e *Engine) saveSequenceWork(id string, work *runWork, change func(*runWork), issues []DiscoveryIssue) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	current, ok := e.state.Work[id]
	if !ok {
		return ErrNotFound
	}
	next := cloneWork(*work)
	next.Confirmed = current.Confirmed
	next.CancelRequested = current.CancelRequested
	change(&next)
	err := e.commit(func(state *persisted) {
		// Keep local cursor maps detached from the published engine state.
		state.Work[id] = cloneWork(next)
		for i := range state.Runs {
			if state.Runs[i].ID == id {
				remaining := 1000 - len(state.Runs[i].DiscoveryIssues)
				if remaining > 0 {
					state.Runs[i].DiscoveryIssues = append(state.Runs[i].DiscoveryIssues, issues[:min(len(issues), remaining)]...)
				}
				break
			}
		}
	})
	if err == nil {
		*work = next
	}
	return err
}

func clearSequenceStage(work *runWork) {
	work.Tasks, work.Next, work.SequenceIndex, work.Confirmed = nil, 0, 0, nil
}

// Families are discovered just in time for one account at a time. Each account
// owns a durable checkpoint: unknown blocks its dependent writes but not another
// account, and re-triggering restores only unfinished checkpoints in this run.
func (e *Engine) performSequence(ctx context.Context, run Run, adapter Adapter, work runWork) (Status, string) {
	if work.SequenceAccounts == nil {
		accounts, err := sequenceAccounts(ctx, e, run, adapter)
		if err != nil {
			return StatusFailed, "discovery_failed"
		}
		if err = e.saveSequenceWork(run.ID, &work, func(w *runWork) { w.SequenceAccounts = accounts }, nil); err != nil {
			return StatusFailed, "storage_failed"
		}
	}
	// Finish a crash-interrupted active account before loading older parked
	// checkpoints, so restoring one cannot overwrite another account's progress.
	order := make([]int, 0, len(work.SequenceAccounts))
	if work.SequenceAccount < len(work.SequenceAccounts) {
		order = append(order, work.SequenceAccount)
	}
	for index := range work.SequenceAccounts {
		if index != work.SequenceAccount {
			order = append(order, index)
		}
	}
	for _, index := range order {
		if ctx.Err() != nil {
			if len(work.SequenceHolds) > 0 {
				return StatusUncertain, "verification_required"
			}
			return StatusCanceled, "task_canceled"
		}
		account := work.SequenceAccounts[index]
		if work.SequenceDone[account] {
			continue
		}
		if work.SequenceAccount != index || work.SequenceHolds[account].Tasks != nil {
			if err := e.saveSequenceWork(run.ID, &work, func(w *runWork) {
				clearSequenceStage(w)
				w.SequenceAccount = index
				if hold, exists := w.SequenceHolds[account]; exists {
					w.Tasks, w.Next, w.SequenceIndex, w.Confirmed = hold.Tasks, hold.Next, hold.Index, hold.Confirmed
					delete(w.SequenceHolds, account)
				}
			}, nil); err != nil {
				return StatusUncertain, "storage_failed"
			}
		}
		result, code := e.performSequenceAccount(ctx, run, adapter, &work)
		if code == "storage_failed" {
			return StatusUncertain, code
		}
		if ctx.Err() != nil {
			if result == StatusUncertain || len(work.SequenceHolds) > 0 {
				return StatusUncertain, "verification_required"
			}
			return StatusCanceled, "task_canceled"
		}
		failedStage := work.SequenceIndex
		if err := e.saveSequenceWork(run.ID, &work, func(w *runWork) {
			if result == StatusUncertain {
				if w.SequenceHolds == nil {
					w.SequenceHolds = map[string]sequenceCheckpoint{}
				}
				w.SequenceHolds[account] = sequenceCheckpoint{Tasks: w.Tasks, Next: w.Next, Index: w.SequenceIndex, Confirmed: w.Confirmed}
			} else {
				w.Status = mergeStatus(w.Status, result)
				if code != "" {
					w.ErrorCode = code
				}
				if w.SequenceDone == nil {
					w.SequenceDone = map[string]bool{}
				}
				w.SequenceDone[account] = true
			}
			clearSequenceStage(w)
			w.SequenceAccount = len(w.SequenceAccounts)
		}, nil); err != nil {
			return StatusUncertain, "storage_failed"
		}
		if result == StatusUncertain || result == StatusFailed {
			// A failed sign-in must not hide the independent balance read. Only
			// Inspect is allowed here; never call Execute/Claim or confirm a
			// pending write based on this diagnostic tail.
			e.inspectSequenceTail(ctx, run, adapter, &work, account, failedStage+1)
		}
	}
	if len(work.SequenceHolds) > 0 {
		return StatusUncertain, "verification_required"
	}
	if work.SequenceCount == 0 && work.Status == StatusSkipped {
		return StatusSkipped, "no_matching_tasks"
	}
	return work.Status, work.ErrorCode
}

func (e *Engine) performSequenceAccount(ctx context.Context, run Run, adapter Adapter, work *runWork) (Status, string) {
	account := work.SequenceAccounts[work.SequenceAccount]
	accountStatus := StatusSkipped
	for stage := work.SequenceIndex; stage < len(run.TaskSequence); stage++ {
		if ctx.Err() != nil {
			return StatusCanceled, "task_canceled"
		}
		if work.Tasks == nil {
			catalog, err := e.CatalogDetails(ctx, run.ProviderID, Scope{AccountIDs: []string{account}}, run.TaskSequence[stage])
			if err != nil {
				if saveErr := e.saveSequenceWork(run.ID, work, func(w *runWork) {
					w.Status, w.ErrorCode = mergeStatus(w.Status, StatusFailed), "discovery_failed"
				}, nil); saveErr != nil {
					return StatusUncertain, "storage_failed"
				}
				return StatusFailed, "discovery_failed"
			}
			tasks := []Task{}
			for _, task := range catalog.Tasks {
				if TaskKeyMatches(run.TaskSequence[stage], task.Key) {
					task.State = nil
					tasks = append(tasks, task)
				}
			}
			if len(tasks) > 1000-work.SequenceCount {
				return StatusFailed, "catalog_limit_exceeded"
			}
			if err = e.saveSequenceWork(run.ID, work, func(w *runWork) {
				w.Tasks, w.Next, w.SequenceIndex, w.Confirmed = tasks, 0, stage, nil
				w.SequenceCount += len(tasks)
				if len(catalog.Issues) > 0 {
					w.Status, w.ErrorCode = mergeStatus(w.Status, StatusFailed), "partial_discovery_failed"
				}
			}, catalog.Issues); err != nil {
				return StatusUncertain, "storage_failed"
			}
			if len(catalog.Issues) > 0 {
				return StatusFailed, "partial_discovery_failed"
			}
		}
		for index := work.Next; index < len(work.Tasks); index++ {
			task := work.Tasks[index]
			result, err := e.runAdmittedTask(ctx, run.ID, task, adapter)
			if err != nil {
				result = mergeStatus(result, StatusFailed)
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					result = StatusCanceled
				}
				if errors.Is(err, ErrUncertain) {
					result = StatusUncertain
				}
			}
			e.mu.Lock()
			_, pending := e.state.Uncertain[taskKey(task)]
			e.mu.Unlock()
			if pending || result == StatusUncertain {
				return StatusUncertain, "verification_required"
			}
			accountStatus = mergeStatus(accountStatus, result)
			if ctx.Err() != nil {
				return StatusCanceled, "task_canceled"
			}
			if saveErr := e.saveSequenceWork(run.ID, work, func(w *runWork) {
				w.Next = index + 1
				w.Status = mergeStatus(w.Status, result)
				if err != nil {
					w.ErrorCode = "task_failed"
				}
			}, nil); saveErr != nil {
				return StatusUncertain, "storage_failed"
			}
			if result == StatusCanceled {
				return StatusCanceled, "task_canceled"
			}
			if result == StatusFailed {
				return StatusFailed, "task_failed"
			}
		}
		if stage+1 < len(run.TaskSequence) {
			if err := e.saveSequenceWork(run.ID, work, func(w *runWork) {
				clearSequenceStage(w)
				w.SequenceIndex = stage + 1
			}, nil); err != nil {
				return StatusUncertain, "storage_failed"
			}
		}
	}
	return accountStatus, ""
}

func (e *Engine) inspectSequenceTail(ctx context.Context, run Run, adapter Adapter, work *runWork, account string, start int) {
	reads := 0
	for _, selector := range run.TaskSequence[start:] {
		if ctx.Err() != nil || reads >= 64 || work.SequenceCount >= 1000 {
			return
		}
		catalog, err := e.CatalogDetails(ctx, run.ProviderID, Scope{AccountIDs: []string{account}}, selector)
		if err != nil {
			continue
		}
		for _, task := range catalog.Tasks {
			if task.SideEffect || task.ManualOnly || !task.Idempotent || !TaskKeyMatches(selector, task.Key) {
				continue
			}
			if ctx.Err() != nil || reads >= 64 || work.SequenceCount >= 1000 {
				return
			}
			if err = e.saveSequenceWork(run.ID, work, func(w *runWork) { w.SequenceCount++ }, nil); err != nil {
				return
			}
			reads++
			_, _ = e.withTaskAdmission(ctx, task, adapter, func(bounded context.Context) (Status, error) {
				_, inspectErr := e.record(bounded, run.ID, task, StageInspect, func(stageCtx context.Context) (Observation, error) { return adapter.Inspect(stageCtx, task) })
				return StatusSkipped, inspectErr
			})
		}
	}
}

// Ordinary tasks, sequence items and read-only diagnostic tails share account
// isolation, cancellation, admission limits and a per-task request timeout.
func (e *Engine) runAdmittedTask(ctx context.Context, runID string, task Task, adapter Adapter) (status Status, err error) {
	defer func() {
		// A diagnostic write must never change the action result, completion
		// ledger or retry behavior. Missing results remain visibly unclassified.
		_ = e.recordTaskResult(runID, task, normalizeTaskResult(status, err))
	}()
	e.mu.Lock()
	confirmed := e.state.Work[runID].Confirmed[taskKey(task)]
	e.mu.Unlock()
	if confirmed {
		return StatusSucceeded, nil
	}
	return e.withTaskAdmission(ctx, task, adapter, func(bounded context.Context) (Status, error) { return e.runTask(bounded, runID, task, adapter) })
}

func (e *Engine) withTaskAdmission(ctx context.Context, task Task, adapter Adapter, perform func(context.Context) (Status, error)) (Status, error) {
	release, err := e.accountLock(ctx, task)
	if err != nil {
		return StatusCanceled, err
	}
	defer release()
	if gate, ok := adapter.(TaskGate); ok {
		gateCtx, gateCancel := context.WithTimeout(ctx, 64*(e.opts.Timeout+10*time.Second))
		free, gateErr := safeGate(gateCtx, gate, task)
		gateCancel()
		if free != nil {
			defer free()
		}
		if gateErr != nil {
			return StatusFailed, gateErr
		}
	}
	select {
	case e.workers <- struct{}{}:
		defer func() { <-e.workers }()
	case <-ctx.Done():
		return StatusCanceled, ctx.Err()
	}
	budget := e.opts.Timeout
	if e.opts.AllowTaskTimeout && task.TimeoutSeconds > 0 && task.TimeoutSeconds <= 900 {
		budget = time.Duration(task.TimeoutSeconds) * time.Second
	}
	bounded, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return perform(bounded)
}
