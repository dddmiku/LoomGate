package autoclaw

// The AutoClaw provider must be a plain model call: one OpenAI-compatible POST
// with the account's access token, no conversation object and no per-turn
// identifier. These tests lock that contract without touching the network.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providerretry"
)

// TestBuildBodyHasNoConversationFields is the core promise of this provider:
// unlike the Doubao client protocol there is nothing conversation-shaped in the
// payload. If a field like conversation_id ever appears here, the provider has
// drifted back into client emulation.
func TestBuildBodyHasNoConversationFields(t *testing.T) {
	body, err := buildBody("autoclaw:glm-5.3-flash", []inference.Message{
		{Role: "user", Content: "hello"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if json.Unmarshal(body, &decoded) != nil {
		t.Fatal("invalid body")
	}
	for _, forbidden := range []string{"client_meta", "option", "conversation_id", "local_conversation_id", "need_create_conversation", "local_message_id"} {
		if _, ok := decoded[forbidden]; ok {
			t.Fatalf("payload carries client-protocol field %q: %s", forbidden, body)
		}
	}
	if decoded["store"] != false {
		t.Fatal("store must be false: the upstream call is stateless")
	}
	if decoded["model"] != "zai_glm-5.3-flash" {
		t.Fatalf("model not normalised: %v", decoded["model"])
	}
}

// TestModelAliasesResolve pins the public names users type to the four upstream
// ids, including the documented fallback for an unknown name.
func TestModelAliasesResolve(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"autoclaw:glm-5.3-flash", "zai_glm-5.3-flash"},
		{"autoclaw:glm-5.3", "zai_glm-5.3-flash"},
		{"autoclaw:auto", "zai_auto"},
		{"autoclaw:glm-5.3-coding", "zaicoding_glm-5.3"},
		{"autoclaw:zai_glm-5.3-flashx-legal", "zai_glm-5.3-flashx-legal"},
		{"autoclaw:unknown-model", "zai_glm-5.3-flash"},
		{"autoclaw:zai_auto", "zai_auto"},
	} {
		if got := within(tc.in); got != tc.want {
			t.Errorf("within(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

// TestSSEStreamEmitsDeltasImmediately proves the streaming path forwards text
// as it arrives rather than buffering the whole answer.
func TestSSEStreamEmitsDeltasImmediately(t *testing.T) {
	frames := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"178"}}]}`,
		`data: {"choices":[{"delta":{"content":"5"}}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	var got []string
	text, events, _, err := streamSSE(strings.NewReader(frames), func(s string) error {
		got = append(got, s)
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if text != "1785" || len(got) != 2 || events != 2 {
		t.Fatalf("text=%q events=%d deltas=%v", text, events, got)
	}
}

// TestNonStreamAggregatesSSE covers the buffered mode used by Responses.
func TestNonStreamAggregatesSSE(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"答"}}]}`,
		`data: {"choices":[{"delta":{"content":"案"}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		`data: [DONE]`,
	}, "\n")
	text, usage, err := aggregate([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if text != "答案" || !usage {
		t.Fatalf("text=%q usage=%v", text, usage)
	}
}

// TestEmptyAnswerIsAnError keeps "HTTP 200 with no text" from being reported as
// a successful completion.
func TestEmptyAnswerIsAnError(t *testing.T) {
	if _, _, err := aggregate([]byte("data: [DONE]\n")); err == nil {
		t.Fatal("empty answer accepted")
	}
}

// TestUpstreamErrorClassifiesForRotation checks the rotation policy: auth,
// quota and server errors implicate the account; a client error does not.
func TestUpstreamErrorClassifiesForRotation(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   providerretry.Class
	}{
		{401, providerretry.RetryAccount}, {403, providerretry.RetryAccount},
		{429, providerretry.RetryAccount}, {402, providerretry.RetryAccount},
		{500, providerretry.RetryAccount}, {503, providerretry.RetryAccount},
		{400, providerretry.Fatal}, {404, providerretry.Fatal}, {422, providerretry.Fatal},
	} {
		got := (&accountError{status: tc.status}).RetryClass()
		if got != tc.want {
			t.Errorf("status %d class=%v want %v", tc.status, got, tc.want)
		}
	}
}

// TestHeadersCarrySignedAppIDAndBearer keeps the auth envelope intact.
func TestHeadersCarrySignedAppIDAndBearer(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	setHeaders(req, "synthetic-token", "zai_auto")
	if req.Header.Get("X-Authorization") != "Bearer synthetic-token" {
		t.Fatal("bearer token missing")
	}
	if req.Header.Get("x-auth-appid") != appID {
		t.Fatal("app id missing")
	}
	if len(req.Header.Get("x-auth-sign")) != 32 {
		t.Fatal("signature is not an md5 hex digest")
	}
	if req.Header.Get("x-request-model") != "zai_auto" {
		t.Fatal("request model header missing")
	}
}

// TestProviderReportsFixedModelsWithoutAnAccount proves the console can list
// models before any account exists.
func TestProviderReportsFixedModelsWithoutAnAccount(t *testing.T) {
	p := &Provider{}
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != len(Models) {
		t.Fatalf("got %d models want %d", len(models), len(Models))
	}
	for _, m := range models {
		if !strings.HasPrefix(m.ID, "autoclaw:") {
			t.Fatalf("model id missing namespace: %q", m.ID)
		}
	}
}

// TestCompleteRequiresAnAccount ensures a missing vault/account is an explicit
// error instead of a nil-pointer panic or an empty success.
func TestCompleteRequiresAnAccount(t *testing.T) {
	p := &Provider{}
	if _, err := p.Complete(context.Background(), "autoclaw:glm-5.3-flash", "hi"); err == nil {
		t.Fatal("missing vault was not reported")
	}
}

// The upstream streams reasoning before content — the desktop client renders
// that as "思考 Ns". Dropping those frames silently lost the whole thinking
// phase; the answer itself must still be the content deltas only.
func TestStreamSeparatesReasoningFromAnswer(t *testing.T) {
	frames := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"先想"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"一想"}}]}`,
		`data: {"choices":[{"delta":{"content":"答案"}}]}`,
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		`data: [DONE]`,
		"",
	}, "\n")
	var answer, thinking []string
	text, events, usage, err := streamSSE(strings.NewReader(frames),
		func(s string) error { answer = append(answer, s); return nil },
		func(s string) error { thinking = append(thinking, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(thinking, "") != "先想一想" {
		t.Fatalf("reasoning not surfaced: %v", thinking)
	}
	if text != "答案" || strings.Join(answer, "") != "答案" {
		t.Fatalf("reasoning leaked into the answer: text=%q answer=%v", text, answer)
	}
	if events != 3 {
		t.Fatalf("events=%d want 3 (2 reasoning + 1 content)", events)
	}
	if !usage.PromptSeen || usage.Prompt != 10 || !usage.CompletionSeen || usage.Completion != 2 {
		t.Fatalf("usage not captured: %+v", usage)
	}
}

// A reasoning-only stream is not an answer: it must report the empty-answer
// error rather than pretending to have succeeded.
func TestReasoningOnlyStreamIsAnEmptyAnswer(t *testing.T) {
	frames := `data: {"choices":[{"delta":{"reasoning_content":"想了很久"}}]}` + "\n\n"
	text, _, _, err := streamSSE(strings.NewReader(frames), func(string) error { return nil }, nil)
	if text != "" {
		t.Fatalf("text=%q", text)
	}
	var empty *emptyAnswerError
	if !errors.As(err, &empty) {
		t.Fatalf("err=%v; a reasoning-only stream must be treated as an empty answer", err)
	}
}

// The upstream gate expects the same per-request identifiers the desktop
// client stamps; their absence put us on the upstream's slow path.
func TestHeadersCarryClientTraceIdentifiers(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/chat", nil)
	setHeaders(req, "token", "zai_glm-5.3-flash")
	for _, name := range []string{"x-query-id", "x-request-id", "x-session-id", "x-trace-id", "x_trace_id", "x-agent-id", "accept-language"} {
		if req.Header.Get(name) == "" {
			t.Errorf("missing header %q", name)
		}
	}
	if req.Header.Get("x_trace_id") != "autoclaw-desktop" {
		t.Errorf("x_trace_id=%q", req.Header.Get("x_trace_id"))
	}
	// request-id and trace-id share one value in the client.
	if req.Header.Get("x-request-id") != req.Header.Get("x-trace-id") {
		t.Error("x-request-id and x-trace-id must carry the same value, as the client sends them")
	}
	// Two requests must not reuse the same ids.
	other, _ := http.NewRequest(http.MethodPost, "https://example.invalid/chat", nil)
	setHeaders(other, "token", "zai_glm-5.3-flash")
	if other.Header.Get("x-request-id") == req.Header.Get("x-request-id") {
		t.Error("trace identifiers are not per-request")
	}
}
