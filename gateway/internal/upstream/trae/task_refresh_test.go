package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestTaskRefreshExchangeContractAndExpiry(t *testing.T) {
	for _, mode := range []string{"seconds", "milliseconds", "missing", "expired", "malformed", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, _ := io.ReadAll(r.Body)
				r.Body.Close()
				var request exchangeRequest
				if json.Unmarshal(raw, &request) != nil || request.ClientID != TraeClientID || request.UserID != "synthetic-task-account" || request.RefreshToken != "synthetic-refresh" || r.URL.Path != exchangePath {
					t.Error("exchange contract changed")
				}
				if mode == "redirect" {
					http.Redirect(w, r, "/retry", 307)
					return
				}
				stamp := time.Now().Add(time.Hour).Unix()
				if mode == "milliseconds" {
					stamp *= 1000
				}
				value := any(strconv.FormatInt(stamp, 10))
				if mode == "expired" {
					value = "1"
				}
				if mode == "malformed" {
					value = "1e100"
				}
				result := map[string]any{"Token": "synthetic-new", "TokenExpireAt": value}
				if mode == "missing" {
					delete(result, "Token")
				}
				json.NewEncoder(w).Encode(map[string]any{"Result": result})
			}))
			defer s.Close()
			a := syntheticTaskAuth()
			a.RefreshToken = "synthetic-refresh"
			got, err := (&TaskClient{HTTP: s.Client(), Origin: s.URL}).Refresh(context.Background(), a)
			if calls.Load() != 1 {
				t.Fatal("exchange replayed")
			}
			if mode == "seconds" || mode == "milliseconds" {
				if err != nil || got.ExpiresAt <= time.Now().Unix() {
					t.Fatal("valid exchange rejected")
				}
			} else if err == nil {
				t.Fatal("bad exchange accepted")
			}
		})
	}
}
