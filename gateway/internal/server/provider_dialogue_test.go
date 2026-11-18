package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"workbuddy2api/internal/inference"
)

type dialogueFixtureProvider struct{ calls []inference.Dialogue }

func (p *dialogueFixtureProvider) Models(context.Context) ([]inference.Model, error) {
	return []inference.Model{{ID: "doubao:synthetic:model", MultiTurn: true, Incremental: true}}, nil
}
func (p *dialogueFixtureProvider) Complete(context.Context, string, string) (inference.Result, error) {
	panic("wrong_provider_entry")
}
func (p *dialogueFixtureProvider) CompleteDialogue(_ context.Context, q inference.Dialogue, emit func(string) error) (inference.Result, error) {
	p.calls = append(p.calls, q)
	result := inference.Result{Text: "synthetic answer", ConversationRef: "dialog-00000000000000000000000000000000", Attempted: true, Acknowledged: true, Complete: true}
	if emit != nil {
		if err := emit("synthetic "); err != nil {
			return result, err
		}
		if err := emit("answer"); err != nil {
			return result, err
		}
	}
	return result, nil
}
func TestProviderDialogueHTTPPreservesRolesAndCallerAcrossProtocols(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"chat", "/v1/chat/completions", `{"model":"doubao:synthetic:model","messages":[{"role":"user","content":"synthetic earlier"},{"role":"assistant","content":"synthetic answer"},{"role":"user","content":"synthetic next"}]}`, 200},
		{"responses", "/v1/responses", `{"model":"doubao:synthetic:model","input":[{"role":"user","content":"synthetic earlier"},{"role":"assistant","content":"synthetic answer"},{"role":"user","content":"synthetic next"}]}`, 200},
		{"messages-budget-ignored", "/v1/messages", `{"model":"doubao:synthetic:model","max_tokens":100,"messages":[{"role":"user","content":"synthetic"}]}`, 200},
		{"gemini", "/v1beta/models/doubao:synthetic:model:generateContent", `{"contents":[{"role":"user","parts":[{"text":"synthetic earlier"}]},{"role":"model","parts":[{"text":"synthetic answer"}]},{"role":"user","parts":[{"text":"synthetic next"}]}]}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &dialogueFixtureProvider{}
			h := NewHandler(Config{Pool: testPoolWith(), APIKey: "synthetic-key", Inference: map[string]inference.Provider{"doubao": p}})
			request := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer synthetic-key")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != tc.want {
				var payload map[string]any
				json.Unmarshal(response.Body.Bytes(), &payload)
				t.Fatalf("unexpected status %d error %v", response.Code, payload["error"])
			}
			if tc.want == 200 {
				if len(p.calls) != 1 || len(p.calls[0].Owner) != 64 {
					t.Fatal("caller scope lost")
				}
				if tc.name == "gemini" {
					if len(p.calls[0].Messages) != 3 || p.calls[0].Messages[1].Role != "assistant" || p.calls[0].Messages[2].Content != "synthetic next" {
						t.Fatal("role lost")
					}
				}
			}
		})
	}
}
func TestProviderDialogueStreamCarriesOpaqueRefAndRejectsCallerToolsBeforeCall(t *testing.T) {
	p := &dialogueFixtureProvider{}
	h := NewHandler(Config{Pool: testPoolWith(), APIKey: "synthetic-key", Inference: map[string]inference.Provider{"doubao": p}})
	w := providerRequest(h, "synthetic-key", `{"model":"doubao:synthetic:model","messages":[{"role":"user","content":"synthetic"}],"stream":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "[DONE]") || w.Result().Trailer.Get("X-LoomGate-Conversation") == "" {
		t.Fatal("stream cursor trailer contract lost")
	}
	// Caller-defined tools have no carrier in the official body. They are ignored
	// rather than rejected: /v1/models advertises supports_tool_call=false, so a
	// client that sends tools anyway gets a plain-text answer instead of a
	// permanent 400. The tools themselves are never forwarded upstream.
	for _, suffix := range []string{`,"tools":[{"type":"function","function":{"name":"synthetic","parameters":{"type":"object"}}}]`, `,"tool_choice":"auto"`, `,"functions":[{"name":"synthetic"}]`} {
		response := providerRequest(h, "synthetic-key", `{"model":"doubao:synthetic:model","messages":[{"role":"user","content":"synthetic"}]`+suffix+`}`)
		if response.Code != 200 {
			t.Fatalf("caller tools blocked the request: %d %s", response.Code, response.Body.String())
		}
	}
	if len(p.calls) != 4 {
		t.Fatalf("unexpected upstream calls: %d", len(p.calls))
	}
	for _, call := range p.calls {
		for _, message := range call.Messages {
			if strings.Contains(strings.ToLower(message.Content), "function") {
				t.Fatal("tool definition leaked into the prompt")
			}
		}
	}
}
