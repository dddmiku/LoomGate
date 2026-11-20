package autoclaw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
)

type refreshTransport func(*http.Request) (*http.Response, error)

func (f refreshTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func refreshFixture(t *testing.T) (*accountvault.Vault, accountvault.Account) {
	t.Helper()
	v := mustProviderVault(t)
	profile, _ := json.Marshal(stored{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh"})
	a, err := v.Put(accountvault.Account{ID: "autoclaw-fixture", Provider: "autoclaw", Enabled: true}, profile, 0)
	if err != nil {
		t.Fatal(err)
	}
	return v, a
}

// Guard the default client too: older implementations ignored Provider.Client.
// No assertion failure in these tests may accidentally reach an external host.
func offlineRefreshProvider(t *testing.T, v *accountvault.Vault, exchange refreshTransport) *Provider {
	t.Helper()
	checked := refreshTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != apiBase || (r.URL.Path != refreshPt && r.URL.Path != chatPath) {
			t.Error("unexpected request at the offline transport boundary")
			return nil, errors.New("fixture_unexpected_request")
		}
		return exchange(r)
	})
	previous := http.DefaultTransport
	http.DefaultTransport = refreshTransport(func(r *http.Request) (*http.Response, error) {
		t.Error("provider bypassed its injected transport")
		return checked.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	return &Provider{Vault: v, Client: &http.Client{Transport: checked}}
}

func refreshSuccess(r *http.Request) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh"}}`)), Request: r}
}

func readRefreshState(t *testing.T, v *accountvault.Vault, id string) (accountvault.Account, map[string]any) {
	t.Helper()
	a, plain, err := v.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	defer wipe(plain)
	var profile map[string]any
	if json.Unmarshal(plain, &profile) != nil {
		t.Fatal("invalid encrypted fixture profile")
	}
	return a, profile
}

func TestRefreshWritesIntentBeforeHTTPAndUsesInjectedTransport(t *testing.T) {
	v, original := refreshFixture(t)
	var calls atomic.Int32
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		a, profile := readRefreshState(t, v, original.ID)
		if a.Revision <= original.Revision || profile["refresh_pending"] != true {
			t.Error("refresh was submitted before its intent was durable")
		}
		return refreshSuccess(r), nil
	})
	if _, err := p.refreshAccount(t.Context(), original.ID, "fixture-old-refresh"); err != nil {
		t.Fatal(err)
	}
	_, profile := readRefreshState(t, v, original.ID)
	if profile["accessToken"] != "fixture-new-access" || profile["refreshToken"] != "fixture-new-refresh" || profile["refresh_pending"] == true {
		t.Fatal("successful refresh did not persist the new pair and clear its intent")
	}
	if calls.Load() != 1 {
		t.Fatalf("exchange count = %d, want 1", calls.Load())
	}
}

func TestRefreshLostResponseIsNotReplayedByANewProvider(t *testing.T) {
	v, a := refreshFixture(t)
	var calls atomic.Int32
	p := offlineRefreshProvider(t, v, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("fixture_response_lost")
	})
	if _, err := p.refreshAccount(t.Context(), a.ID, "fixture-old-refresh"); err == nil {
		t.Fatal("lost response was reported as success")
	}
	p2 := &Provider{Vault: v, Client: p.Client}
	if _, err := p2.refreshAccount(t.Context(), a.ID, "fixture-old-refresh"); err == nil {
		t.Fatal("pending refresh was reported as success")
	}
	if calls.Load() != 1 {
		t.Fatalf("lost-response refresh was replayed: %d exchanges", calls.Load())
	}
	_, profile := readRefreshState(t, v, a.ID)
	if profile["refresh_pending"] != true {
		t.Fatal("ambiguous refresh intent was not preserved")
	}
}

func TestRefreshDoesNotReenableAnAccountDisabledDuringExchange(t *testing.T) {
	v, a := refreshFixture(t)
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		current, _ := readRefreshState(t, v, a.ID)
		if _, err := v.SetEnabled(a.ID, false, current.Revision); err != nil {
			t.Fatal(err)
		}
		return refreshSuccess(r), nil
	})
	if _, err := p.refreshAccount(t.Context(), a.ID, "fixture-old-refresh"); err == nil {
		t.Error("refresh overwrote a concurrent account revision")
	}
	current, _ := readRefreshState(t, v, a.ID)
	if current.Enabled {
		t.Fatal("refresh re-enabled an account disabled by the operator")
	}
}

func TestRefreshCoalescesAnAlreadyRotatedTokenWithoutHTTP(t *testing.T) {
	v, a := refreshFixture(t)
	profile, _ := json.Marshal(stored{AccessToken: "fixture-new-access", RefreshToken: "fixture-new-refresh"})
	if _, err := v.Put(a, profile, a.Revision); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return refreshSuccess(r), nil
	})
	fresh, err := p.refreshAccount(context.Background(), a.ID, "fixture-old-refresh")
	if err != nil || fresh.AccessToken != "fixture-new-access" {
		t.Fatal("did not adopt the already persisted session")
	}
	if calls.Load() != 0 {
		t.Fatalf("stale request rotated a token again: %d exchanges", calls.Load())
	}
}

func TestRefreshPendingSurvivesVaultReopenAndPreventsChat(t *testing.T) {
	dir := t.TempDir()
	v, err := accountvault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	profile, _ := json.Marshal(stored{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh"})
	a, err := v.Put(accountvault.Account{ID: "autoclaw-fixture", Provider: "autoclaw", Enabled: true}, profile, 0)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	p := offlineRefreshProvider(t, v, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("fixture_response_lost")
	})
	_, _ = p.refreshAccount(t.Context(), a.ID, "fixture-old-refresh")
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := accountvault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	p2 := &Provider{Vault: reopened, Client: p.Client}
	if _, err := p2.refreshAccount(t.Context(), a.ID, "fixture-old-refresh"); err == nil {
		t.Error("reopened vault forgot pending refresh")
	}
	if _, err := p2.session(a.ID); err == nil {
		t.Error("chat accepted an uncertain credential chain")
	}
	if calls.Load() != 1 {
		t.Fatal("restart replayed the refresh")
	}
}

func TestRefreshConcurrentProvidersHaveOneExchange(t *testing.T) {
	v, a := refreshFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) != 1 {
			return nil, errors.New("fixture_duplicate_exchange")
		}
		close(entered)
		<-release
		return refreshSuccess(r), nil
	})
	first := make(chan error, 1)
	go func() { _, err := p.refreshAccount(t.Context(), a.ID, "fixture-old-refresh"); first <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("first exchange never reached offline transport")
	}
	p2 := &Provider{Vault: v, Client: p.Client}
	_, secondErr := p2.refreshAccount(t.Context(), a.ID, "fixture-old-refresh")
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if secondErr == nil {
		t.Error("concurrent pending refresh was not blocked")
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent providers consumed a token twice")
	}
	if _, err := p2.refreshAccount(t.Context(), a.ID, "fixture-old-refresh"); err != nil {
		t.Fatal("completed exchange was not adopted")
	}
	if calls.Load() != 1 {
		t.Fatal("adopting completed exchange submitted HTTP")
	}
}

func TestRefreshDoesNotOverwriteConcurrentRelogin(t *testing.T) {
	v, a := refreshFixture(t)
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		current, _ := readRefreshState(t, v, a.ID)
		profile, _ := json.Marshal(stored{AccessToken: "fixture-login-access", RefreshToken: "fixture-login-refresh"})
		if _, err := v.Put(current, profile, current.Revision); err != nil {
			t.Fatal(err)
		}
		return refreshSuccess(r), nil
	})
	if _, err := p.refreshAccount(t.Context(), a.ID, "fixture-old-refresh"); err == nil {
		t.Error("refresh overwrote a newer login")
	}
	_, profile := readRefreshState(t, v, a.ID)
	if profile["accessToken"] != "fixture-login-access" || profile["refresh_pending"] == true {
		t.Fatal("new login was changed by late refresh response")
	}
}

func TestRefreshIntentFailureMakesNoHTTP(t *testing.T) {
	dir := t.TempDir()
	v, err := accountvault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	profile, _ := json.Marshal(stored{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh"})
	a, err := v.Put(accountvault.Account{ID: "autoclaw-fixture", Provider: "autoclaw", Enabled: true}, profile, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, a.ID+".account.pending"), []byte("fixture obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) { calls.Add(1); return refreshSuccess(r), nil })
	if _, err := p.refreshAccount(t.Context(), a.ID, "fixture-old-refresh"); err == nil {
		t.Error("intent write failure was ignored")
	}
	if calls.Load() != 0 {
		t.Fatal("HTTP sent despite failed durable intent")
	}
}

func TestRefreshCommitFailureKeepsIntentAndNeverReplays(t *testing.T) {
	dir := t.TempDir()
	v, err := accountvault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	profile, _ := json.Marshal(stored{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh"})
	a, err := v.Put(accountvault.Account{ID: "autoclaw-fixture", Provider: "autoclaw", Enabled: true}, profile, 0)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if err := os.WriteFile(filepath.Join(dir, a.ID+".account.pending"), []byte("fixture obstruction"), 0600); err != nil {
			t.Fatal(err)
		}
		return refreshSuccess(r), nil
	})
	if _, err := p.refreshAccount(t.Context(), a.ID, "fixture-old-refresh"); err == nil {
		t.Error("failed credential commit was reported successful")
	}
	_, state := readRefreshState(t, v, a.ID)
	if state["refresh_pending"] != true {
		t.Fatal("credential commit failure lost its intent")
	}
	p2 := &Provider{Vault: v, Client: p.Client}
	_, _ = p2.refreshAccount(t.Context(), a.ID, "fixture-old-refresh")
	if calls.Load() != 1 {
		t.Fatal("credential commit failure replayed HTTP")
	}
}

func TestRefreshCancellationIsBoundedAndKeepsIntent(t *testing.T) {
	v, a := refreshFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 31*time.Second {
			t.Error("refresh has no bounded request deadline")
		}
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	if _, err := p.refreshAccount(ctx, a.ID, "fixture-old-refresh"); err == nil {
		t.Error("cancelled exchange was accepted")
	}
	_, state := readRefreshState(t, v, a.ID)
	if state["refresh_pending"] != true {
		t.Fatal("cancelled submitted exchange forgot its intent")
	}
}

func TestRefreshResponseMustContainCompletePairAndExplicitSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing_code", `{"data":{"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh"}}`, 200},
		{"missing_refresh", `{"code":0,"data":{"access_token":"fixture-new-access"}}`, 200},
		{"business_failure", `{"code":9,"msg":"fixture private upstream message","data":{"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh"}}`, 200},
		{"http_failure", `{"code":0,"data":{"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh"}}`, 503},
		{"oversized", strings.Repeat(" ", 1<<20) + `{}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, a := refreshFixture(t)
			p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
				resp := refreshSuccess(r)
				resp.StatusCode, resp.Body = tc.status, io.NopCloser(strings.NewReader(tc.body))
				return resp, nil
			})
			_, err := p.refreshAccount(t.Context(), a.ID, "fixture-old-refresh")
			if err == nil {
				t.Fatal("invalid refresh response was accepted")
			}
			if strings.Contains(err.Error(), "private upstream") {
				t.Fatal("upstream private message reached error output")
			}
			_, state := readRefreshState(t, v, a.ID)
			if state["refresh_pending"] != true {
				t.Fatal("invalid exchange response cleared the intent")
			}
		})
	}
}

func TestRefreshRequestPathRetriesOnlyAfterDurablePair(t *testing.T) {
	v, a := refreshFixture(t)
	var chatCalls, refreshCalls atomic.Int32
	tr := refreshTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != apiBase {
			t.Error("unexpected host")
			return nil, errors.New("fixture_unexpected_host")
		}
		switch r.URL.Path {
		case refreshPt:
			refreshCalls.Add(1)
			_, profile := readRefreshState(t, v, a.ID)
			if profile["refresh_pending"] != true {
				t.Error("exchange has no durable intent")
			}
			return refreshSuccess(r), nil
		case chatPath:
			n := chatCalls.Add(1)
			resp := refreshSuccess(r)
			if n == 1 {
				resp.StatusCode, resp.Body = http.StatusUnauthorized, io.NopCloser(strings.NewReader(`{"msg":"token expired"}`))
			} else {
				_, profile := readRefreshState(t, v, a.ID)
				if profile["accessToken"] != "fixture-new-access" || profile["refresh_pending"] == true {
					t.Error("chat retried before durable credential commit")
				}
				resp.Body = io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"fixture answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			}
			return resp, nil
		default:
			t.Error("unexpected endpoint")
			return nil, errors.New("fixture_unexpected_endpoint")
		}
	})
	previous := http.DefaultTransport
	http.DefaultTransport = refreshTransport(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected default transport")
		return nil, errors.New("fixture_network_blocked")
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	p := &Provider{Vault: v, Client: &http.Client{Transport: tr}}
	result, err := p.turnWith(t.Context(), a, inference.Dialogue{Model: Models[0].ID, Messages: []inference.Message{{Role: "user", Content: "fixture prompt"}}}, nil, nil, nil)
	if err != nil || result.Text != "fixture answer" {
		t.Fatalf("retry did not complete: %v", err)
	}
	if refreshCalls.Load() != 1 || chatCalls.Load() != 2 {
		t.Fatal("incorrect request-path exchange count")
	}
}

func TestRefreshRequestPathPreservesPendingError(t *testing.T) {
	v, a := refreshFixture(t)
	var chats, exchanges atomic.Int32
	p := &Provider{Vault: v, Client: &http.Client{Transport: refreshTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == refreshPt {
			exchanges.Add(1)
			return nil, errors.New("fixture_lost")
		}
		chats.Add(1)
		resp := refreshSuccess(r)
		resp.StatusCode, resp.Body = 401, io.NopCloser(strings.NewReader(`{"msg":"expired"}`))
		return resp, nil
	})}}
	q := inference.Dialogue{Model: Models[0].ID, Messages: []inference.Message{{Role: "user", Content: "fixture prompt"}}}
	for range 2 {
		_, err := p.turnWith(t.Context(), a, q, nil, nil, nil)
		if public, ok := err.(interface{ PublicCode() string }); !ok || public.PublicCode() != "autoclaw_refresh_pending" {
			t.Fatal("pending refresh was hidden behind generic authentication failure")
		}
	}
	if chats.Load() != 1 || exchanges.Load() != 1 {
		t.Fatal("uncertain request path submitted more HTTP")
	}
}

func TestRefreshNeverFollowsRedirectOrMutatesCallerClient(t *testing.T) {
	v, a := refreshFixture(t)
	var calls atomic.Int32
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := refreshSuccess(r)
		resp.StatusCode = http.StatusTemporaryRedirect
		resp.Header.Set("Location", "https://fixture.invalid/never-follow")
		return resp, nil
	})
	var redirects atomic.Int32
	p.Client.CheckRedirect = func(*http.Request, []*http.Request) error { redirects.Add(1); return nil }
	if _, err := p.refreshAccount(t.Context(), a.ID, "fixture-old-refresh"); err == nil {
		t.Error("redirect was accepted")
	}
	if calls.Load() != 1 || redirects.Load() != 0 {
		t.Fatal("refresh followed redirect")
	}
	if p.Client.Timeout != 0 || p.Client.CheckRedirect == nil {
		t.Fatal("refresh mutated the shared inference client")
	}
}
