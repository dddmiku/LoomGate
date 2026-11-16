package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/auth"
)

func TestWorkbenchTaskSchemaRejectsMissingEligibilityFields(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		call       func(*Client, *auth.Auth) error
	}{
		{"missing_buddy", `{}`, func(c *Client, a *auth.Auth) error { _, e := c.TaskBuddy(context.Background(), a); return e }},
		{"missing_daily", `{"state":"idle","record_id":0}`, func(c *Client, a *auth.Auth) error { _, e := c.TaskTravel(context.Background(), a); return e }},
		{"unknown_travel", `{"state":"new-state","record_id":1,"daily_limit_reached":false}`, func(c *Client, a *auth.Auth) error { _, e := c.TaskTravel(context.Background(), a); return e }},
		{"missing_score", `{"today":{"date":"2099-01-03"},"cells":[{"date":"2099-01-02"}]}`, func(c *Client, a *auth.Auth) error { _, e := c.TaskHeatmap(context.Background(), a); return e }},
		{"null_score", `{"today":{"date":"2099-01-03"},"cells":[{"date":"2099-01-02","score":null}]}`, func(c *Client, a *auth.Auth) error { _, e := c.TaskHeatmap(context.Background(), a); return e }},
		{"missing_days", `{"streak":{}}`, func(c *Client, a *auth.Auth) error { _, e := c.TaskStreak(context.Background(), a); return e }},
		{"missing_claimed", `{"active":true,"credit_num":100,"end_time":"2099-01-03"}`, func(c *Client, a *auth.Auth) error { _, e := c.TaskCompensation(context.Background(), a); return e }},
		{"duplicate_locations", `{"locations":[{"id":73},{"id":73}]}`, func(c *Client, a *auth.Auth) error { _, e := c.TaskTravelConfig(context.Background(), a); return e }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"code":0,"data":` + tc.data + `}`))
			}))
			defer s.Close()
			c := &Client{HTTP: s.Client(), ChatBaseCN: s.URL, BillingBaseCN: s.URL}
			if tc.call(c, &auth.Auth{UID: "synthetic", Domain: "copilot.tencent.com"}) == nil {
				t.Fatal("missing schema was converted into eligibility")
			}
		})
	}
}
func TestWorkbenchReadsAndMutationsUseCallerCancellation(t *testing.T) {
	var received atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		received.Add(1)
		<-r.Context().Done()
	}))
	defer s.Close()
	c := &Client{HTTP: s.Client(), ChatBaseCN: s.URL, BillingBaseCN: s.URL}
	a := &auth.Auth{UID: "synthetic", Domain: "copilot.tencent.com"}
	for _, call := range []func(context.Context) error{
		func(ctx context.Context) error { _, e := c.TaskTravel(ctx, a); return e },
		func(ctx context.Context) error { return c.TaskTravelDepart(ctx, a, 73) },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		started := time.Now()
		err := call(ctx)
		cancel()
		if err == nil || time.Since(started) > time.Second {
			t.Fatal("task transport ignored cancellation")
		}
	}
	if received.Load() != 2 {
		t.Fatal("cancellation request not exercised")
	}
}
