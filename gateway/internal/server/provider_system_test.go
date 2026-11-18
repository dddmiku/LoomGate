package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/inference"
)

// TestProviderSystemInstructionsAreCarriedAsText proves a system prompt no longer
// kills the request. The official body has no separate system role, so the text
// is prepended to the first user turn: the model still receives it, and a client
// that always sends one keeps working.
func TestProviderSystemInstructionsAreCarriedAsText(t *testing.T) {
	p := &dialogueFixtureProvider{}
	h := NewHandler(Config{Pool: testPoolWith(), APIKey: "synthetic-key", Inference: map[string]inference.Provider{"doubao": p}})
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"doubao:synthetic:model","messages":[{"role":"system","content":"SYNTHETIC-RULE"},{"role":"user","content":"synthetic question"}]}`))
	request.Header.Set("Authorization", "Bearer synthetic-key")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("system prompt rejected: %d %s", response.Code, response.Body.String())
	}
	if len(p.calls) != 1 || len(p.calls[0].Messages) != 1 {
		t.Fatalf("unexpected dialogue: %+v", p.calls)
	}
	got := p.calls[0].Messages[0]
	if got.Role != "user" || !strings.Contains(got.Content, "SYNTHETIC-RULE") || !strings.Contains(got.Content, "synthetic question") {
		t.Fatalf("system text lost: %+v", got)
	}
}

// TestProviderAcceptsToolsWithoutForwardingThem proves a tools block degrades to
// plain text instead of a 400, and never reaches the prompt.
func TestProviderAcceptsToolsWithoutForwardingThem(t *testing.T) {
	p := &dialogueFixtureProvider{}
	h := NewHandler(Config{Pool: testPoolWith(), APIKey: "synthetic-key", Inference: map[string]inference.Provider{"doubao": p}})
	body := `{"model":"doubao:synthetic:model","messages":[{"role":"user","content":"synthetic"}],"tools":[{"type":"function","function":{"name":"SECRET-TOOL","parameters":{"type":"object"}}}],"tool_choice":"auto"}`
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer synthetic-key")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("tools blocked the request: %d %s", response.Code, response.Body.String())
	}
	if len(p.calls) != 1 {
		t.Fatalf("unexpected calls: %d", len(p.calls))
	}
	for _, message := range p.calls[0].Messages {
		if strings.Contains(message.Content, "SECRET-TOOL") {
			t.Fatal("tool definition leaked into the prompt")
		}
	}
}
