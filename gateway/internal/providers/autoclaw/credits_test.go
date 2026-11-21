package autoclaw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providercatalog"
)

func resetCreditCacheForTest(t *testing.T) {
	t.Helper()
	autoClawCreditCache.Lock()
	autoClawCreditCache.rows = map[string]creditCacheEntry{}
	autoClawCreditCache.inflight = map[string]*creditFlight{}
	autoClawCreditCache.Unlock()
	t.Cleanup(func() {
		autoClawCreditCache.Lock()
		autoClawCreditCache.rows = map[string]creditCacheEntry{}
		autoClawCreditCache.inflight = map[string]*creditFlight{}
		autoClawCreditCache.Unlock()
	})
}

func waitForCreditRefreshIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		autoClawCreditCache.Lock()
		idle := len(autoClawCreditCache.inflight) == 0
		autoClawCreditCache.Unlock()
		if idle && len(creditRefreshSlots) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("credit refreshes did not become idle")
}

func creditFixtureVault(t *testing.T, access, refresh string) (*accountvault.Vault, accountvault.Account) {
	t.Helper()
	v := mustProviderVault(t)
	body, err := json.Marshal(stored{AccessToken: access, RefreshToken: refresh})
	if err != nil {
		t.Fatal(err)
	}
	a, err := v.Put(accountvault.Account{ID: "autoclaw-credit-fixture", Provider: "autoclaw", Enabled: true}, body, 0)
	if err != nil {
		t.Fatal(err)
	}
	return v, a
}

func creditResponse(r *http.Request, status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func TestFetchCreditUsesBothWalletEndpointsAndPreservesZero(t *testing.T) {
	var calls atomic.Int32
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Fatalf("credit method=%s", r.Method)
		}
		if r.Header.Get("X-Authorization") != "Bearer fixture-access" {
			t.Fatal("credit request omitted access header")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/wallets"):
			return creditResponse(r, http.StatusOK, "{\"code\":0,\"data\":{\"total_balance\":0}}")
		case strings.HasSuffix(r.URL.Path, "/expiring"):
			return creditResponse(r, http.StatusOK, "{\"code\":0,\"data\":{\"expiring_points\":12,\"expiring_points_text\":\"fixture-expiry\"}}")
		default:
			t.Fatalf("unexpected credit path %s", r.URL.Path)
			return nil, nil
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, _ := rt.RoundTrip(r)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer server.Close()
	view, status, err := fetchCreditWith(t.Context(), "fixture-access", &http.Client{}, server.URL)
	if err != nil || status != http.StatusOK {
		t.Fatalf("fetch credit status=%d err=%v", status, err)
	}
	if view.Remain == nil || *view.Remain != 0 || view.Expiring == nil || *view.Expiring != 12 || view.Expiry != nil || view.ExpiringText != "fixture-expiry" {
		t.Fatalf("credit view lost values: %+v", view)
	}
	if calls.Load() != 2 {
		t.Fatalf("credit endpoint calls=%d want 2", calls.Load())
	}
}

func TestCreditsCacheCoalescesForcedRefreshAndServesCachedValue(t *testing.T) {
	resetCreditCacheForTest(t)
	v, account := creditFixtureVault(t, "fixture-access", "")
	var calls atomic.Int32
	enteredWallet, releaseWallet := make(chan struct{}, 1), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseWallet) })
	t.Cleanup(release)
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/wallets") {
			select {
			case enteredWallet <- struct{}{}:
			default:
			}
			<-releaseWallet
			return creditResponse(r, http.StatusOK, "{\"code\":0,\"data\":{\"total_balance\":7}}")
		}
		return creditResponse(r, http.StatusOK, "{\"code\":0,\"data\":{\"expiring_points\":3,\"expiring_points_text\":\"fixture\"}}")
	})
	s := Source{Vault: v, Client: &http.Client{Transport: rt}}
	for i := 0; i < 20; i++ {
		view, err := s.Credits(t.Context(), account.ID, false)
		if err != nil || !view.Pending {
			t.Fatalf("background credit read err=%v view=%+v", err, view)
		}
	}
	awaitCreditSignal(t, enteredWallet)
	var wg sync.WaitGroup
	results := make(chan int64, 2)
	joined := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			view, err := s.Credits(joinCreditContext{Context: t.Context(), joined: joined}, account.ID, true)
			if err != nil || view.Remain == nil {
				t.Errorf("forced credits err=%v view=%+v", err, view)
				return
			}
			results <- *view.Remain
		}()
	}
	awaitCreditSignal(t, joined)
	awaitCreditSignal(t, joined)
	release()
	wg.Wait()
	waitForCreditRefreshIdle(t)
	close(results)
	for value := range results {
		if value != 7 {
			t.Fatalf("cached credit value=%d want 7", value)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("coalesced refresh made %d endpoint calls want 2", calls.Load())
	}
	view, err := s.Credits(t.Context(), account.ID, false)
	if err != nil || view.Remain == nil || *view.Remain != 7 {
		t.Fatalf("cached read err=%v view=%+v", err, view)
	}
	if calls.Load() != 2 {
		t.Fatalf("cached read made new endpoint calls: %d", calls.Load())
	}
}

func TestCreditsRefreshesUnauthorizedSessionBeforeRetryingWallet(t *testing.T) {
	resetCreditCacheForTest(t)
	v, account := creditFixtureVault(t, "fixture-old-access", "fixture-old-refresh")
	var walletCalls, refreshCalls atomic.Int32
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/wallets"):
			if walletCalls.Add(1) == 1 {
				return creditResponse(r, http.StatusUnauthorized, "{\"code\":401}")
			}
			if r.Header.Get("X-Authorization") != "Bearer fixture-new-access" {
				t.Fatal("wallet retry did not use refreshed access")
			}
			return creditResponse(r, http.StatusOK, "{\"code\":0,\"data\":{\"total_balance\":9}}")
		case r.URL.Path == refreshPt:
			refreshCalls.Add(1)
			return creditResponse(r, http.StatusOK, "{\"code\":0,\"data\":{\"access_token\":\"fixture-new-access\",\"refresh_token\":\"fixture-new-refresh\"}}")
		case strings.HasSuffix(r.URL.Path, "/expiring"):
			return creditResponse(r, http.StatusOK, "{\"code\":0,\"data\":{\"expiring_points\":2,\"expiring_points_text\":\"fixture\"}}")
		default:
			t.Fatalf("unexpected refresh fixture path %s", r.URL.Path)
			return nil, nil
		}
	})
	client := &http.Client{Transport: rt}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, _ := rt.RoundTrip(r)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer server.Close()
	s := Source{Vault: v, Client: client, BaseURL: server.URL}
	view, err := s.Credits(t.Context(), account.ID, true)
	if err != nil || view.Remain == nil || *view.Remain != 9 {
		t.Fatalf("refreshed credits err=%v view=%+v", err, view)
	}
	if walletCalls.Load() != 2 || refreshCalls.Load() != 1 {
		t.Fatalf("wallet calls=%d refresh calls=%d want 2/1", walletCalls.Load(), refreshCalls.Load())
	}
	_, plain, err := v.Get(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer wipe(plain)
	var saved stored
	if json.Unmarshal(plain, &saved) != nil || saved.AccessToken != "fixture-new-access" || saved.RefreshToken != "fixture-new-refresh" || saved.RefreshPending {
		t.Fatal("refreshed session not committed")
	}
}

func TestFetchCreditWalletAuthenticationBusinessCodeKeepsHTTP200(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/wallets") {
			t.Error("authentication failure must not query expiring points")
		}
		return creditResponse(r, http.StatusOK, `{"code":410000,"msg":"fixture authentication required"}`)
	})}
	view, status, err := fetchCreditWith(t.Context(), "fixture-access", client, "")
	if status != http.StatusOK || err == nil || err.Error() != "autoclaw_credit_authentication_required" || view.Remain != nil {
		t.Fatalf("wallet business authentication status=%d err=%v remain_known=%t", status, err, view.Remain != nil)
	}
}

func TestCreditsRefreshesWalletBusinessAuthenticationBeforeRetry(t *testing.T) {
	resetCreditCacheForTest(t)
	v, account := creditFixtureVault(t, "fixture-old-access", "fixture-old-refresh")
	var wallets, refreshes, expirings atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/wallets"):
			if wallets.Add(1) == 1 {
				return creditResponse(r, http.StatusOK, `{"code":410000,"msg":"fixture authentication required"}`)
			}
			if r.Header.Get("X-Authorization") != "Bearer fixture-new-access" {
				t.Error("wallet retry did not use the renewed session")
			}
			return creditResponse(r, http.StatusOK, `{"code":0,"data":{"total_balance":9}}`)
		case r.URL.Path == refreshPt:
			refreshes.Add(1)
			metadata, profile := readRefreshState(t, v, account.ID)
			if metadata.Revision <= account.Revision || profile["refresh_pending"] != true {
				t.Error("wallet refresh was sent before its durable intent")
			}
			return refreshSuccess(r), nil
		case strings.HasSuffix(r.URL.Path, "/expiring"):
			expirings.Add(1)
			return creditResponse(r, http.StatusOK, `{"code":0,"data":{"expiring_points":2}}`)
		default:
			return nil, errors.New("fixture_unexpected_endpoint")
		}
	})}
	s := Source{Vault: v, Client: client}
	view, err := s.Credits(t.Context(), account.ID, true)
	if err != nil || view.Remain == nil || *view.Remain != 9 || view.Expiring == nil || *view.Expiring != 2 {
		t.Fatalf("business authentication did not recover err=%v remain_known=%t", err, view.Remain != nil)
	}
	if wallets.Load() != 2 || refreshes.Load() != 1 || expirings.Load() != 1 {
		t.Fatalf("endpoint counts wallet=%d refresh=%d expiring=%d", wallets.Load(), refreshes.Load(), expirings.Load())
	}
	_, saved := readRefreshState(t, v, account.ID)
	if saved["accessToken"] != "fixture-new-access" || saved["refreshToken"] != "fixture-new-refresh" || saved["refresh_pending"] == true {
		t.Fatal("renewed session was not committed")
	}
}

func TestCreditsWalletBusinessAuthenticationRetriesOnlyOnce(t *testing.T) {
	resetCreditCacheForTest(t)
	v, account := creditFixtureVault(t, "fixture-old-access", "fixture-old-refresh")
	var wallets, refreshes atomic.Int32
	s := Source{Vault: v, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == refreshPt {
			refreshes.Add(1)
			return refreshSuccess(r), nil
		}
		if strings.HasSuffix(r.URL.Path, "/wallets") {
			wallets.Add(1)
			return creditResponse(r, http.StatusOK, `{"code":410000}`)
		}
		return nil, errors.New("fixture_unexpected_endpoint")
	})}}
	view, err := s.Credits(t.Context(), account.ID, true)
	if err == nil || view.Remain != nil || wallets.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("unbounded or missing retry err=%v wallets=%d refreshes=%d", err, wallets.Load(), refreshes.Load())
	}
}

func TestCreditsWalletBusinessAuthenticationRefreshLostResponseIsNotReplayed(t *testing.T) {
	resetCreditCacheForTest(t)
	v, account := creditFixtureVault(t, "fixture-old-access", "fixture-old-refresh")
	var wallets, refreshes atomic.Int32
	s := Source{Vault: v, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == refreshPt {
			refreshes.Add(1)
			return nil, errors.New("fixture_response_lost")
		}
		if strings.HasSuffix(r.URL.Path, "/wallets") {
			wallets.Add(1)
			return creditResponse(r, http.StatusOK, `{"code":410000}`)
		}
		return nil, errors.New("fixture_unexpected_endpoint")
	})}}
	for range 2 {
		if _, err := s.Credits(t.Context(), account.ID, true); err == nil {
			t.Error("uncertain refresh was reported successful")
		}
	}
	_, saved := readRefreshState(t, v, account.ID)
	if wallets.Load() != 1 || refreshes.Load() != 1 || saved["refresh_pending"] != true {
		t.Fatalf("ambiguous single-use refresh replayed wallets=%d refreshes=%d pending=%v", wallets.Load(), refreshes.Load(), saved["refresh_pending"])
	}
}

func TestCreditsDoesNotRefreshOtherWalletFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		expiring bool
	}{
		{"unknown business code", http.StatusOK, `{"code":410001}`, false},
		{"other business error with token words", http.StatusOK, `{"code":1,"msg":"fixture token expired"}`, false},
		{"quota forbidden", http.StatusForbidden, `{"code":403,"msg":"fixture quota exhausted"}`, false},
		{"other forbidden", http.StatusForbidden, `{"msg":"fixture token expired"}`, false},
		{"invalid JSON", http.StatusOK, `{`, false},
		{"missing business code", http.StatusOK, `{"data":{"total_balance":7}}`, false},
		{"expiring auth does not discard known wallet", http.StatusOK, `{"code":410000}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetCreditCacheForTest(t)
			v, account := creditFixtureVault(t, "fixture-old-access", "fixture-old-refresh")
			var refreshes atomic.Int32
			s := Source{Vault: v, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == refreshPt {
					refreshes.Add(1)
					return refreshSuccess(r), nil
				}
				if tc.expiring && strings.HasSuffix(r.URL.Path, "/wallets") {
					return creditResponse(r, http.StatusOK, `{"code":0,"data":{"total_balance":7}}`)
				}
				return creditResponse(r, tc.status, tc.body)
			})}}
			view, err := s.Credits(t.Context(), account.ID, true)
			metadata, saved := readRefreshState(t, v, account.ID)
			if err == nil || refreshes.Load() != 0 || metadata.Revision != account.Revision || saved["refresh_pending"] == true {
				t.Fatalf("non-wallet-auth error consumed refresh err=%v calls=%d", err, refreshes.Load())
			}
			if tc.expiring && (view.Remain == nil || *view.Remain != 7 || view.FetchedAt == nil || view.ErrorCode != "autoclaw_expiring_unavailable") {
				t.Fatal("expiring failure discarded the confirmed wallet balance")
			}
		})
	}
}

func TestCreditsWithoutRefreshDoesNotRetryBusinessAuthentication(t *testing.T) {
	resetCreditCacheForTest(t)
	v, account := creditFixtureVault(t, "fixture-old-access", "")
	var calls atomic.Int32
	s := Source{Vault: v, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return creditResponse(r, http.StatusOK, `{"code":410000}`)
	})}}
	if _, err := s.Credits(t.Context(), account.ID, true); err == nil || calls.Load() != 1 {
		t.Fatalf("missing refresh session calls=%d err=%v", calls.Load(), err)
	}
}

func TestCreditsConcurrentBusinessAuthenticationRefreshCoalesces(t *testing.T) {
	resetCreditCacheForTest(t)
	v, account := creditFixtureVault(t, "fixture-old-access", "fixture-old-refresh")
	var wallets, refreshes atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	s := Source{Vault: v, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == refreshPt:
			refreshes.Add(1)
			return refreshSuccess(r), nil
		case strings.HasSuffix(r.URL.Path, "/wallets"):
			if wallets.Add(1) == 1 {
				close(entered)
				<-release
				return creditResponse(r, http.StatusOK, `{"code":410000}`)
			}
			return creditResponse(r, http.StatusOK, `{"code":0,"data":{"total_balance":9}}`)
		case strings.HasSuffix(r.URL.Path, "/expiring"):
			return creditResponse(r, http.StatusOK, `{"code":0,"data":{"expiring_points":0}}`)
		default:
			return nil, errors.New("fixture_unexpected_endpoint")
		}
	})}}
	const callers = 8
	results := make(chan error, callers)
	read := func(ctx context.Context) {
		view, err := s.Credits(ctx, account.ID, true)
		if err == nil && (view.Remain == nil || *view.Remain != 9) {
			err = errors.New("fixture_missing_balance")
		}
		results <- err
	}
	go read(t.Context())
	awaitCreditSignal(t, entered)
	joined := make(chan struct{}, callers-1)
	for range callers - 1 {
		go read(joinCreditContext{Context: t.Context(), joined: joined})
	}
	for range callers - 1 {
		awaitCreditSignal(t, joined)
	}
	releaseOnce()
	for range callers {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("coalesced wallet read: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("coalesced wallet read timed out")
		}
	}
	if wallets.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("coalesced wallet=%d refresh=%d", wallets.Load(), refreshes.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

type joinCreditContext struct {
	context.Context
	joined chan<- struct{}
}

func (c joinCreditContext) Done() <-chan struct{} {
	select {
	case c.joined <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func awaitCreditSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("credit refresh did not reach its expected state")
	}
}

func TestCreditsBacksOffFailuresAndRetainsLastGoodBalance(t *testing.T) {
	resetCreditCacheForTest(t)
	v, account := creditFixtureVault(t, "fixture-access", "")
	key := v.OpaqueID("credit-cache", account.ID)
	var walletCalls atomic.Int32
	var walletDown atomic.Bool
	walletDown.Store(true)
	s := Source{Vault: v, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/wallets") {
			walletCalls.Add(1)
			if walletDown.Load() {
				return creditResponse(r, http.StatusBadGateway, "{}")
			}
			return creditResponse(r, http.StatusOK, `{"code":0,"data":{"total_balance":7}}`)
		}
		return creditResponse(r, http.StatusOK, `{"code":0,"data":{"expiring_points":0}}`)
	})}}
	read := func() providercatalog.CreditView {
		t.Helper()
		view, err := s.Credits(t.Context(), account.ID, false)
		if err != nil {
			t.Fatalf("credit snapshot: %v", err)
		}
		return view
	}
	expire := func(ttl time.Duration) {
		t.Helper()
		autoClawCreditCache.Lock()
		defer autoClawCreditCache.Unlock()
		entry := autoClawCreditCache.rows[key]
		if entry.retryAt.Sub(entry.at) != ttl {
			t.Fatalf("cache TTL = %s, want %s", entry.retryAt.Sub(entry.at), ttl)
		}
		entry.retryAt = time.Now()
		autoClawCreditCache.rows[key] = entry
	}
	if got := read(); !got.Pending {
		t.Fatalf("cold snapshot = %+v", got)
	}
	waitForCreditRefreshIdle(t)
	for i := 0; i < 20; i++ {
		if got := read(); got.Pending || got.ErrorCode != "autoclaw_credit_unavailable" {
			t.Fatalf("negative cache snapshot = %+v", got)
		}
	}
	if got := walletCalls.Load(); got != 1 {
		t.Fatalf("wallet calls during backoff = %d", got)
	}
	walletDown.Store(false)
	expire(creditFailureBackoff)
	if got := read(); !got.Pending {
		t.Fatalf("expired negative cache should retry: %+v", got)
	}
	waitForCreditRefreshIdle(t)
	good := read()
	if good.Remain == nil || *good.Remain != 7 || good.FetchedAt == nil || good.ErrorCode != "" {
		t.Fatalf("failure was not replaced by successful refresh: %+v", good)
	}
	walletDown.Store(true)
	expire(creditCacheTTL)
	read()
	waitForCreditRefreshIdle(t)
	for i := 0; i < 20; i++ {
		got := read()
		if got.Remain == nil || *got.Remain != 7 || got.FetchedAt == nil || !got.FetchedAt.Equal(*good.FetchedAt) || got.ErrorCode != "autoclaw_credit_unavailable" {
			t.Fatalf("last known balance lost: %+v", got)
		}
	}
	if got := walletCalls.Load(); got != 3 {
		t.Fatalf("wallet calls across failure/retry/success = %d", got)
	}
}

func TestCreditPartialKeepsWalletWhenExpiringUnavailable(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"error code", `{"code":1}`},
		{"amount missing", `{"code":0,"data":{"expiring_points_text":"pending message"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetCreditCacheForTest(t)
			v, account := creditFixtureVault(t, "fixture-access", "")
			var calls atomic.Int32
			s := Source{Vault: v, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if strings.HasSuffix(r.URL.Path, "/wallets") {
					return creditResponse(r, http.StatusOK, `{"code":0,"data":{"total_balance":0}}`)
				}
				return creditResponse(r, http.StatusOK, tc.body)
			})}}
			view, err := s.Credits(t.Context(), account.ID, true)
			if err == nil || view.Remain == nil || *view.Remain != 0 || view.Expiring != nil || view.ErrorCode != "autoclaw_expiring_unavailable" || view.FetchedAt == nil {
				t.Fatalf("partial wallet view = %+v, err = %v", view, err)
			}
			cached, err := s.Credits(t.Context(), account.ID, false)
			if err != nil || cached.Remain == nil || *cached.Remain != 0 || cached.ErrorCode != view.ErrorCode || calls.Load() != 2 {
				t.Fatalf("partial wallet cache = %+v, err = %v, calls = %d", cached, err, calls.Load())
			}
		})
	}
}

func TestCreditsDoesNotCacheCanceledSlotWait(t *testing.T) {
	resetCreditCacheForTest(t)
	v, account := creditFixtureVault(t, "fixture-access", "")
	for i := 0; i < cap(creditRefreshSlots); i++ {
		creditRefreshSlots <- struct{}{}
	}
	release := sync.OnceFunc(func() {
		for i := 0; i < cap(creditRefreshSlots); i++ {
			<-creditRefreshSlots
		}
	})
	t.Cleanup(release)
	var calls atomic.Int32
	s := Source{Vault: v, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/wallets") {
			return creditResponse(r, http.StatusOK, `{"code":0,"data":{"total_balance":7}}`)
		}
		return creditResponse(r, http.StatusOK, `{"code":0,"data":{"expiring_points":0}}`)
	})}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	queued := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() {
		view, err := s.Credits(joinCreditContext{Context: ctx, joined: queued}, account.ID, true)
		if !view.Pending || view.ErrorCode != "credit_pending" {
			t.Errorf("canceled slot wait view = %+v", view)
		}
		result <- err
	}()
	awaitCreditSignal(t, queued)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("slot wait returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slot wait ignored cancellation")
	}
	key := v.OpaqueID("credit-cache", account.ID)
	autoClawCreditCache.Lock()
	_, cached := autoClawCreditCache.rows[key]
	_, inflight := autoClawCreditCache.inflight[key]
	autoClawCreditCache.Unlock()
	if cached || inflight || calls.Load() != 0 {
		t.Fatalf("canceled slot wait cached=%t inflight=%t calls=%d", cached, inflight, calls.Load())
	}
	release()
	_, _ = s.Credits(t.Context(), account.ID, false)
	waitForCreditRefreshIdle(t)
	view, err := s.Credits(t.Context(), account.ID, false)
	if err != nil || view.Remain == nil || *view.Remain != 7 || calls.Load() != 2 {
		t.Fatalf("normal read after cancel view=%+v err=%v calls=%d", view, err, calls.Load())
	}
}

func TestCreditsPreservesCachedBalanceWhenManualRequestIsCanceled(t *testing.T) {
	resetCreditCacheForTest(t)
	v, account := creditFixtureVault(t, "fixture-access", "")
	started := make(chan struct{})
	var block atomic.Bool
	s := Source{Vault: v, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/wallets") {
			if block.Load() {
				close(started)
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return creditResponse(r, http.StatusOK, `{"code":0,"data":{"total_balance":7}}`)
		}
		return creditResponse(r, http.StatusOK, `{"code":0,"data":{"expiring_points":0}}`)
	})}}
	view, err := s.Credits(t.Context(), account.ID, true)
	if err != nil || view.Remain == nil || *view.Remain != 7 || view.FetchedAt == nil {
		t.Fatalf("initial refresh view=%+v err=%v", view, err)
	}
	key := v.OpaqueID("credit-cache", account.ID)
	autoClawCreditCache.Lock()
	before := autoClawCreditCache.rows[key]
	autoClawCreditCache.Unlock()
	block.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err := s.Credits(ctx, account.ID, true)
		if !errors.Is(err, context.Canceled) || !got.Pending {
			t.Errorf("canceled refresh view=%+v err=%v", got, err)
		}
	}()
	awaitCreditSignal(t, started)
	cancel()
	awaitCreditSignal(t, done)
	autoClawCreditCache.Lock()
	after := autoClawCreditCache.rows[key]
	autoClawCreditCache.Unlock()
	if after.value.ErrorCode != "" || after.value.Pending || !after.at.Equal(before.at) || !after.retryAt.Equal(before.retryAt) {
		t.Fatalf("canceled refresh changed cache: %+v", after)
	}
	got, err := s.Credits(t.Context(), account.ID, false)
	if err != nil || got.Remain == nil || *got.Remain != 7 || got.FetchedAt == nil || !got.FetchedAt.Equal(*view.FetchedAt) {
		t.Fatalf("cached balance view=%+v err=%v", got, err)
	}
}

func TestBackgroundAndManualRefreshesShareGlobalConcurrencyLimit(t *testing.T) {
	resetCreditCacheForTest(t)
	v := mustProviderVault(t)
	accounts := make([]accountvault.Account, 8)
	body, err := json.Marshal(stored{AccessToken: "fixture-access"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range accounts {
		account, putErr := v.Put(accountvault.Account{ID: fmt.Sprintf("autoclaw-credit-%d", i), Provider: "autoclaw", Enabled: true}, body, 0)
		if putErr != nil {
			t.Fatal(putErr)
		}
		accounts[i] = account
	}
	var current, maximum atomic.Int32
	walletStart := make(chan struct{}, len(accounts))
	releaseWallets := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseWallets) })
	t.Cleanup(release)
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		now := current.Add(1)
		defer current.Add(-1)
		for {
			old := maximum.Load()
			if now <= old || maximum.CompareAndSwap(old, now) {
				break
			}
		}
		if strings.HasSuffix(r.URL.Path, "/wallets") {
			walletStart <- struct{}{}
			<-releaseWallets
			return creditResponse(r, http.StatusOK, `{"code":0,"data":{"total_balance":7}}`)
		}
		return creditResponse(r, http.StatusOK, `{"code":0,"data":{"expiring_points":3,"expiring_points_text":"fixture"}}`)
	})
	s := Source{Vault: v, Client: &http.Client{Transport: rt}}
	const backgroundAccounts = maxConcurrentCreditRefreshes / 2
	for _, account := range accounts[:backgroundAccounts] {
		view, err := s.Credits(t.Context(), account.ID, false)
		if err != nil || !view.Pending {
			t.Fatalf("background credit view=%+v err=%v", view, err)
		}
		awaitCreditSignal(t, walletStart)
	}
	var wg sync.WaitGroup
	queued := make(chan struct{}, len(accounts))
	for _, account := range accounts[backgroundAccounts:] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			view, refreshErr := s.Credits(joinCreditContext{Context: t.Context(), joined: queued}, account.ID, true)
			if refreshErr != nil || view.Remain == nil || *view.Remain != 7 {
				t.Errorf("forced credit err=%v view=%+v", refreshErr, view)
			}
		}()
	}
	for range accounts[backgroundAccounts:] {
		awaitCreditSignal(t, queued)
	}
	for i := backgroundAccounts; i < maxConcurrentCreditRefreshes; i++ {
		awaitCreditSignal(t, walletStart)
	}
	if active := current.Load(); active != maxConcurrentCreditRefreshes {
		t.Errorf("wallet requests before release=%d want %d", active, maxConcurrentCreditRefreshes)
	}
	release()
	wg.Wait()
	waitForCreditRefreshIdle(t)
	if got := maximum.Load(); got != maxConcurrentCreditRefreshes {
		t.Fatalf("peak upstream concurrent requests=%d want %d", got, maxConcurrentCreditRefreshes)
	}
}

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
