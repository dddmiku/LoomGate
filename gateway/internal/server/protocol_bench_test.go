package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/inference"
)

// Measure gateway parsing/translation/writing separately from provider network
// latency. The provider completes immediately and retains no request content.
type benchmarkDialogueProvider struct{}

func (benchmarkDialogueProvider) Models(context.Context) ([]inference.Model, error) {
	return []inference.Model{{ID: "benchmark:model", MultiTurn: true, Incremental: true}}, nil
}
func (benchmarkDialogueProvider) Complete(context.Context, string, string) (inference.Result, error) {
	return inference.Result{Text: "fixture answer", Attempted: true, Acknowledged: true, Complete: true}, nil
}
func (p benchmarkDialogueProvider) CompleteDialogue(ctx context.Context, _ inference.Dialogue, emit func(string) error) (inference.Result, error) {
	result, _ := p.Complete(ctx, "", "")
	if emit != nil {
		if err := emit(result.Text); err != nil {
			return result, err
		}
	}
	return result, nil
}

func BenchmarkProtocolGateway(b *testing.B) {
	for _, size := range []int{64, 128 << 10} {
		for _, protocol := range []string{"chat", "responses", "messages", "gemini"} {
			for _, streaming := range []bool{false, true} {
				label := protocol + "/short"
				if size > 64 {
					label = protocol + "/128KiB"
				}
				if streaming {
					label += "/stream"
				}
				b.Run(label, func(b *testing.B) {
					text := strings.Repeat("x", size)
					messages := []any{map[string]any{"role": "user", "content": text}}
					body := map[string]any{"model": "benchmark:model", "messages": messages, "stream": streaming}
					path := "/v1/chat/completions"
					switch protocol {
					case "responses":
						path = "/v1/responses"
						body = map[string]any{"model": "benchmark:model", "input": messages, "stream": streaming}
					case "messages":
						path = "/v1/messages"
						body["max_tokens"] = 128
					case "gemini":
						path = "/v1beta/models/benchmark:model:generateContent"
						if streaming {
							path = "/v1beta/models/benchmark:model:streamGenerateContent"
						}
						body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}}}}
					}
					raw, _ := json.Marshal(body)
					h := NewHandler(Config{APIKey: "fixture-key", Inference: map[string]inference.Provider{"benchmark": benchmarkDialogueProvider{}}})
					defer h.cfg.KeyLimits.Close()
					b.ReportAllocs()
					b.SetBytes(int64(len(raw)))
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
						r.Header.Set("Authorization", "Bearer fixture-key")
						w := httptest.NewRecorder()
						h.ServeHTTP(w, r)
						if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fixture answer") {
							b.Fatalf("protocol fixture status=%d", w.Code)
						}
					}
				})
			}
		}
	}
}
