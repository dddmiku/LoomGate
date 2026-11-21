package taskproviders

import (
	"context"
	"errors"
	"time"

	"workbuddy2api/internal/taskengine"
)

const traeAutoRetryLimit = 3

var errTraeRetryNotDue = errors.New("trae_retry_not_due")

func traeBusinessDay(now time.Time) string {
	return now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
}

func traeRetryBusinessDay(state traeTaskState) string {
	if state.Code != 9074 {
		return ""
	}
	if state.RetryDay != "" {
		return state.RetryDay
	}
	if state.Failures <= 0 || state.RetryAt.IsZero() {
		return ""
	}
	// v1.0.8 persisted only the cooldown deadline and failure count. Recover
	// the refusal day rather than mistaking a midnight deadline for a new-day
	// refusal. Missing failure evidence never authorizes migration.
	delay := time.Minute << min(state.Failures-1, 3)
	return traeBusinessDay(state.RetryAt.Add(-delay))
}

func traeCooldownActive(state traeTaskState, now time.Time) bool {
	if !now.Before(state.RetryAt) {
		return false
	}
	if day := traeRetryBusinessDay(state); day != "" {
		return day == traeBusinessDay(now)
	}
	// A deadline whose origin cannot be proved remains conservatively gated.
	return true
}

func traeRetryDue(state traeTaskState, now time.Time) bool {
	day := traeBusinessDay(now)
	if state.Code != 9074 || state.CheckedDay == day || state.RetryAt.IsZero() || now.Before(state.RetryAt) || now.Before(state.NextAutoAt) || state.AutoAttempts >= traeAutoRetryLimit {
		return false
	}
	if state.RetryDay == "" {
		// The v1.0.8 journal had no retry day. A same-day refusal is the only
		// safe migration; never revive a previous-day or future-day attempt.
		return traeBusinessDay(state.RetryAt) == day && traeRetryBusinessDay(state) == day
	}
	return state.RetryDay == day && state.AutoPending
}

func traeRetryPlan(schedules []taskengine.Schedule, account string) string {
	for _, s := range schedules {
		if !s.Enabled || s.ProviderID != "trae" {
			continue
		}
		covers := false
		if len(s.TaskSequence) == 0 {
			covers = taskengine.TaskKeyMatches(s.TaskKey, "checkin")
		} else {
			for _, key := range s.TaskSequence {
				if taskengine.TaskKeyMatches(key, "checkin") {
					covers = true
					break
				}
			}
		}
		if !covers {
			continue
		}
		if len(s.AccountIDs) == 0 {
			return s.ID
		}
		for _, id := range s.AccountIDs {
			if id == account {
				return s.ID
			}
		}
	}
	return ""
}

// TakeDeferredRequests only reserves local, explicitly refused work. It never
// performs HTTP or claims success, and every returned request must go through
// the engine usual discovery, uncertainty guard, account lock and verify path.
// Reserving before dispatch can lose one opportunity on a crash; it cannot
// create unlimited submissions or replay an unknown provider action.
func (a *Accounts) TakeDeferredRequests(ctx context.Context, now time.Time, schedules []taskengine.Schedule) ([]taskengine.Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.Provider != "trae" || a.Pool == nil {
		return nil, nil
	}
	modules, err := a.modules()
	if err != nil {
		return nil, err
	}
	var module *traeCheckinModule
	for _, candidate := range modules {
		if found, ok := candidate.(*traeCheckinModule); ok {
			module = found
			break
		}
	}
	if module == nil || module.store == nil || module.store.vault == nil {
		return nil, nil
	}
	var requests []taskengine.Request
	for _, status := range a.Pool.List() {
		if err = ctx.Err(); err != nil {
			return requests, err
		}
		if status.Disabled {
			continue
		}
		account, accountErr := a.account(status.UID)
		if accountErr != nil || account.Realm() != "trae" {
			continue
		}
		scheduleID := traeRetryPlan(schedules, account.UID)
		if scheduleID == "" {
			continue
		}
		if !traeRetryDue(module.store.read(account.UID), now) {
			continue
		}
		err = module.store.change(account.UID, func(state *traeTaskState, _ *traeTaskStateData) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !traeRetryDue(*state, now) {
				return errTraeRetryNotDue
			}
			state.RetryDay = traeBusinessDay(now)
			state.AutoPending = true
			state.AutoAttempts++
			state.NextAutoAt = now.Add(time.Minute)
			return nil
		})
		if errors.Is(err, errTraeRetryNotDue) {
			continue
		}
		if err != nil {
			return requests, err
		}
		requests = append(requests, taskengine.Request{ProviderID: a.Provider, TaskKey: "checkin", AccountIDs: []string{account.UID}, ScheduleID: scheduleID, Trigger: "retry", ExpectedPeriodKey: traeBusinessDay(now)})
		// Reserve only the request this tick can dispatch. A batch reservation
		// would consume tail-account budgets when the engine queue is bounded.
		return requests, nil
	}
	return requests, nil
}
