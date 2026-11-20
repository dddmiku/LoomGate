package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestModelEntitlementRejectionKeepsItsMeaningAcrossProtocols(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			pool := testPoolWith(
				&auth.Auth{UID: "fixture-one", AccessToken: "fixture-private-upstream", ExpiresAt: 9999999999},
				&auth.Auth{UID: "fixture-two", AccessToken: "fixture-private-second", ExpiresAt: 9999999999},
			)
			calls := 0
			upstream := newFakeUpstream(t, func(string) (int, string, bool) {
				calls++
				return http.StatusBadRequest, `{"code":11158,"msg":"model fixture-model is not allowed for this free user","requestId":"fixture-request","detail":"fixture-private-upstream"}`, false
			})
			h := NewHandler(Config{Pool: pool, Upstream: upstream, MaxSoftRotations: 2, RotateOnClientError: true})
			path, body := "/v1/chat/completions", `{"model":"cn:fixture-model","messages":[{"role":"user","content":"fixture"}]}`
			switch protocol {
			case "responses":
				path, body = "/v1/responses", `{"model":"cn:fixture-model","input":"fixture","stream":false}`
			case "messages":
				path, body = "/v1/messages", `{"model":"cn:fixture-model","messages":[{"role":"user","content":"fixture"}],"max_tokens":4096}`
			case "gemini":
				path, body = "/v1beta/models/cn:fixture-model:generateContent", `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
			if w.Code != http.StatusForbidden || calls != 1 {
				t.Fatalf("entitlement rejection changed status or retried: status=%d calls=%d", w.Code, calls)
			}
			if protocol != "gemini" && !strings.Contains(w.Body.String(), "upstream_model_access_denied") {
				t.Fatal("protocol dropped the public entitlement error code")
			}
			if !strings.Contains(w.Body.String(), "11158") || !strings.Contains(w.Body.String(), "fixture-request") || strings.Contains(w.Body.String(), "fixture-private-upstream") {
				t.Fatal("entitlement diagnosis lost its code/request or exposed a credential")
			}
			for _, id := range []string{"fixture-one", "fixture-two"} {
				status, _ := pool.Status(id)
				if status.Cooling || status.ErrTotal != 0 || status.InFlight != 0 {
					t.Fatal(fmt.Sprintf("model entitlement rejection penalized a healthy account %s", id))
				}
			}
		})
	}
}
