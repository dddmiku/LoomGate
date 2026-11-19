package providerretry

// The shared runner has to match the legacy path's behaviour, because that is
// the behaviour the user already trusts: a bounded number of account switches,
// a transient hiccup retried once on the same account, and a client error
// surfaced immediately.

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fixedClass struct {
	class Class
	msg   string
}

func (e fixedClass) Error() string     { return e.msg }
func (e fixedClass) RetryClass() Class { return e.class }

func TestTransientRetriesSameAccountOnce(t *testing.T) {
	calls := map[string]int{}
	cool := NewCooldown()
	value, err := Run(context.Background(), []string{"a", "b"}, 3, cool, func(_ context.Context, account string) (int, error) {
		calls[account]++
		if account == "a" && calls[account] == 1 {
			return 0, fixedClass{class: RetryTransient, msg: "hiccup"}
		}
		return 7, nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if value != 7 {
		t.Fatalf("value=%d", value)
	}
	if calls["a"] != 2 {
		t.Fatalf("account a tried %d times, want 2 (one retry)", calls["a"])
	}
	if calls["b"] != 0 {
		t.Fatal("a healthy account was abandoned after a transient error")
	}
}

func TestTransientStopsAfterOneRetry(t *testing.T) {
	calls := 0
	cool := NewCooldown()
	_, err := Run(context.Background(), []string{"a"}, 3, cool, func(context.Context, string) (int, error) {
		calls++
		return 0, fixedClass{class: RetryTransient, msg: "always empty"}
	})
	if err == nil {
		t.Fatal("persistent transient failure reported success")
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2", calls)
	}
}

func TestAccountErrorRotatesAndCools(t *testing.T) {
	calls := []string{}
	cool := NewCooldown()
	value, err := Run(context.Background(), []string{"a", "b"}, 3, cool, func(_ context.Context, account string) (int, error) {
		calls = append(calls, account)
		if account == "a" {
			return 0, fixedClass{class: RetryAccount, msg: "401"}
		}
		return 9, nil
	})
	if err != nil || value != 9 {
		t.Fatalf("value=%d err=%v", value, err)
	}
	if len(calls) != 2 || calls[0] != "a" || calls[1] != "b" {
		t.Fatalf("unexpected rotation: %v", calls)
	}
	if cool.Ready("a") {
		t.Fatal("failed account was not cooled down")
	}
	if !cool.Ready("b") {
		t.Fatal("successful account was cooled down")
	}
}

func TestFatalReturnsImmediately(t *testing.T) {
	calls := 0
	cool := NewCooldown()
	_, err := Run(context.Background(), []string{"a", "b"}, 3, cool, func(context.Context, string) (int, error) {
		calls++
		return 0, fixedClass{class: Fatal, msg: "bad request"}
	})
	if err == nil {
		t.Fatal("fatal error reported success")
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1 (no retry on a client error)", calls)
	}
}

func TestCooldownSkipsWithoutSpendingBudget(t *testing.T) {
	cool := NewCooldown()
	cool.Set("a", time.Minute)
	seen := []string{}
	_, err := Run(context.Background(), []string{"a", "b", "c"}, 1, cool, func(_ context.Context, account string) (int, error) {
		seen = append(seen, account)
		if account == "b" {
			return 0, fixedClass{class: RetryAccount, msg: "500"}
		}
		return 1, nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// "a" is skipped for free, so the single switch budget still reaches "c".
	if len(seen) != 2 || seen[0] != "b" || seen[1] != "c" {
		t.Fatalf("seen=%v (a must be skipped without spending the switch budget)", seen)
	}
}

func TestAllAccountsExhaustedReturnsLastError(t *testing.T) {
	cool := NewCooldown()
	_, err := Run(context.Background(), []string{"a"}, 3, cool, func(context.Context, string) (int, error) {
		return 0, fixedClass{class: RetryAccount, msg: "always down"}
	})
	if err == nil {
		t.Fatal("exhausted pool reported success")
	}
	var fc fixedClass
	if !errors.As(err, &fc) || fc.msg != "always down" {
		t.Fatalf("last error not surfaced: %v", err)
	}
}

func TestContextCancelStopsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := Run(ctx, []string{"a", "b"}, 3, NewCooldown(), func(context.Context, string) (int, error) {
		calls++
		return 0, fixedClass{class: RetryAccount, msg: "x"}
	})
	if err == nil {
		t.Fatal("cancelled context reported success")
	}
	if calls != 0 {
		t.Fatalf("calls=%d want 0 after cancellation", calls)
	}
}

// A quota exhaustion needs a longer rest than a transient failure. The default
// 30s made a rate-limited account come back and get rate-limited again on
// repeat, burning a rotation slot each time.
func TestCooldownDurationIsHonoured(t *testing.T) {
	cool := NewCooldown()
	_, err := Run(context.Background(), []string{"a", "b"}, 1, cool, func(_ context.Context, account string) (struct{}, error) {
		if account == "a" {
			return struct{}{}, longCoolingError{}
		}
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if cool.Ready("a") {
		t.Fatal("long-cooldown account was immediately ready again")
	}
}

type longCoolingError struct{}

func (longCoolingError) Error() string             { return "quota exhausted" }
func (longCoolingError) RetryClass() Class         { return RetryAccount }
func (longCoolingError) CooldownDuration() time.Duration { return time.Hour }
