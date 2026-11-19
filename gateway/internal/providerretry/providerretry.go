// Package providerretry gives inference providers the same retry and error
// discipline the WorkBuddy path already had: a bounded rotation budget, a
// classification of which failures are worth retrying, and a cooldown per
// account so a failing account is skipped instead of being hammered.
//
// It exists because the legacy path solved these problems once and the newer
// providers (AutoClaw, Doubao) were each hand-rolling a weaker version of it.
package providerretry

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Class is what a failure means for routing.
type Class int

const (
	// RetryAccount: the account is implicated (auth, quota, rate limit, 5xx).
	// Move to the next account and cool this one down.
	RetryAccount Class = iota
	// RetryTransient: the account is fine; the upstream hiccupped (an
	// acknowledged turn with no answer). Retry once on the same account.
	RetryTransient
	// Fatal: retrying cannot help (bad request, unsupported model). Return it.
	Fatal
)

// Classified is implemented by provider errors that know their class.
type Classified interface {
	RetryClass() Class
}

func classOf(err error) Class {
	var c Classified
	if errors.As(err, &c) {
		return c.RetryClass()
	}
	return Fatal
}

// Cooldown is a small per-account cool-down table. It is deliberately in-memory:
// a restart forgetting cooldowns is harmless, and it keeps the provider free of
// another persistence format.
type Cooldown struct {
	mu    sync.Mutex
	until map[string]time.Time
	now   func() time.Time
}

func NewCooldown() *Cooldown {
	return &Cooldown{until: map[string]time.Time{}, now: time.Now}
}

// Set marks an account unavailable until d from now.
func (c *Cooldown) Set(account string, d time.Duration) {
	if c == nil || account == "" || d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until[account] = c.now().Add(d)
}

// Ready reports whether the account may be tried now.
func (c *Cooldown) Ready(account string) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.until[account]
	if !ok {
		return true
	}
	if c.now().After(until) {
		delete(c.until, account)
		return true
	}
	return false
}

// Clear removes an account's cool-down after a success.
func (c *Cooldown) Clear(account string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.until, account)
}

// Attempt is one account try.
type Attempt[T any] func(ctx context.Context, account string) (T, error)

// Run executes attempts over the ordered account list with the same budget
// semantics as the legacy path:
//
//   - at most `budget` account switches (the first account does not count)
//   - cooled-down accounts are skipped without counting against the budget
//   - a transient failure gets exactly one same-account retry
//   - a fatal failure returns immediately
//
// It returns the last error when every allowed attempt fails.
func Run[T any](ctx context.Context, accounts []string, budget int, cool *Cooldown, attempt Attempt[T]) (T, error) {
	var zero T
	if budget <= 0 {
		budget = 3
	}
	switches := 0
	attempted := 0
	var lastErr error
	for _, account := range accounts {
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		// A cooled-down account is skipped for free: it was not a switch, so it
		// must not consume the budget that decides how many healthy accounts may
		// still be tried.
		if !cool.Ready(account) {
			continue
		}
		if attempted > 0 {
			if switches >= budget {
				break
			}
			switches++
		}
		attempted++
		for try := 0; ; try++ {
			value, err := attempt(ctx, account)
			if err == nil {
				cool.Clear(account)
				return value, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				return zero, ctx.Err()
			}
			switch classOf(err) {
			case Fatal:
				return zero, err
			case RetryTransient:
				if try == 0 {
					continue
				}
				// The account is healthy; stop trying this one but do not cool it.
				goto next
			case RetryAccount:
				cool.Set(account, 30*time.Second)
				goto next
			}
			goto next
		}
	next:
	}
	if lastErr == nil {
		lastErr = errors.New("no_available_account")
	}
	return zero, lastErr
}
