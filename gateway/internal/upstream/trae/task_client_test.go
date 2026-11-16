package trae

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"workbuddy2api/internal/auth"
)

func syntheticTaskAuth() *auth.Auth {
	a, err := auth.Parse([]byte(`{"uid":"synthetic-task-account","realm":"trae","domain":"api.trae.cn","accessToken":"synthetic-only"}`))
	if err != nil {
		panic("synthetic_auth_invalid")
	}
	return a
}

func TestTaskClientStrictEnvelopesCannotConfirmOrZeroCredits(t *testing.T) {
	for _, body := range []string{`{}`, `{"code":null}`, `{"code":"0"}`, `{"code":0}`, `{"code":0,"usage_summary":{},"user_entitlement_pack_list":[]}`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		}))
		c := &TaskClient{HTTP: s.Client(), Origin: s.URL}
		if _, err := c.Credits(context.Background(), syntheticTaskAuth()); err == nil {
			t.Fatal("malformed balance became zero")
		}
		if _, err := c.Status(context.Background(), syntheticTaskAuth()); err == nil {
			t.Fatal("malformed status became eligibility")
		}
		if body == `{}` || body == `{"code":null}` || body == `{"code":"0"}` {
			if _, err := c.Claim(context.Background(), syntheticTaskAuth()); err == nil {
				t.Fatal("claim accepted without business code")
			}
		}
		s.Close()
	}
}
func TestTaskClientCancellationAndRedirectNeverReplayClaim(t *testing.T) {
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		requests++
		if r.URL.Path == "/redirected" {
			t.Error("claim redirect followed")
		}
		http.Redirect(w, r, "/redirected", 307)
	}))
	c := &TaskClient{HTTP: s.Client(), Origin: s.URL}
	if _, err := c.Claim(context.Background(), syntheticTaskAuth()); err == nil || requests != 1 {
		t.Fatal("redirect was followed or accepted")
	}
	s.Close()
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		<-r.Context().Done()
	}))
	defer s.Close()
	c = &TaskClient{HTTP: s.Client(), Origin: s.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := c.Credits(ctx, syntheticTaskAuth()); err == nil || time.Since(started) > time.Second {
		t.Fatal("cancel not propagated")
	}
}
