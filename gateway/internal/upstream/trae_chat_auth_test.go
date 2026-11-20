package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream/trae"
)

type traeExchangeTransport func(*http.Request) (*http.Response, error)

func (f traeExchangeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTraeRevokedChatTokenRefreshesOnceWithoutChangingModelOrBody(t *testing.T) {
	for _, mode := range []string{"success", "unauthorized", "still-rejected", "refresh-failed", "ordinary-400", "concurrent"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("WB2API_TRAE_REMOTE", "0")
			var refreshCalls, chatCalls, oldCalls atomic.Int32
			var mu sync.Mutex
			models := []string{}
			firstOld := make(chan struct{})
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case trae.BatchCatalogPath:
					io.WriteString(w, `{"function_configs":[]}`)
				case trae.ChatPath:
					chatCalls.Add(1)
					var request map[string]any
					json.NewDecoder(r.Body).Decode(&request)
					name, _ := request["config_name"].(string)
					mu.Lock()
					models = append(models, name)
					mu.Unlock()
					if mode == "ordinary-400" {
						w.WriteHeader(400)
						io.WriteString(w, `{"code":3008,"message":"fixture invalid parameter"}`)
						return
					}
					if r.Header.Get("Authorization") == "Cloud-IDE-JWT fixture-old" || mode == "still-rejected" {
						count := oldCalls.Add(1)
						if mode == "concurrent" {
							if count == 2 {
								close(firstOld)
							}
							<-firstOld
						}
						status := http.StatusBadRequest
						if mode == "unauthorized" {
							status = http.StatusUnauthorized
						}
						w.WriteHeader(status)
						io.WriteString(w, `{"code":1001,"message":"not able to authenticate"}`)
						return
					}
					if r.Header.Get("Authorization") != "Cloud-IDE-JWT fixture-new" {
						t.Error("retry did not use the refreshed account snapshot")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "event: output\ndata: {\"response\":\"fixture answer\"}\n\nevent: done\ndata: {}\n\n")
				default:
					w.WriteHeader(404)
				}
			}))
			defer origin.Close()
			oldGateway := trae.GatewayBaseVar
			trae.GatewayBaseVar = origin.URL
			trae.InvalidateCatalog("")
			defer func() { trae.GatewayBaseVar = oldGateway; trae.InvalidateCatalog("") }()
			oldHTTP := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: traeExchangeTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.trae.cn" || !strings.HasSuffix(r.URL.Path, "/ExchangeToken") {
					return nil, errors.New("unexpected fixture exchange destination")
				}
				refreshCalls.Add(1)
				if mode == "refresh-failed" {
					return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"private":"fixture must not escape"}`)), Request: r}, nil
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"Result":{"Token":"fixture-new","RefreshToken":"fixture-next","TokenExpireAt":"4102444800"}}`)), Request: r}, nil
			})}
			defer func() { http.DefaultClient = oldHTTP }()
			account, err := auth.Parse([]byte(`{"uid":"fixture-trae-chat","realm":"trae","accessToken":"fixture-old","refreshToken":"fixture-refresh","expiresAt":4102444800}`))
			if err != nil {
				t.Fatal(err)
			}
			account.FilePath = filepath.Join(t.TempDir(), "fixture-auth.json")
			if err := account.SaveAtomic(); err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"model":"DeepSeek-V4-Flash","messages":[{"role":"user","content":"fixture immutable question"}],"max_tokens":4096}`)
			call := func() {
				rc, status, raw, err := (&Client{}).ChatStreamContext(t.Context(), account, body, "", ChatMeta{})
				if err != nil {
					t.Error("fixture request failed")
					return
				}
				if mode == "ordinary-400" {
					if status != 400 || !strings.Contains(string(raw), "3008") {
						t.Error("ordinary parameter rejection changed")
					}
					return
				}
				if mode == "still-rejected" || mode == "refresh-failed" {
					if !IsTraeAuthenticationRequired(status, raw) || rc != nil {
						t.Error("authentication failure was still presented as request params")
					}
					return
				}
				if status != 200 || rc == nil {
					t.Error("same-model retry did not succeed")
					return
				}
				defer rc.Close()
				answer, err := io.ReadAll(rc)
				if err != nil || !strings.Contains(string(answer), "fixture answer") || !strings.Contains(string(answer), "[DONE]") {
					t.Error("successful retry lost its response or terminal marker")
				}
			}
			if mode == "concurrent" {
				var wg sync.WaitGroup
				wg.Add(2)
				for range 2 {
					go func() { defer wg.Done(); call() }()
				}
				wg.Wait()
			} else {
				call()
			}
			wantRefresh, wantChat := int32(1), int32(2)
			if mode == "ordinary-400" {
				wantRefresh, wantChat = 0, 1
			}
			if mode == "refresh-failed" {
				wantChat = 1
			}
			if mode == "concurrent" {
				wantChat = 4
			}
			if refreshCalls.Load() != wantRefresh || chatCalls.Load() != wantChat {
				t.Fatalf("bounded refresh/retry count changed: refresh=%d chat=%d", refreshCalls.Load(), chatCalls.Load())
			}
			for _, name := range models {
				if name != "DeepSeek-V4-Flash" {
					t.Fatalf("requested model silently changed: %s", name)
				}
			}
			if mode == "success" || mode == "unauthorized" || mode == "concurrent" {
				if account.Snapshot().AccessToken != "fixture-new" {
					t.Error("original pool account was not updated")
				}
			}
		})
	}
}

func TestTraeChatRevocationRequiresExactHTTPBusinessCode(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"code":1001}`, true}, {400, `{"code":3008,"message":"code1001"}`, false},
		{200, `{"code":1001}`, false}, {401, `{"code":1001}`, true}, {400, `{"code":"1001"}`, false},
		{401, `{"code":"1001"}`, false}, {401, `{"code":3008}`, false}, {403, `{"code":1001}`, false},
	} {
		if trae.ChatAuthRejected(tc.status, []byte(tc.body)) != tc.want {
			t.Error("auth retry predicate accepted a different upstream condition")
		}
	}
}

func TestTraeRevokedTokenRefreshHonorsCancellation(t *testing.T) {
	oldHTTP := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: traeExchangeTransport(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
	defer func() { http.DefaultClient = oldHTTP }()
	account, _ := auth.Parse([]byte(`{"uid":"fixture","realm":"trae","accessToken":"fixture-old","refreshToken":"fixture-refresh"}`))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, _, err := trae.RefreshContext(ctx, account)
	if err == nil || ctx.Err() == nil {
		t.Error("refresh outlived caller cancellation")
	}
}
