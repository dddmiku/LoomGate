package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sessionSourceFunc func(context.Context, string) (Session, error)

func (f sessionSourceFunc) Session(ctx context.Context, account string) (Session, error) {
	return f(ctx, account)
}

type syntheticTransport func(*http.Request) (*http.Response, error)

func (f syntheticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureSession(account, revision string) Session {
	return Session{Query: url.Values{"fixture_account": {account}, "fixture_revision": {revision}},
		Headers: http.Header{"X-Test-Account": {account}, "X-Test-Revision": {revision}}, BotID: "synthetic-bot", Revision: revision}
}
func fixedSessions() Sessions {
	return sessionSourceFunc(func(_ context.Context, account string) (Session, error) {
		return fixtureSession(account, "revision-a"), nil
	})
}
func jsonFixture(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func fixtureResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func modelFixture(name string) map[string]any {
	return map[string]any{"model_item_key": "shared-model", "item_id": 101, "name": name, "show_task_tools": 1,
		"model_extra_params":      map[string]any{"total_window_size": "131072", "memory_profile": "fixture-memory"},
		"reasoning_effort_config": map[string]any{"default_level": 2, "option_list": []any{map[string]any{"level": 2, "name": "Synthetic effort"}}},
		"tag_list":                []any{map[string]any{"text": "Synthetic tier"}}}
}
func catalogFixture(t *testing.T, name string) string {
	t.Helper()
	return jsonFixture(t, map[string]any{"code": 0, "data": map[string]any{"entry_list": []any{
		map[string]any{"active_switch_conf": map[string]any{"menu_conf_v2": map[string]any{"use_mode_model_list": true,
			"mode_list": map[string]any{"item_list": []any{map[string]any{"mode_id": "mode-a", "name": "Synthetic mode", "model_list": map[string]any{"item_list": []any{modelFixture(name)}}}}}}}},
	}}})
}

type catalogResult struct {
	catalog Catalog
	err     error
}

func waitCatalog(t *testing.T, ch <-chan catalogResult) catalogResult {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("catalog operation did not finish")
		return catalogResult{}
	}
}
func waitFixtureSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture stage was not reached")
	}
}

type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}
func observedContext(ctx context.Context) *observedDoneContext {
	return &observedDoneContext{Context: ctx, observed: make(chan struct{})}
}

// Only timestamps are advanced under the cache mutex; tests exercise Catalog
// through its public behavior without sleeping for the production five-minute TTL.
func ageOnlyCacheEntry(t *testing.T, c *Client, age time.Duration) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) != 1 {
		t.Fatalf("fixture expected one account cache, got %d", len(c.cache))
	}
	for _, entry := range c.cache {
		entry.snapshot.FetchedAt = time.Now().Add(-age)
		entry.snapshot.ExpiresAt = time.Now().Add(-time.Second)
	}
}

func TestDoubaoModeGroupedCatalogKeepsIdentityCapabilitiesAndFutureFields(t *testing.T) {
	first := modelFixture("Synthetic model A")
	second := modelFixture("Synthetic model B")
	second["item_id"], second["need_login"] = 202, true
	second["future_model_options"] = map[string]any{"unknown_capability": true}
	data := map[string]any{"future_root": true, "entry_list": []any{map[string]any{"active_switch_conf": map[string]any{"menu_conf_v2": map[string]any{
		"use_mode_model_list": true, "future_menu": []int{1, 2},
		"model_list": map[string]any{"item_list": []any{modelFixture("Should not use global catalog")}},
		"mode_list": map[string]any{"item_list": []any{
			map[string]any{"mode_id": "mode-a", "name": "Mode A", "model_list": map[string]any{"item_list": []any{first, first}}},
			map[string]any{"mode_id": "mode-b", "name": "Mode B", "model_list": map[string]any{"item_list": []any{second}}},
		}},
	}}}}}
	client := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Host != "www.doubao.com" || r.URL.Path != "/alice/slot/action_bar_v3/brief_list" {
			t.Error("wrong official catalog request target")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["bot_id"] != "synthetic-bot" {
			t.Error("catalog request lost session bot ID")
		}
		return fixtureResponse(r, 200, jsonFixture(t, map[string]any{"code": 0, "data": data})), nil
	}))
	got, err := client.Catalog(context.Background(), "account-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("grouped models=%d, want two unique mode/model combinations", len(got.Models))
	}
	if got.Models[0].ID == got.Models[1].ID || got.Models[0].UpstreamID != got.Models[1].UpstreamID {
		t.Error("mode identity was flattened or upstream model identity changed")
	}
	if got.Models[0].Name != "Synthetic model A" || got.Models[1].Name != "Synthetic model B" || got.Models[1].ModeName != "Mode B" || !got.Models[1].NeedLogin {
		t.Error("response-driven model or mode metadata was lost")
	}
	m := got.Models[0]
	if m.ContextWindow != 131072 || !m.NativeTools || m.DefaultEffort != 2 || len(m.Efforts) != 1 || len(m.Tiers) != 1 {
		t.Errorf("capabilities were not preserved: %+v", m)
	}
}

func TestDoubaoGlobalCatalogUsesActualItemIDWhenModelKeyIsAbsent(t *testing.T) {
	data := `{"entry_list":[{"active_switch_conf":{"menu_conf_v2":{"use_mode_model_list":false,"model_list":{"item_list":[{"item_id":321,"name":"Numeric identifier model"}]}}}}]}`
	models, err := ParseCatalog([]byte(data))
	if err != nil || len(models) != 1 {
		t.Fatalf("valid item_id fallback not parsed: models=%d error=%v", len(models), err)
	}
	if models[0].UpstreamID != "321" || models[0].ItemID != 321 {
		t.Errorf("item_id identity fallback was not retained: %+v", models[0])
	}
}

func TestDoubaoHTTP200DoesNotTurnBusinessOrSchemaFailureIntoCatalogSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"business-error", `{"code":42,"data":{"entry_list":[]}}`, 200},
		{"missing-code", `{"data":{"entry_list":[]}}`, 200},
		{"null-code", `{"code":null,"data":{"entry_list":[]}}`, 200},
		{"missing-data", `{"code":0}`, 200},
		{"null-data", `{"code":0,"data":null}`, 200},
		{"empty-data-schema", `{"code":0,"data":{}}`, 200},
		{"empty-menu-schema", `{"code":0,"data":{"entry_list":[{"active_switch_conf":{"menu_conf_v2":{}}}]}}`, 200},
		{"empty-per-mode-schema", `{"code":0,"data":{"entry_list":[{"active_switch_conf":{"menu_conf_v2":{"use_mode_model_list":true,"mode_list":{"item_list":[]}}}}]}}`, 200},
		{"missing-mode-identity", `{"code":0,"data":{"entry_list":[{"active_switch_conf":{"menu_conf_v2":{"use_mode_model_list":true,"mode_list":{"item_list":[{"model_list":{"item_list":[{"model_item_key":"m","name":"Model"}]}}]}}}}]}}`, 200},
		{"trailing-envelope", `{"code":0,"data":{}} {"code":0}`, 200},
		{"http-error", `{"code":0,"data":{}}`, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) { return fixtureResponse(r, tc.status, tc.body), nil }))
			got, err := c.Catalog(context.Background(), "account-a", false)
			if err == nil {
				t.Error("invalid/business-failed response was accepted as a successful catalog")
			}
			if len(got.Models) != 0 {
				t.Error("invalid response supplied usable models")
			}
		})
	}
}

func TestDoubaoConcurrentRefreshMergesForTheSameAccount(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	response := catalogFixture(t, "Merged model")
	c := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return fixtureResponse(r, 200, response), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results := make(chan catalogResult, 8)
	go func() { v, err := c.Catalog(ctx, "account-a", true); results <- catalogResult{v, err} }()
	waitFixtureSignal(t, entered)
	for i := 0; i < 7; i++ {
		waiter := observedContext(ctx)
		go func() { v, err := c.Catalog(waiter, "account-a", true); results <- catalogResult{v, err} }()
		waitFixtureSignal(t, waiter.observed)
	}
	close(release)
	for i := 0; i < 8; i++ {
		if got := waitCatalog(t, results); got.err != nil || len(got.catalog.Models) != 1 {
			t.Errorf("merged result: models=%d error=%v", len(got.catalog.Models), got.err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("same-account simultaneous refresh count=%d", calls.Load())
	}
}

func TestDoubaoAccountCatalogsRefreshIndependently(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	responses := map[string]string{"account-a": catalogFixture(t, "Account A model"), "account-b": catalogFixture(t, "Account B model")}
	c := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) {
		account := r.URL.Query().Get("fixture_account")
		entered <- account
		select {
		case <-release:
			return fixtureResponse(r, 200, responses[account]), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results := make(chan catalogResult, 2)
	for _, account := range []string{"account-a", "account-b"} {
		go func() { v, err := c.Catalog(ctx, account, false); results <- catalogResult{v, err} }()
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case account := <-entered:
			seen[account] = true
		case <-ctx.Done():
			t.Fatal("one account blocked a different account refresh")
		}
	}
	close(release)
	if len(seen) != 2 {
		t.Fatal("account sessions were mixed")
	}
	for i := 0; i < 2; i++ {
		if result := waitCatalog(t, results); result.err != nil {
			t.Fatal(result.err)
		}
	}
	for account, name := range map[string]string{"account-a": "Account A model", "account-b": "Account B model"} {
		got, err := c.Catalog(ctx, account, false)
		if err != nil || len(got.Models) != 1 || got.Models[0].Name != name {
			t.Errorf("account catalog isolation failed for %s", account)
		}
	}
}

func TestDoubaoOldRevisionWaiterCannotReplaceNewLoginCatalog(t *testing.T) {
	var revision atomic.Int32
	var oldCalls, newCalls atomic.Int32
	oldEntered, releaseOld := make(chan struct{}), make(chan struct{})
	oldResponse, newResponse := catalogFixture(t, "Old login model"), catalogFixture(t, "New login model")
	sessions := sessionSourceFunc(func(_ context.Context, account string) (Session, error) {
		rev := "revision-a"
		if revision.Load() != 0 {
			rev = "revision-b"
		}
		return fixtureSession(account, rev), nil
	})
	c := New(sessions, syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Test-Revision") == "revision-a" {
			if oldCalls.Add(1) == 1 {
				close(oldEntered)
			}
			select {
			case <-releaseOld:
				return fixtureResponse(r, 200, oldResponse), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		newCalls.Add(1)
		return fixtureResponse(r, 200, newResponse), nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	oldResults := make(chan catalogResult, 2)
	go func() { v, err := c.Catalog(ctx, "account-a", false); oldResults <- catalogResult{v, err} }()
	waitFixtureSignal(t, oldEntered)
	waiter := observedContext(ctx)
	go func() { v, err := c.Catalog(waiter, "account-a", false); oldResults <- catalogResult{v, err} }()
	waitFixtureSignal(t, waiter.observed)
	revision.Store(1)
	current, err := c.Catalog(ctx, "account-a", false)
	if err != nil || len(current.Models) != 1 || current.Models[0].Name != "New login model" {
		t.Fatal("new login could not independently refresh its catalog")
	}
	close(releaseOld)
	waitCatalog(t, oldResults)
	waitCatalog(t, oldResults)
	current, err = c.Catalog(ctx, "account-a", false)
	if err != nil || len(current.Models) != 1 || current.Models[0].Name != "New login model" {
		t.Fatal("old login result replaced current identity data")
	}
	if newCalls.Load() != 1 {
		t.Errorf("new login cache was discarded by an old waiter: new fetches=%d", newCalls.Load())
	}
	if oldCalls.Load() != 1 {
		t.Errorf("old waiter re-fetched stale revision after login changed: old fetches=%d", oldCalls.Load())
	}
}

func TestDoubaoCacheExpirationBackoffAndRecovery(t *testing.T) {
	var calls atomic.Int32
	first, refreshed := catalogFixture(t, "Cached model"), catalogFixture(t, "Refreshed model")
	c := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return fixtureResponse(r, 200, first), nil
		case 2:
			return fixtureResponse(r, 503, `{}`), nil
		default:
			return fixtureResponse(r, 200, refreshed), nil
		}
	}))
	if _, err := c.Catalog(context.Background(), "account-a", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Catalog(context.Background(), "account-a", false); err != nil || calls.Load() != 1 {
		t.Fatal("unexpired cache unexpectedly refreshed")
	}
	ageOnlyCacheEntry(t, c, 6*time.Minute)
	for i := 0; i < 2; i++ {
		got, err := c.Catalog(context.Background(), "account-a", false)
		if err == nil || !got.Stale || got.ErrorCode == "" || len(got.Models) != 1 || got.Models[0].Name != "Cached model" {
			t.Errorf("failed refresh lost explicit stale/error state: %+v error=%v", got, err)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("failure cooldown made repeated HTTP calls: %d", calls.Load())
	}
	c.mu.Lock()
	for _, entry := range c.cache {
		entry.retryAt = time.Now().Add(-time.Second)
	}
	c.mu.Unlock()
	got, err := c.Catalog(context.Background(), "account-a", false)
	if err != nil || got.Stale || got.ErrorCode != "" || got.Models[0].Name != "Refreshed model" || calls.Load() != 3 {
		t.Fatalf("refresh did not recover after cooldown: %+v error=%v", got, err)
	}
}

func TestDoubaoAncientStaleCatalogIsWithheldAndReturnedSnapshotsAreDetached(t *testing.T) {
	var fail atomic.Bool
	response := catalogFixture(t, "Original model")
	c := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if fail.Load() {
			return fixtureResponse(r, 503, `{}`), nil
		}
		return fixtureResponse(r, 200, response), nil
	}))
	got, err := c.Catalog(context.Background(), "account-a", false)
	if err != nil {
		t.Fatal(err)
	}
	got.Models[0].Name, got.Models[0].Efforts[0].Name, got.Models[0].Tiers[0] = "Changed", "Changed", "Changed"
	got, err = c.Catalog(context.Background(), "account-a", false)
	if err != nil || got.Models[0].Name != "Original model" || got.Models[0].Efforts[0].Name == "Changed" || got.Models[0].Tiers[0] == "Changed" {
		t.Fatal("caller mutation changed cached model metadata")
	}
	ageOnlyCacheEntry(t, c, 2*time.Hour)
	fail.Store(true)
	got, err = c.Catalog(context.Background(), "account-a", false)
	if err == nil || !got.Stale || len(got.Models) != 0 {
		t.Errorf("ancient catalog was returned as usable: models=%d stale=%v error=%v", len(got.Models), got.Stale, err)
	}
}

func TestDoubaoCanceledRefreshDoesNotPoisonNextCallerBackoff(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	response := catalogFixture(t, "Healthy retry model")
	c := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return fixtureResponse(r, 200, response), nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan catalogResult, 1)
	go func() { v, err := c.Catalog(ctx, "account-a", false); result <- catalogResult{v, err} }()
	waitFixtureSignal(t, entered)
	cancel()
	if got := waitCatalog(t, result); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("cancel did not reach caller: %v", got.err)
	}
	got, err := c.Catalog(context.Background(), "account-a", false)
	if err != nil || len(got.Models) != 1 || calls.Load() != 2 {
		t.Errorf("canceled request was cached as an upstream outage: calls=%d error=%v", calls.Load(), err)
	}
}

func TestDoubaoCanceledWaiterDoesNotCancelSharedLeader(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	response := catalogFixture(t, "Still loading")
	var calls atomic.Int32
	c := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return fixtureResponse(r, 200, response), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}))
	leaderContext, stopLeader := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopLeader()
	leader := make(chan catalogResult, 1)
	go func() { v, err := c.Catalog(leaderContext, "account-a", false); leader <- catalogResult{v, err} }()
	waitFixtureSignal(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	waiterContext := observedContext(ctx)
	waiter := make(chan catalogResult, 1)
	go func() { v, err := c.Catalog(waiterContext, "account-a", false); waiter <- catalogResult{v, err} }()
	waitFixtureSignal(t, waiterContext.observed)
	cancel()
	if result := waitCatalog(t, waiter); !errors.Is(result.err, context.Canceled) {
		t.Errorf("waiter cancellation=%v", result.err)
	}
	close(release)
	if result := waitCatalog(t, leader); result.err != nil || len(result.catalog.Models) != 1 {
		t.Errorf("waiter canceled shared leader: %v", result.err)
	}
	if calls.Load() != 1 {
		t.Errorf("waiter created another HTTP request: %d", calls.Load())
	}
}

type canceledFixtureBody struct {
	ctx     context.Context
	entered chan struct{}
	closed  atomic.Bool
	once    sync.Once
}

func (b *canceledFixtureBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *canceledFixtureBody) Close() error { b.closed.Store(true); return nil }

func TestDoubaoCancellationDuringResponseReadIsNotAResponseLimitFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &canceledFixtureBody{ctx: ctx, entered: make(chan struct{})}
	c := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: body, Request: r}, nil
	}))
	result := make(chan catalogResult, 1)
	go func() { v, err := c.Catalog(ctx, "account-a", false); result <- catalogResult{v, err} }()
	waitFixtureSignal(t, body.entered)
	cancel()
	if got := waitCatalog(t, result); !errors.Is(got.err, context.Canceled) {
		t.Errorf("body-read cancellation misclassified: %v", got.err)
	}
	if !body.closed.Load() {
		t.Error("canceled response body was not closed")
	}
}

func TestDoubaoSessionLookupCancellationIsNotReportedAsExpiredLogin(t *testing.T) {
	for _, operation := range []string{"catalog", "activities"} {
		t.Run(operation, func(t *testing.T) {
			var requests atomic.Int32
			c := New(sessionSourceFunc(func(context.Context, string) (Session, error) { return Session{}, context.Canceled }), syntheticTransport(func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				return fixtureResponse(r, 500, `{}`), nil
			}))
			var err error
			if operation == "catalog" {
				_, err = c.Catalog(context.Background(), "account-a", false)
			} else {
				_, err = c.Activities(context.Background(), "account-a")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("cancellation incorrectly became a login failure: %v", err)
			}
			if requests.Load() != 0 {
				t.Error("failed session lookup still made an upstream request")
			}
		})
	}
}

func TestDoubaoPartiallyRestoredSessionCannotPanicOnNilHeaders(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Error("partially restored Session caused a client panic")
		}
	}()
	response := catalogFixture(t, "Synthetic catalog")
	c := New(sessionSourceFunc(func(_ context.Context, account string) (Session, error) {
		session := fixtureSession(account, "revision-a")
		session.Headers = nil
		return session, nil
	}), syntheticTransport(func(r *http.Request) (*http.Response, error) { return fixtureResponse(r, 200, response), nil }))
	// Either a structured session rejection or a safe request with initialized
	// headers is acceptable; a panic can strand the single-flight loading entry.
	_, _ = c.Catalog(context.Background(), "account-a", false)
}
