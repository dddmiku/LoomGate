package taskengine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

func detachedSchedule(s Schedule) Schedule {
	s.Hours = append([]int(nil), s.Hours...)
	s.AccountIDs = append([]string(nil), s.AccountIDs...)
	s.TaskSequence = append([]string(nil), s.TaskSequence...)
	if s.NextRun != nil {
		v := *s.NextRun
		s.NextRun = &v
	}
	if s.LastDispatched != nil {
		v := *s.LastDispatched
		s.LastDispatched = &v
	}
	if s.LegacyEnabled != nil {
		v := *s.LegacyEnabled
		s.LegacyEnabled = &v
	}
	return s
}

func accountScopesOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, id := range a {
		if contains(b, id) {
			return true
		}
	}
	return false
}

func taskSelectionsOverlap(a string, as []string, b string, bs []string) bool {
	if len(as) == 0 {
		as = []string{a}
	}
	if len(bs) == 0 {
		bs = []string{b}
	}
	for _, left := range as {
		for _, right := range bs {
			if TaskKeyMatches(left, right) || TaskKeyMatches(right, left) {
				return true
			}
		}
	}
	return false
}

// Do not spend a recovery reservation while matching work is active. A broad
// plan waits conservatively for its overlapping run; ordinary scheduling and
// manual verification are unchanged.
func (e *Engine) deferredActiveLocked(s Schedule) bool {
	for _, run := range e.state.Runs {
		if run.ProviderID == s.ProviderID && (run.FinishedAt == nil || e.running[run.ID] != nil) && accountScopesOverlap(run.AccountIDs, s.AccountIDs) && taskSelectionsOverlap(run.TaskKey, run.TaskSequence, s.TaskKey, s.TaskSequence) {
			return true
		}
	}
	return false
}

func (e *Engine) deferredPendingLocked(s Schedule) bool {
	for _, task := range e.state.PendingTasks {
		if task.ProviderID == s.ProviderID && (len(s.AccountIDs) == 0 || contains(s.AccountIDs, task.AccountID)) && taskIncluded(s.TaskKey, s.TaskSequence, task.Key) {
			return true
		}
	}
	return false
}

func safeDeferred(ctx context.Context, a DeferredRequestAdapter, now time.Time, schedules []Schedule) (requests []Request, err error) {
	defer func() {
		if recover() != nil {
			requests = nil
			err = errors.New("adapter_panic")
		}
	}()
	return a.TakeDeferredRequests(ctx, now, schedules)
}

func (e *Engine) tickDeferred(now time.Time) error {
	// Tick can also be called explicitly. Serialize local reservations without
	// holding the engine mutex over a provider's file I/O.
	e.deferredMu.Lock()
	defer e.deferredMu.Unlock()
	type source struct {
		adapter DeferredRequestAdapter
		plans   []Schedule
		allowed map[string]bool
	}
	sources := map[string]*source{}
	e.mu.Lock()
	if e.closed || e.draining || len(e.running) >= 64 || len(e.state.Work) >= 64 {
		e.mu.Unlock()
		return nil
	}
	for _, plan := range e.state.Schedules {
		adapter, ok := e.adapters[plan.ProviderID].(DeferredRequestAdapter)
		// A pending account must not indefinitely block unrelated accounts in
		// an all-accounts plan. Recheck pending work per candidate below.
		if !ok || !plan.Enabled || e.deferredActiveLocked(plan) || len(plan.AccountIDs) == 1 && e.deferredPendingLocked(plan) {
			continue
		}
		s := sources[plan.ProviderID]
		if s == nil {
			s = &source{adapter: adapter, allowed: map[string]bool{}}
			sources[plan.ProviderID] = s
		}
		s.plans = append(s.plans, detachedSchedule(plan))
		s.allowed[plan.ID] = true
	}
	if len(sources) == 0 {
		e.mu.Unlock()
		return nil
	}
	// Close/Drain must wait for local journal access before the caller closes
	// provider storage. Add while holding the same mutex that gates Close.
	e.wg.Add(1)
	e.mu.Unlock()
	defer e.wg.Done()
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seen := map[string]bool{}
	for _, id := range ids {
		s := sources[id]
		sort.Slice(s.plans, func(i, j int) bool { return s.plans[i].ID < s.plans[j].ID })
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		requests, _ := safeDeferred(ctx, s.adapter, now, s.plans)
		cancel()
		// A hook failure must not stop the scheduler or another provider. Valid
		// partial results already own a durable reservation and can still run.
		if len(requests) > 1000 {
			requests = requests[:1000]
		}
		for _, request := range requests {
			if request.ProviderID != id || !s.allowed[request.ScheduleID] || len(request.AccountIDs) != 1 || !validAccountScope(request.AccountIDs) || len(request.TaskSequence) != 0 || request.TaskKey == "" || request.TaskKey == "*" || strings.HasSuffix(request.TaskKey, "/") || !validTaskSelector(request.TaskKey) {
				continue
			}
			key := id + "\x00" + request.AccountIDs[0] + "\x00" + request.TaskKey
			if seen[key] {
				continue
			}
			seen[key] = true
			e.mu.Lock()
			plan, ok := e.state.Schedules[request.ScheduleID]
			allowed := ok && !e.closed && !e.draining && plan.Enabled && plan.ProviderID == id && taskIncluded(plan.TaskKey, plan.TaskSequence, request.TaskKey) && (len(plan.AccountIDs) == 0 || contains(plan.AccountIDs, request.AccountIDs[0]))
			if allowed {
				probe := Schedule{ProviderID: id, TaskKey: request.TaskKey, AccountIDs: request.AccountIDs}
				allowed = !e.deferredActiveLocked(probe) && !e.deferredPendingLocked(probe)
			}
			var err error
			if allowed {
				_, err = e.submitLocked(Request{ProviderID: id, TaskKey: request.TaskKey, AccountIDs: request.AccountIDs, ScheduleID: plan.ID, Trigger: "retry", ExpectedPeriodKey: request.ExpectedPeriodKey})
			}
			e.mu.Unlock()
			if err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrInvalid) {
				return fmt.Errorf("deferred_dispatch_failed: %w", err)
			}
		}
	}
	return nil
}
