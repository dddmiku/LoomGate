package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/inference"
)

// An OpenAI-compatible client sends an open-ended set of fields. Anything this
// provider cannot carry must be ignored, never turned into a 400 — that is what
// made every real client fail while the model itself was reachable.
func TestProviderIgnoresUnknownFields(t *testing.T) {
	for _, extra := range []string{
		`"tools":[{"type":"function","function":{"name":"x","parameters":{"type":"object"}}}],"tool_choice":"auto"`,
		`"max_tokens":512,"temperature":0.7,"top_p":0.9,"frequency_penalty":0,"presence_penalty":0`,
		`"response_format":{"type":"json_object"},"seed":42,"user":"u"`,
		`"enable_thinking":true,"thinking":{"type":"enabled","budget_tokens":1024}`,
		`"chat_template_kwargs":{"enable_thinking":false},"repetition_penalty":1.05`,
		`"parallel_tool_calls":false,"stream_options":{"include_usage":true}`,
	} {
		p := &dialogueFixtureProvider{}
		h := NewHandler(Config{Pool: testPoolWith(), APIKey: "synthetic-key", Inference: map[string]inference.Provider{"doubao": p}})
		body := `{"model":"doubao:synthetic:model","messages":[{"role":"user","content":"synthetic"}],` + extra + `}`
		request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer synthetic-key")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("field set rejected: %s -> %d %s", extra, response.Code, response.Body.String())
		}
		if len(p.calls) != 1 || len(p.calls[0].Messages) != 1 || p.calls[0].Messages[0].Content != "synthetic" {
			t.Fatalf("unexpected dialogue for %s: %+v", extra, p.calls)
		}
	}
}

// Even a message with extra keys (name, tool_calls, refusal) must not fail: only
// role and content are read.
func TestProviderIgnoresExtraMessageFields(t *testing.T) {
	p := &dialogueFixtureProvider{}
	h := NewHandler(Config{Pool: testPoolWith(), APIKey: "synthetic-key", Inference: map[string]inference.Provider{"doubao": p}})
	body := `{"model":"doubao:synthetic:model","messages":[{"role":"user","content":"synthetic","name":"alice","tool_calls":[],"refusal":null}]}`
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer synthetic-key")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("extra message fields rejected: %d %s", response.Code, response.Body.String())
	}
	if len(p.calls) != 1 || p.calls[0].Messages[0].Content != "synthetic" {
		t.Fatalf("unexpected dialogue: %+v", p.calls)
	}
}
