package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/autoclaw"
	"workbuddy2api/internal/requestlog"
	"workbuddy2api/internal/usage"
)

// The actual provider, encrypted account store and gateway handler run together;
// every HTTP request stops at this injected transport before reaching a network.
func TestAutoClawStreamContractReachesPublicResponseAndAccounting(t *testing.T) {
	cases := []struct {
		name, frames, errorCode, finishReason string
		prompt, completion                    int64
		missingOutput, tools                  bool
	}{
		{"plain-eof", "data: {\"choices\":[{\"delta\":{\"content\":\"fixture partial answer\",\"reasoning_content\":\"fixture private reasoning\"}}],\"usage\":{\"prompt_tokens\":11,\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\n", "autoclaw_stream_incomplete", "", 11, 0, true, false},
		{"malformed-after-output", "data: {\"choices\":[{\"delta\":{\"content\":\"fixture partial answer\"}}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: {bad fixture\n\ndata: [DONE]\n\n", "autoclaw_stream_invalid", "", 7, 2, false, false},
		{"final-no-newline", `data: {"choices":[{"delta":{"content":"fixture final answer","reasoning_content":"fixture private reasoning"},"finish_reason":"length"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`, "", "length", 7, 2, false, false},
		{"oversized-frame", "data: {\"choices\":[{\"delta\":{\"content\":\"fixture partial answer\"}}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: " + strings.Repeat(" ", (1<<20)+1), "autoclaw_stream_limit", "", 7, 2, false, false},
		{"filtered-empty", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n", "", "content_filter", 7, 0, false, false},
		{"length-tool-turn", "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"fixture-call\",\"type\":\"function\",\"function\":{\"name\":\"fixture_tool\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", "", "length", 7, 2, false, true},
	}
	for _, tc := range cases {
		for _, streaming := range []bool{false, true} {
			name := tc.name + "/buffered"
			if streaming {
				name = tc.name + "/streaming"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				vault, err := accountvault.Open(filepath.Join(root, "vault"))
				if err != nil {
					t.Fatal(err)
				}
				defer vault.Close()
				profile, _ := json.Marshal(map[string]any{"accessToken": "synthetic-fixture-access"})
				if _, err = vault.Put(accountvault.Account{ID: "autoclaw-13900002222", Provider: "autoclaw", Enabled: true}, profile, 0); err != nil {
					t.Fatal(err)
				}
				keys, err := apikeys.Open(filepath.Join(root, "keys.json"), "")
				if err != nil {
					t.Fatal(err)
				}
				model := autoclaw.Models[0].ID
				_, key, err := keys.Create("fixture-client", "", []string{model})
				if err != nil {
					t.Fatal(err)
				}
				ledger, err := usage.Open(filepath.Join(root, "usage.json"), time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				defer ledger.Close()
				historyPath := filepath.Join(root, "requests.jsonl")
				history, err := requestlog.Open(historyPath, requestlog.Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer history.Close()
				var calls atomic.Int32
				provider := &autoclaw.Provider{Vault: vault, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					if r.Method != http.MethodPost || r.URL.Host != "autoglm-acceleration-api.zhipuai.cn" || r.URL.Path != "/autoclaw-proxy/proxy/autoclaw/chat/completions" {
						t.Error("unexpected HTTP at synthetic transport boundary")
						return nil, errors.New("fixture_unexpected_request")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.frames)), Request: r}, nil
				})}}
				handler := NewHandler(Config{Pool: testPoolWith(), APIKeys: keys, Usage: ledger, Requests: history, Inference: map[string]inference.Provider{"autoclaw": provider}})
				defer handler.cfg.KeyLimits.Close()
				body := map[string]any{"model": model, "stream": streaming, "messages": []any{map[string]any{"role": "user", "content": "fixture private prompt"}}}
				if tc.tools {
					body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "fixture_tool", "parameters": map[string]any{"type": "object"}}}}
				}
				raw, _ := json.Marshal(body)
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw)))
				request.Header.Set("Authorization", "Bearer "+key)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if calls.Load() != 1 {
					t.Fatalf("generation call count=%d, want one", calls.Load())
				}
				if tc.errorCode != "" {
					if !strings.Contains(response.Body.String(), tc.errorCode) {
						t.Errorf("public response lost the stable error code %s", tc.errorCode)
					}
					if strings.Contains(response.Body.String(), "[DONE]") || strings.Contains(response.Body.String(), `"finish_reason":"stop"`) {
						t.Error("failed stream advertised successful completion")
					}
					if !streaming && response.Code != 502 {
						t.Errorf("buffered failure status=%d", response.Code)
					}
				} else {
					if response.Code != 200 || !strings.Contains(response.Body.String(), `"finish_reason":"`+tc.finishReason+`"`) {
						t.Error("upstream terminal reason was replaced or response failed")
					}
					if streaming && !strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
						t.Error("terminal-only stream did not use the SSE content type")
					}
					if streaming && strings.Count(response.Body.String(), "[DONE]") != 1 {
						t.Error("completed stream lacks exactly one terminal marker")
					}
				}
				totals := ledger.Snapshot().Totals
				wantFailed := int64(0)
				if tc.errorCode != "" {
					wantFailed = 1
				}
				if totals.Requests != 1 || totals.PromptTokens != tc.prompt || totals.CompletionTokens != tc.completion || totals.FailedRequests != wantFailed {
					t.Errorf("provider accounting facts diverged: requests=%d prompt=%d completion=%d failed=%d", totals.Requests, totals.PromptTokens, totals.CompletionTokens, totals.FailedRequests)
				}
				page, err := history.List(requestlog.Query{})
				if err != nil || page.Total != 1 {
					t.Fatal("request history missing")
				}
				record, err := history.Get(page.Items[0].RequestID)
				if err != nil || len(record.Attempts) != 1 {
					t.Fatal("per-attempt history missing")
				}
				attempt := record.Attempts[0]
				if attempt.InputTokens == nil || *attempt.InputTokens != tc.prompt || tc.missingOutput && attempt.OutputTokens != nil || !tc.missingOutput && (attempt.OutputTokens == nil || *attempt.OutputTokens != tc.completion) {
					t.Error("history invented or lost token observations")
				}
				if tc.errorCode != "" && (attempt.Status == requestlog.StatusSuccess || record.ErrorCode != tc.errorCode || attempt.ErrorCode != tc.errorCode) {
					t.Error("failed stream history lost error attribution")
				}
				if err = history.Close(); err != nil {
					t.Fatal(err)
				}
				stored, err := os.ReadFile(historyPath)
				if err != nil {
					t.Fatal(err)
				}
				for _, private := range []string{"synthetic-fixture-access", "fixture private prompt", "fixture private reasoning", "fixture partial answer", "autoclaw-13900002222"} {
					if strings.Contains(string(stored), private) {
						t.Error("private content or source account identity entered request history")
					}
				}
			})
		}
	}
}
