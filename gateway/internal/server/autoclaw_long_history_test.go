package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/autoclaw"
)

// Switching an existing Claude Code conversation to AutoClaw must not impose a
// local 101-message cap. Exercise both protocol conversion and the real provider
// serializer, stopping HTTP at an injected transport. Never replay private history.
func TestAutoClawAcceptsLongHistoryAcrossProtocols(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, tools := range []bool{false, true} {
			for _, streaming := range []bool{false, true} {
				name := path + "/text"
				if tools {
					name = path + "/tools"
				}
				if streaming {
					name += "/stream"
				}
				t.Run(name, func(t *testing.T) {
					vault, err := accountvault.Open(filepath.Join(t.TempDir(), "vault"))
					if err != nil {
						t.Fatal(err)
					}
					defer vault.Close()
					profile, _ := json.Marshal(map[string]any{"accessToken": "fixture-access"})
					if _, err := vault.Put(accountvault.Account{ID: "autoclaw-long-fixture", Provider: "autoclaw", Enabled: true}, profile, 0); err != nil {
						t.Fatal(err)
					}
					calls := 0
					provider := &autoclaw.Provider{Vault: vault, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						calls++
						var payload struct {
							Messages []inference.Message        `json:"messages"`
							Tools    []inference.ToolDefinition `json:"tools"`
						}
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Fatal(err)
						}
						// Tool-capable requests retain the system role; text-only
						// requests prepend its instructions to the first user turn.
						wantMessages, firstUser := 103, 1
						if tools {
							wantMessages, firstUser = 104, 2
						}
						if len(payload.Messages) != wantMessages {
							t.Errorf("forwarded messages=%d, want %d (no history truncation)", len(payload.Messages), wantMessages)
						}
						if len(payload.Messages) > firstUser && !strings.Contains(payload.Messages[firstUser].Content, "fixture beginning") {
							t.Error("earliest conversation turn lost")
						}
						if len(payload.Messages) > 1 && !strings.Contains(payload.Messages[1].Content, "fixture system instruction") {
							t.Error("system instructions lost during history forwarding")
						}
						if len(payload.Messages) > 0 && payload.Messages[len(payload.Messages)-1].Content != "fixture latest" {
							t.Error("latest conversation turn lost")
						}
						if tools && len(payload.Tools) != 1 {
							t.Error("tool catalog lost during history forwarding")
						}
						if tools && len(payload.Messages) == wantMessages {
							for i := 0; i < 50; i++ {
								call, result := payload.Messages[firstUser+1+i*2], payload.Messages[firstUser+2+i*2]
								id := fmt.Sprintf("fixture-call-%d", i)
								if call.Role != "assistant" || len(call.ToolCalls) != 1 || call.ToolCalls[0].ID != id || call.ToolCalls[0].Function.Name != "fixture_tool" || call.ToolCalls[0].Function.Arguments != fmt.Sprintf(`{"index":%d}`, i) || result.Role != "tool" || result.ToolCallID != id || result.Content != "fixture tool result" {
									t.Errorf("tool call/result pair %d lost structure", i)
								}
							}
						}
						frames := "data: {\"choices\":[{\"delta\":{\"content\":\"fixture answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames)), Request: r}, nil
					})}}
					h := NewHandler(Config{Pool: testPoolWith(), APIKey: "fixture-key", Inference: map[string]inference.Provider{"autoclaw": provider}})
					defer h.cfg.KeyLimits.Close()
					messages := []any{map[string]any{"role": "user", "content": "fixture beginning"}}
					for i := 0; i < 50; i++ {
						if !tools {
							messages = append(messages, map[string]any{"role": "assistant", "content": "fixture answer"}, map[string]any{"role": "user", "content": "fixture question"})
							continue
						}
						id := fmt.Sprintf("fixture-call-%d", i)
						if path == "/v1/messages" {
							messages = append(messages, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "fixture_tool", "input": map[string]any{"index": i}}}}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "fixture tool result"}}})
						} else {
							messages = append(messages, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "fixture_tool", "arguments": fmt.Sprintf(`{"index":%d}`, i)}}}}, map[string]any{"role": "tool", "tool_call_id": id, "content": "fixture tool result"})
						}
					}
					messages = append(messages, map[string]any{"role": "user", "content": "fixture latest"})
					body := map[string]any{"model": autoclaw.Models[0].ID, "messages": messages, "max_tokens": 64, "stream": streaming}
					if path == "/v1/messages" {
						body["system"] = "fixture system instruction"
					} else {
						body["messages"] = append([]any{map[string]any{"role": "system", "content": "fixture system instruction"}}, messages...)
					}
					if tools {
						definition := map[string]any{"name": "fixture_tool", "parameters": map[string]any{"type": "object"}}
						if path == "/v1/messages" {
							definition = map[string]any{"name": "fixture_tool", "input_schema": map[string]any{"type": "object"}}
							body["tools"] = []any{definition}
						} else {
							body["tools"] = []any{map[string]any{"type": "function", "function": definition}}
						}
					}
					raw, _ := json.Marshal(body)
					req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
					req.Header.Set("Authorization", "Bearer fixture-key")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, req)
					if w.Code != 200 || calls != 1 || !strings.Contains(w.Body.String(), "fixture answer") {
						t.Fatalf("long history status=%d upstream_calls=%d response=%s", w.Code, calls, w.Body.String())
					}
				})
			}
		}
	}
}

func TestAutoClawOversizedHistoryRejectedBeforeUpstream(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			calls := 0
			p := &autoclaw.Provider{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, fmt.Errorf("unexpected upstream request")
			})}}
			h := NewHandler(Config{Pool: testPoolWith(), APIKey: "fixture-key", MaxBodyBytes: 16 << 20, Inference: map[string]inference.Provider{"autoclaw": p}})
			defer h.cfg.KeyLimits.Close()
			body := map[string]any{"model": autoclaw.Models[0].ID, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", (8<<20)+1)}}}
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer fixture-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusRequestEntityTooLarge || calls != 0 || !strings.Contains(w.Body.String(), "request_body_too_large") {
				t.Fatalf("oversized history status=%d upstream_calls=%d", w.Code, calls)
			}
		})
	}
}
