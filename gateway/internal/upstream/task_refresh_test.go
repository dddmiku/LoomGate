package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/auth"
)

func TestTaskRefreshOfficialHTTPStrictCancelableAndSingleAttempt(t *testing.T) {
	for _, mode := range []string{"valid", "missing_code", "missing_expiry", "oversize_expiry", "realm_change", "session_dead", "redirect", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			entered := make(chan struct{}, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				r.Body.Close()
				calls.Add(1)
				entered <- struct{}{}
				if r.Method != "POST" || r.URL.Path != "/v2/plugin/auth/token/refresh" || r.Header.Get("X-Refresh-Token") == "" {
					t.Error("official refresh contract missing")
				}
				if mode == "cancel" {
					<-r.Context().Done()
					return
				}
				if mode == "redirect" {
					http.Redirect(w, r, "/retry", 307)
					return
				}
				data := map[string]any{"accessToken": "synthetic-new", "refreshToken": "synthetic-next", "expiresIn": 3600}
				envelope := map[string]any{"code": 0, "data": data}
				switch mode {
				case "missing_code":
					delete(envelope, "code")
				case "missing_expiry":
					delete(data, "expiresIn")
				case "oversize_expiry":
					data["expiresIn"] = 9e18
				case "realm_change":
					data["domain"] = "www.workbuddy.ai"
				case "session_dead":
					envelope["code"] = 12153
				}
				json.NewEncoder(w).Encode(envelope)
			}))
			defer s.Close()
			a, _ := auth.Parse([]byte(`{"uid":"synthetic-refresh","realm":"cn","accessToken":"synthetic-old","refreshToken":"synthetic-refresh","domain":"www.codebuddy.cn"}`))
			c := &Client{HTTP: s.Client(), ChatBaseCN: s.URL}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				go func() { <-entered; cancel() }()
			}
			update, err := c.TaskRefresh(ctx, a)
			if calls.Load() != 1 {
				t.Fatal("refresh not exactly one request")
			}
			if mode == "valid" {
				if err != nil || update.ExpiresAt <= time.Now().Unix() || a.AccessToken != "synthetic-old" {
					t.Fatal("valid response not isolated from credential write")
				}
			} else if err == nil {
				t.Fatal("invalid refresh accepted")
			}
			if mode == "session_dead" {
				var rejected *TaskRefreshRejected
				if !errors.As(err, &rejected) || rejected.Code != 12153 {
					t.Fatal("typed rejection missing")
				}
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancel lost")
			}
		})
	}
}
