package autoclaw

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
)

func attemptDialogue() inference.Dialogue {
	return inference.Dialogue{Model: Models[0].ID, Messages: []inference.Message{{Role: "user", Content: "fixture input"}}}
}

func attemptAnswer(r *http.Request) *http.Response {
	resp := refreshSuccess(r)
	resp.Body = io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"fixture answer\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":3,\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\ndata: [DONE]\n\n"))
	return resp
}

func TestAttemptAccountingCountsBothChatHTTPCallsAroundRefresh(t *testing.T) {
	v, a := refreshFixture(t)
	var chats, exchanges atomic.Int32
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == refreshPt {
			exchanges.Add(1)
			return refreshSuccess(r), nil
		}
		if chats.Add(1) == 1 {
			resp := refreshSuccess(r)
			resp.StatusCode, resp.Body = 401, io.NopCloser(strings.NewReader(`{"msg":"token expired"}`))
			return resp, nil
		}
		return attemptAnswer(r), nil
	})
	result, err := p.CompleteDialogue(t.Context(), attemptDialogue(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if chats.Load() != 2 || exchanges.Load() != 1 {
		t.Fatal("offline fixture did not exercise renewal path")
	}
	if !result.Attempted || result.Attempts != 2 || len(result.AttemptRecords) != 2 {
		t.Fatalf("two chat HTTP calls were not retained as two attempts: attempted=%v count=%d records=%d", result.Attempted, result.Attempts, len(result.AttemptRecords))
	}
	if result.AttemptRecords[0].HTTPStatus != 401 || result.AttemptRecords[1].HTTPStatus != 200 {
		t.Fatal("attempt HTTP statuses were collapsed")
	}
	if result.AccountRef != a.ID {
		t.Fatal("successful attempt lost its account identity")
	}
}

func TestAttemptAccountingLocalPendingMakesNoHTTPAttempt(t *testing.T) {
	v, a := refreshFixture(t)
	profile, _ := json.Marshal(stored{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh", RefreshPending: true})
	if _, err := v.Put(a, profile, a.Revision); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) { calls.Add(1); return attemptAnswer(r), nil })
	result, err := p.CompleteDialogue(t.Context(), attemptDialogue(), nil)
	if err == nil {
		t.Fatal("uncertain refresh allowed a new generation")
	}
	if calls.Load() != 0 {
		t.Fatal("local guard crossed the offline HTTP boundary")
	}
	if result.Attempted || result.Attempts != 0 || len(result.AttemptRecords) != 0 {
		t.Fatalf("local rejection invented a billable HTTP attempt: attempted=%v count=%d records=%d", result.Attempted, result.Attempts, len(result.AttemptRecords))
	}
}

func TestAttemptAccountingFailedHTTPKeepsAccountAndAttempted(t *testing.T) {
	v, a := refreshFixture(t)
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		resp := refreshSuccess(r)
		resp.StatusCode, resp.Body = 503, io.NopCloser(strings.NewReader(`{"error":"fixture unavailable"}`))
		return resp, nil
	})
	result, err := p.CompleteDialogue(t.Context(), attemptDialogue(), nil)
	if err == nil {
		t.Fatal("failed HTTP was accepted")
	}
	if !result.Attempted || result.AccountRef != a.ID || result.Attempts != 1 || len(result.AttemptRecords) != 1 {
		t.Fatalf("failed HTTP lost its submission/account facts: attempted=%v account_present=%v count=%d records=%d", result.Attempted, result.AccountRef != "", result.Attempts, len(result.AttemptRecords))
	}
}

func TestAttemptAccountingPartialUsageSurvivesBrokenBody(t *testing.T) {
	v, a := refreshFixture(t)
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		resp := refreshSuccess(r)
		frames := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}],\"usage\":{\"prompt_tokens\":17}}\n\n"
		resp.Body = io.NopCloser(io.MultiReader(strings.NewReader(frames), iotest.ErrReader(io.ErrUnexpectedEOF)))
		return resp, nil
	})
	var delivered strings.Builder
	result, err := p.CompleteDialogue(t.Context(), attemptDialogue(), func(s string) error { delivered.WriteString(s); return nil })
	if err == nil || delivered.String() != "partial" {
		t.Fatal("fixture did not reproduce partial delivery followed by broken body")
	}
	if result.Complete || !result.Attempted || !result.Usage.Reported() || !result.UsageReported || result.Usage.Prompt == nil || *result.Usage.Prompt != 17 || result.Usage.Completion != nil || result.AccountRef != a.ID {
		t.Fatalf("broken body discarded observed usage/account facts: attempted=%v usage=%v account_present=%v", result.Attempted, result.Usage.Reported(), result.AccountRef != "")
	}
}

// Production 2026-11-24: four different AutoClaw accounts each answered HTTP
// 500 within ~2s. The old policy cooled every one; the next request had zero
// available accounts and returned no_available_account without any upstream
// call. A provider-wide 5xx must rotate without poisoning the pool.
func TestProviderWide500DoesNotPoisonEveryAccount(t *testing.T) {
	v, first := refreshFixture(t)
	second := accountvault.Account{ID: "autoclaw-fixture-2", Provider: "autoclaw", Enabled: true}
	profile, _ := json.Marshal(stored{AccessToken: "fixture-access-2", RefreshToken: "fixture-refresh-2"})
	if _, err := v.Put(second, profile, 0); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != chatPath {
			return refreshSuccess(r), nil
		}
		calls.Add(1)
		resp := refreshSuccess(r)
		resp.StatusCode = http.StatusInternalServerError
		resp.Body = io.NopCloser(strings.NewReader(`{"error":"upstream outage"}`))
		return resp, nil
	})
	if _, err := p.CompleteDialogue(t.Context(), attemptDialogue(), nil); err == nil {
		t.Fatal("provider-wide 500 was reported as success")
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want both accounts tried", calls.Load())
	}
	// Both accounts must remain usable for the next request.
	if !p.cooldown().Ready(first.ID) {
		t.Fatal("first account was cooled by a provider-wide 500")
	}
	if !p.cooldown().Ready(second.ID) {
		t.Fatal("second account was cooled by a provider-wide 500")
	}
	calls.Store(0)
	if _, err := p.CompleteDialogue(t.Context(), attemptDialogue(), nil); err == nil {
		t.Fatal("second request unexpectedly succeeded")
	}
	if calls.Load() == 0 {
		t.Fatal("second request short-circuited with no upstream call")
	}
}
