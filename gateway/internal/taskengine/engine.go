package taskengine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
	_ "time/tzdata"
)

type Options struct {
	Path        string
	Concurrency int
	Timeout     time.Duration
	PollMin     time.Duration
	PollMax     time.Duration
}
type Engine struct {
	mu       sync.Mutex
	state    persisted
	opts     Options
	owner    *os.File
	adapters map[string]Adapter
	running  map[string]context.CancelFunc
	accounts map[string]chan struct{}
	closed   bool
	workers  chan struct{}
	wake     chan struct{}
	wg       sync.WaitGroup
}

func Open(opts Options) (*Engine, error) {
	if opts.Path == "" {
		return nil, ErrInvalid
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.Concurrency > 32 {
		return nil, ErrInvalid
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Minute
	}
	if opts.PollMin <= 0 {
		opts.PollMin = time.Second
	}
	if opts.PollMax <= 0 {
		opts.PollMax = 15 * time.Second
	}
	owner, s, err := openState(opts.Path)
	if err != nil {
		return nil, err
	}
	return &Engine{state: s, opts: opts, owner: owner, adapters: map[string]Adapter{}, running: map[string]context.CancelFunc{}, accounts: map[string]chan struct{}{}, workers: make(chan struct{}, opts.Concurrency), wake: make(chan struct{}, 1)}, nil
}
func (e *Engine) Register(id string, a Adapter) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || !IDPattern.MatchString(id) || a == nil {
		return ErrInvalid
	}
	if _, ok := e.adapters[id]; ok {
		return ErrInvalid
	}
	e.adapters[id] = a
	for _, r := range e.state.Runs {
		if r.ProviderID == id {
			if _, ok := e.state.Work[r.ID]; ok {
				e.startLocked(r)
			}
		}
	}
	return nil
}
func (e *Engine) Providers() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []string{}
	for id := range e.adapters {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
func (e *Engine) commit(fn func(*persisted)) error {
	next := cloneState(e.state)
	fn(&next)
	if err := writeState(e.opts.Path, next); err != nil {
		return err
	}
	e.state = next
	return nil
}
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	for _, cancel := range e.running {
		cancel()
	}
	e.mu.Unlock()
	e.notify()
	e.wg.Wait()
	return e.owner.Close()
}
func (e *Engine) notify() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func safeCall(fn func() (Observation, error)) (o Observation, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("adapter_panic")
		}
	}()
	return fn()
}

func safeGate(ctx context.Context, g TaskGate, t Task) (release func(), err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("adapter_panic")
		}
	}()
	return g.AcquireTask(ctx, t)
}
func safeDiscover(ctx context.Context, a Adapter, s Scope) (tasks []Task, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("adapter_panic")
		}
	}()
	return a.Discover(ctx, s)
}

// Catalog is always resolved from the registered provider. No captured task IDs
// or model names are compiled into the scheduler.
func (e *Engine) Catalog(ctx context.Context, provider string, scope Scope) ([]Task, error) {
	catalog, err := e.CatalogDetails(ctx, provider, scope, "")
	if err == nil && len(catalog.Issues) > 0 {
		return catalog.Tasks, errors.New("partial_discovery_failed")
	}
	return catalog.Tasks, err
}
func (e *Engine) CatalogDetails(ctx context.Context, provider string, scope Scope, key string) (Catalog, error) {
	e.mu.Lock()
	a := e.adapters[provider]
	closed := e.closed
	e.mu.Unlock()
	if closed || a == nil {
		return Catalog{}, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, e.opts.Timeout)
	defer cancel()
	catalog, err := discoverDetails(ctx, a, scope, key)
	tasks := catalog.Tasks
	if err != nil {
		return Catalog{}, errors.New("discovery_failed")
	}
	if len(tasks) > 1000 || len(catalog.Issues) > 1000 {
		return Catalog{}, errors.New("catalog_limit_exceeded")
	}
	for _, issue := range catalog.Issues {
		if issue.ProviderID != provider || !IDPattern.MatchString(issue.AccountID) || !IDPattern.MatchString(issue.ModuleID) || issue.ErrorCode != "module_discovery_failed" || len(scope.AccountIDs) > 0 && !contains(scope.AccountIDs, issue.AccountID) {
			return Catalog{}, errors.New("invalid_discovery_issue")
		}
	}
	seen := map[string]bool{}
	out := []Task{}
	for _, t := range tasks {
		// Provider discovery cannot manufacture an accepted mutation receipt.
		t.AcceptedStage = ""
		if t.ProviderID != provider || t.Key == "" || len(t.Key) > 512 || len(t.PeriodKey) > 512 || !IDPattern.MatchString(t.AccountID) {
			return Catalog{}, errors.New("invalid_catalog")
		}
		if len(scope.AccountIDs) > 0 && !contains(scope.AccountIDs, t.AccountID) {
			return Catalog{}, errors.New("account_scope_violation")
		}
		if t.Once && t.OncePerDay {
			return Catalog{}, errors.New("invalid_recurrence")
		}
		if t.OncePerDay && t.PeriodKey == "" {
			return Catalog{}, errors.New("business_period_missing")
		}
		key := taskKey(t)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	if catalog.Issues == nil {
		catalog.Issues = []DiscoveryIssue{}
	}
	e.mu.Lock()
	// A changing period/record must not hide an unresolved mutation. Keep its
	// exact non-secret definition available for read-back, including restart.
	for pendingKey, pending := range e.state.PendingTasks {
		if _, ok := e.state.Uncertain[pendingKey]; !ok {
			continue
		}
		if pending.ProviderID != provider || len(scope.AccountIDs) > 0 && !contains(scope.AccountIDs, pending.AccountID) || key != "" && key != "*" && key != pending.Key {
			continue
		}
		found := false
		for i, current := range out {
			if current.AccountID == pending.AccountID && current.Key == pending.Key {
				found = true
				if taskKey(current) != pendingKey {
					out[i] = pending
				} else {
					out[i].AcceptedStage = pending.AcceptedStage
				}
				break
			}
		}
		if !found {
			out = append(out, pending)
		}
	}
	if len(out) > 1000 {
		e.mu.Unlock()
		return Catalog{}, errors.New("catalog_limit_exceeded")
	}
	for i := range out {
		_, out[i].PendingVerification = e.state.Uncertain[taskKey(out[i])]
	}
	e.mu.Unlock()
	return Catalog{Tasks: out, Issues: catalog.Issues}, nil
}
func discoverDetails(ctx context.Context, a Adapter, s Scope, key string) (out Catalog, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("adapter_panic")
		}
	}()
	if detailed, ok := a.(CatalogAdapter); ok {
		return detailed.DiscoverCatalog(ctx, s, key)
	}
	tasks, err := safeDiscover(ctx, a, s)
	return Catalog{Tasks: tasks, Issues: []DiscoveryIssue{}}, err
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func taskKey(t Task) string {
	period := t.PeriodKey
	if t.Once {
		period = "once"
	}
	h := sha256.Sum256([]byte(t.ProviderID + "\x00" + t.AccountID + "\x00" + t.Key + "\x00" + period))
	return hex.EncodeToString(h[:])
}
func randomID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

func (e *Engine) Submit(req Request) (Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.submitLocked(req)
}
func (e *Engine) submitLocked(req Request) (Run, error) {
	return e.submitScheduleLocked(req, nil)
}
func (e *Engine) submitScheduleLocked(req Request, schedule *Schedule) (Run, error) {
	if e.closed || e.adapters[req.ProviderID] == nil || len(req.AccountIDs) > 500 {
		return Run{}, ErrInvalid
	}
	for _, id := range req.AccountIDs {
		if !IDPattern.MatchString(id) {
			return Run{}, ErrInvalid
		}
	}
	if len(e.running) >= 64 || len(e.state.Work) >= 64 {
		return Run{}, ErrBusy
	}
	r := Run{ID: randomID(), ProviderID: req.ProviderID, TaskKey: req.TaskKey, ScheduleID: req.ScheduleID, Trigger: req.Trigger, Status: StatusQueued, StartedAt: time.Now().UTC(), AccountIDs: append([]string{}, req.AccountIDs...), Steps: []Step{}}
	if err := e.commit(func(s *persisted) {
		s.Runs = append(s.Runs, r)
		// Reserve durable work before dispatch; no discovery or action can occur
		// before this intent. Restart rediscovery is safe until Tasks is captured.
		if s.Work == nil {
			s.Work = map[string]runWork{}
		}
		s.Work[r.ID] = runWork{Status: StatusSkipped}
		if schedule != nil {
			schedule.LastRunID = r.ID
			s.Schedules[schedule.ID] = *schedule
		}
		if len(s.Runs) > 1000 {
			for i, x := range s.Runs {
				if x.Status != StatusQueued && x.Status != StatusRunning && x.Status != StatusUncertain {
					s.Runs = append(s.Runs[:i], s.Runs[i+1:]...)
					break
				}
			}
		}
	}); err != nil {
		return Run{}, err
	}
	e.startLocked(r)
	return r, nil
}
func (e *Engine) startLocked(r Run) {
	if e.running[r.ID] != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.running[r.ID] = cancel
	e.wg.Add(1)
	go e.perform(ctx, r)
}
func (e *Engine) updateRun(id string, fn func(*Run)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commit(func(s *persisted) {
		for i := range s.Runs {
			if s.Runs[i].ID == id {
				fn(&s.Runs[i])
				break
			}
		}
	})
}
func (e *Engine) Runs() []Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := cloneState(e.state)
	sort.Slice(s.Runs, func(i, j int) bool { return s.Runs[i].StartedAt.After(s.Runs[j].StartedAt) })
	return s.Runs
}
func (e *Engine) Cancel(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	cancel := e.running[id]
	if cancel == nil {
		return false
	}
	if err := e.commit(func(s *persisted) { w := s.Work[id]; w.CancelRequested = true; s.Work[id] = w }); err != nil {
		return false
	}
	cancel()
	return true
}
func (e *Engine) accountLock(ctx context.Context, t Task) (func(), error) {
	key := t.ProviderID + ":" + t.AccountID
	e.mu.Lock()
	ch := e.accounts[key]
	if ch == nil {
		ch = make(chan struct{}, 1)
		e.accounts[key] = ch
	}
	e.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (e *Engine) perform(ctx context.Context, r Run) {
	defer e.wg.Done()
	defer func() {
		e.mu.Lock()
		if c := e.running[r.ID]; c != nil {
			c()
		}
		delete(e.running, r.ID)
		e.mu.Unlock()
	}()
	status := StatusFailed
	code := ""
	defer func() {
		if recover() != nil {
			status = StatusUncertain
			code = "adapter_panic"
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		_ = e.commit(func(s *persisted) {
			w, ok := s.Work[r.ID]
			for i := range s.Runs {
				x := &s.Runs[i]
				if x.ID != r.ID {
					continue
				}
				if e.closed && ok && !w.CancelRequested {
					x.Status = StatusQueued
					x.ErrorCode = "interrupted_resume"
					return
				}
				if w.CancelRequested && status != StatusUncertain {
					status = StatusCanceled
				}
				now := time.Now().UTC()
				x.FinishedAt = &now
				x.Status = status
				x.ErrorCode = code
			}
			delete(s.Work, r.ID)
		})
	}()
	if err := e.updateRun(r.ID, func(x *Run) { x.Status = StatusRunning }); err != nil {
		code = "storage_failed"
		return
	}
	e.mu.Lock()
	a := e.adapters[r.ProviderID]
	work := cloneState(e.state).Work[r.ID]
	e.mu.Unlock()
	if work.Tasks == nil {
		catalog, err := e.CatalogDetails(ctx, r.ProviderID, Scope{AccountIDs: r.AccountIDs}, r.TaskKey)
		if err != nil {
			code = "discovery_failed"
			return
		}
		work.Tasks = []Task{}
		work.Confirmed = map[string]bool{}
		for _, t := range catalog.Tasks {
			if r.TaskKey == "" || r.TaskKey == "*" || r.TaskKey == t.Key {
				t.State = nil
				work.Tasks = append(work.Tasks, t)
			}
		}
		if len(catalog.Issues) > 0 {
			work.Status = StatusFailed
			work.ErrorCode = "partial_discovery_failed"
		}
		e.mu.Lock()
		err = e.commit(func(s *persisted) {
			current := s.Work[r.ID]
			work.CancelRequested = current.CancelRequested
			s.Work[r.ID] = work
			for i := range s.Runs {
				if s.Runs[i].ID == r.ID {
					s.Runs[i].DiscoveryIssues = catalog.Issues
				}
			}
		})
		e.mu.Unlock()
		if err != nil {
			code = "storage_failed"
			return
		}
	}
	status = work.Status
	code = work.ErrorCode
	for index := work.Next; index < len(work.Tasks); index++ {
		t := work.Tasks[index]
		if ctx.Err() != nil {
			return
		}
		result, err := func() (Status, error) {
			if work.Confirmed[taskKey(t)] {
				return StatusSucceeded, nil
			}
			release, err := e.accountLock(ctx, t)
			if err != nil {
				return StatusCanceled, err
			}
			defer release()
			if gate, ok := a.(TaskGate); ok {
				// Bounded admission covers the maximum registered run queue and the
				// per-task request limit; a broken gate cannot wait forever.
				gateCtx, gateCancel := context.WithTimeout(ctx, 64*(e.opts.Timeout+10*time.Second))
				free, err := safeGate(gateCtx, gate, t)
				gateCancel()
				if err != nil {
					return StatusFailed, err
				}
				if free != nil {
					defer free()
				}
			}
			select {
			case e.workers <- struct{}{}:
				defer func() { <-e.workers }()
			case <-ctx.Done():
				return StatusCanceled, ctx.Err()
			}
			bounded, cancel := context.WithTimeout(ctx, e.opts.Timeout)
			defer cancel()
			return e.runTask(bounded, r.ID, t, a)
		}()
		if err != nil {
			result = mergeStatus(result, StatusFailed)
			code = "task_failed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				result = StatusCanceled
			}
			if errors.Is(err, ErrUncertain) {
				result = StatusUncertain
				code = "verification_required"
			}
		}
		status = mergeStatus(status, result)
		if status == StatusUncertain {
			code = "verification_required"
		}
		if ctx.Err() != nil {
			return
		}
		e.mu.Lock()
		err = e.commit(func(s *persisted) {
			w := s.Work[r.ID]
			w.Next = index + 1
			w.Status = status
			w.ErrorCode = code
			s.Work[r.ID] = w
		})
		e.mu.Unlock()
		if err != nil {
			status = StatusUncertain
			code = "storage_failed"
			return
		}
	}
	if len(work.Tasks) == 0 && status == StatusSkipped {
		status = StatusSkipped
		code = "no_matching_tasks"
	}
}
func mergeStatus(a, b Status) Status {
	rank := map[Status]int{StatusSucceeded: 0, StatusSkipped: -1, StatusManual: 1, StatusCanceled: 2, StatusFailed: 3, StatusUncertain: 4}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
func (e *Engine) record(id string, t Task, stage Stage, fn func() (Observation, error)) (Observation, error) {
	begin := time.Now().UTC()
	o, err := safeCall(fn)
	end := time.Now().UTC()
	status := StatusSucceeded
	code := ""
	var upstreamCode *int
	if err != nil {
		status = StatusFailed
		code = "upstream_failed"
		var classified interface{ TaskFailure() (string, *int) }
		if errors.As(err, &classified) {
			candidate, number := classified.TaskFailure()
			switch candidate {
			case "provider_rejected", "operation_unconfirmed", "account_identity_changed", "authentication_required", "task_expired", "operation_not_eligible", "provider_busy":
				code = candidate
				if number != nil {
					value := *number
					upstreamCode = &value
				}
			}
		}
	}
	// Provider errors and arbitrary response summaries are deliberately never persisted.
	outcome := ""
	if stage == StageExecute || stage == StageClaim {
		outcome = actionOutcome(t, stage, o)
		if outcome == "accepted" && err != nil {
			outcome = "unknown"
		}
		if outcome == "not_submitted" && err == nil {
			status = StatusSkipped
		}
		if outcome == "rejected" {
			status = StatusFailed
			code = "provider_rejected"
			number := o.action.code
			upstreamCode = &number
		}
	}
	serr := e.updateRun(id, func(r *Run) {
		r.Stage = stage
		r.Steps = append(r.Steps, Step{AccountID: t.AccountID, TaskKey: t.Key, Stage: stage, Status: status, Attempt: 1, StartedAt: begin, FinishedAt: &end, ErrorCode: code, UpstreamCode: upstreamCode, ActionOutcome: outcome})
		if len(r.Steps) > 2000 {
			r.Steps = r.Steps[len(r.Steps)-2000:]
		}
	})
	if serr != nil {
		return o, serr
	}
	return o, err
}
func (e *Engine) runTask(ctx context.Context, id string, t Task, a Adapter) (Status, error) {
	key := taskKey(t)
	e.mu.Lock()
	_, done := e.state.Completed[key]
	_, uncertain := e.state.Uncertain[key]
	t.AcceptedStage = ""
	if receipt, ok := e.state.Receipts[key]; ok {
		t.AcceptedStage = receiptStage(t, receipt)
	}
	for oldKey, pending := range e.state.PendingTasks {
		if oldKey != key && pending.ProviderID == t.ProviderID && pending.AccountID == t.AccountID && pending.Key == t.Key {
			if _, ok := e.state.Uncertain[oldKey]; ok {
				e.mu.Unlock()
				return StatusUncertain, ErrUncertain
			}
		}
	}
	e.mu.Unlock()
	if done && (t.Once || t.OncePerDay) {
		return StatusSkipped, nil
	}
	o, err := e.record(id, t, StageInspect, func() (Observation, error) { return a.Inspect(ctx, t) })
	if err != nil {
		return StatusFailed, err
	}
	if o.Phase == "done" {
		return e.confirm(id, key, t)
	}
	if uncertain {
		return StatusUncertain, ErrUncertain
	}
	if t.ManualOnly || o.Phase == "manual" {
		return StatusManual, nil
	}
	delay := e.opts.PollMin
	executed, claimed := false, false
	for {
		if ctx.Err() != nil {
			if executed || claimed {
				return StatusUncertain, ErrUncertain
			}
			return StatusCanceled, ctx.Err()
		}
		var stage Stage
		var action func() (Observation, error)
		switch o.Phase {
		case "done":
			return e.confirm(id, key, t)
		case "manual":
			return StatusManual, nil
		case "blocked":
			return StatusFailed, errors.New("task_unavailable")
		case "ready":
			if executed {
				return StatusUncertain, ErrUncertain
			}
			executed = true
			stage = StageExecute
			action = func() (Observation, error) { return a.Execute(ctx, t) }
		case "claimable":
			if claimed {
				return StatusUncertain, ErrUncertain
			}
			claimed = true
			stage = StageClaim
			action = func() (Observation, error) { return a.Claim(ctx, t) }
		case "pending":
		default:
			return StatusFailed, errors.New("unknown_upstream_state")
		}
		if action != nil {
			t.AcceptedStage = ""
			t.actionAttempt = randomID()
			// Commit uncertainty BEFORE sending a mutating request. On crash/retry, only
			// authoritative read-back may clear this; never blindly replay a mutation.
			e.mu.Lock()
			previousStage, hadPrevious := e.state.Uncertain[key]
			previousTask := e.state.PendingTasks[key]
			previousReceipt, hadReceipt := e.state.Receipts[key]
			if len(e.state.PendingTasks) >= 1000 {
				if _, exists := e.state.PendingTasks[key]; !exists {
					e.mu.Unlock()
					return StatusFailed, ErrBusy
				}
			}
			err = e.commit(func(s *persisted) {
				s.Uncertain[key] = stage
				delete(s.Receipts, key)
				if s.PendingTasks == nil {
					s.PendingTasks = map[string]Task{}
				}
				pending := t
				pending.State = nil
				pending.PendingVerification = true
				s.PendingTasks[key] = pending
			})
			e.mu.Unlock()
			if err != nil {
				return StatusFailed, err
			}
			result, actionErr := e.record(id, t, stage, action)
			outcome := actionOutcome(t, stage, result)
			if outcome == "not_submitted" || outcome == "rejected" {
				e.mu.Lock()
				err = e.commit(func(s *persisted) {
					delete(s.Uncertain, key)
					delete(s.PendingTasks, key)
					delete(s.Receipts, key)
					if hadPrevious {
						s.Uncertain[key] = previousStage
						s.PendingTasks[key] = previousTask
					}
					if hadReceipt {
						s.Receipts[key] = previousReceipt
					}
				})
				e.mu.Unlock()
				if err != nil || hadPrevious {
					return StatusUncertain, ErrUncertain
				}
				if ctx.Err() != nil {
					return StatusCanceled, ctx.Err()
				}
				if actionErr != nil {
					return StatusFailed, actionErr
				}
				if result.Phase == "manual" {
					return StatusManual, nil
				}
				if result.Phase == "blocked" {
					return StatusFailed, errors.New("task_unavailable")
				}
				return StatusSkipped, nil
			}
			if actionErr != nil {
				return StatusUncertain, ErrUncertain
			}
			if outcome == "accepted" {
				e.mu.Lock()
				err = e.commit(func(s *persisted) {
					if s.Receipts == nil {
						s.Receipts = map[string]actionReceipt{}
					}
					s.Receipts[key] = actionReceipt{Attempt: t.actionAttempt, Stage: stage, Binding: actionBinding(t, stage)}
					pending := s.PendingTasks[key]
					pending.AcceptedStage = stage
					s.PendingTasks[key] = pending
				})
				e.mu.Unlock()
				if err != nil {
					return StatusUncertain, ErrUncertain
				}
				t.AcceptedStage = stage
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			if !executed && !claimed {
				return StatusCanceled, ctx.Err()
			}
			return StatusUncertain, ErrUncertain
		}
		o, err = e.record(id, t, StageVerify, func() (Observation, error) { return a.Verify(ctx, t) })
		if err != nil {
			if !executed && !claimed {
				if ctx.Err() != nil {
					return StatusCanceled, ctx.Err()
				}
				return StatusFailed, err
			}
			return StatusUncertain, ErrUncertain
		}
		if delay < e.opts.PollMax {
			delay *= 2
			if delay > e.opts.PollMax {
				delay = e.opts.PollMax
			}
		}
	}
}
func (e *Engine) confirm(id, key string, t Task) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.commit(func(s *persisted) {
		if work, ok := s.Work[id]; ok {
			if work.Confirmed == nil {
				work.Confirmed = map[string]bool{}
			}
			work.Confirmed[key] = true
			s.Work[id] = work
		}
		delete(s.Uncertain, key)
		delete(s.PendingTasks, key)
		delete(s.Receipts, key)
		if t.Once || t.OncePerDay {
			s.Completed[key] = time.Now().UTC()
		}
	})
	return StatusSucceeded, err
}

func (e *Engine) SaveSchedule(s Schedule) (Schedule, error) {
	s.Hours = append([]int{}, s.Hours...)
	s.AccountIDs = append([]string{}, s.AccountIDs...)
	if !IDPattern.MatchString(s.ID) || !IDPattern.MatchString(s.ProviderID) || s.Minute < 0 || s.Minute > 59 || len(s.Hours) == 0 || len(s.Hours) > 24 || len(s.AccountIDs) > 500 {
		return s, ErrInvalid
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return s, ErrInvalid
	}
	seen := map[int]bool{}
	for _, h := range s.Hours {
		if h < 0 || h > 23 || seen[h] {
			return s, ErrInvalid
		}
		seen[h] = true
	}
	for _, id := range s.AccountIDs {
		if !IDPattern.MatchString(id) {
			return s, ErrInvalid
		}
	}
	sort.Ints(s.Hours)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.adapters[s.ProviderID] == nil {
		return s, ErrInvalid
	}
	s.LastDispatched = nil
	s.LastRunID = ""
	if old, ok := e.state.Schedules[s.ID]; ok {
		s.LastDispatched = old.LastDispatched
		s.LastRunID = old.LastRunID
	}
	next := NextFire(s, time.Now())
	s.NextRun = nil
	if !next.IsZero() {
		s.NextRun = &next
	}
	err := e.commit(func(state *persisted) { state.Schedules[s.ID] = s })
	if err == nil {
		e.notify()
	}
	b := cloneState(e.state)
	return b.Schedules[s.ID], err
}
func (e *Engine) Schedules() []Schedule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []Schedule{}
	for _, s := range cloneState(e.state).Schedules {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (e *Engine) DeleteSchedule(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.state.Schedules[id]; !ok {
		return ErrNotFound
	}
	err := e.commit(func(s *persisted) { delete(s.Schedules, id) })
	e.notify()
	return err
}
func (e *Engine) Tick(now time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrInvalid
	}
	for _, s := range e.state.Schedules {
		if !s.Enabled || s.NextRun == nil || s.NextRun.After(now) {
			continue
		}
		due := *s.NextRun
		// One dispatch for each saved instant, even across restart. Missed days are
		// coalesced into one discovery of today's upstream state.
		next := NextFire(s, now)
		s.NextRun = &next
		if s.OncePerDay && s.LastDispatched != nil {
			loc, _ := time.LoadLocation(s.Timezone)
			if s.LastDispatched.In(loc).Format("2006-01-02") == now.In(loc).Format("2006-01-02") {
				if err := e.commit(func(st *persisted) { st.Schedules[s.ID] = s }); err != nil {
					return err
				}
				continue
			}
		}
		s.LastDispatched = &now
		_, err := e.submitScheduleLocked(Request{ProviderID: s.ProviderID, TaskKey: s.TaskKey, AccountIDs: s.AccountIDs, ScheduleID: s.ID, Trigger: "schedule", ScheduledAt: &due}, &s)
		if err != nil {
			return fmt.Errorf("schedule_dispatch_failed: %w", err)
		}
	}
	return nil
}
func (e *Engine) Run(ctx context.Context) error {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		if err := e.Tick(time.Now()); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		case <-e.wake:
		}
	}
}
