package autoclaw

// End-to-end: a real HTTP request through Provider.turn against a local server
// that speaks the AutoClaw SSE dialect, with a real encrypted vault account.
// No external network is touched.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/accountvault"
)

// TestProviderServesStreamFromRealVaultAccount is the integration proof for the
// new provider: a vault-stored account, a real HTTP round trip, incrementally
// forwarded text, and no client-protocol fields on the wire.
func TestProviderServesStreamFromRealVaultAccount(t *testing.T) {
	var gotPath, gotAuth, gotModel string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("X-Authorization")
		gotModel = r.Header.Get("x-request-model")
		raw, _ := readAll(r)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	vault, err := accountvault.Open(filepath.Join(t.TempDir(), "vault"))
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	blob, _ := json.Marshal(stored{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Phone: "13800000000"})
	if _, err := vault.Put(accountvault.Account{ID: "acct-a", Provider: "autoclaw", Enabled: true}, blob, 0); err != nil {
		t.Fatal(err)
	}

	p := &Provider{Vault: vault, Client: &http.Client{Transport: rewriteHost{base: upstream.URL}}}
	var deltas []string
	result, err := p.Stream(context.Background(), "autoclaw:glm-5.3-flash", "hi", func(s string) error {
		deltas = append(deltas, s)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "你好" || len(deltas) != 2 {
		t.Fatalf("text=%q deltas=%v", result.Text, deltas)
	}
	if !result.Complete || !result.Acknowledged {
		t.Fatal("result not marked complete")
	}
	if gotAuth != "Bearer synthetic-access" {
		t.Fatalf("access token not used: %q", gotAuth)
	}
	if gotPath != chatPath {
		t.Fatalf("wrong upstream path: %q", gotPath)
	}
	if gotModel != "zai_glm-5.3-flash" {
		t.Fatalf("model alias not normalised on the wire: %q", gotModel)
	}
	for _, forbidden := range []string{"conversation_id", "local_conversation_id", "client_meta", "need_create_conversation"} {
		if _, ok := gotBody[forbidden]; ok {
			t.Fatalf("wire body leaked client-protocol field %q", forbidden)
		}
	}
	if _, ok := gotBody["store"]; ok {
		t.Fatal("store must be stripped; the upstream 406s on generic OpenAI knobs with an empty body")
	}
}

// TestProviderRotatesToSecondAccountOnAuthError proves the pool behaviour: a
// 401 on the first account retries the next one instead of failing the call.
func TestProviderRotatesToSecondAccountOnAuthError(t *testing.T) {
	seen := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("X-Authorization")
		seen = append(seen, auth)
		if strings.Contains(auth, "bad") {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"expired"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	vault, err := accountvault.Open(filepath.Join(t.TempDir(), "vault"))
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	for i, token := range []string{"bad-token", "good-token"} {
		blob, _ := json.Marshal(stored{AccessToken: token})
		id := fmt.Sprintf("acct-%d", i)
		if _, err := vault.Put(accountvault.Account{ID: id, Provider: "autoclaw", Enabled: true}, blob, 0); err != nil {
			t.Fatal(err)
		}
	}
	p := &Provider{Vault: vault, Client: &http.Client{Transport: rewriteHost{base: upstream.URL}}}
	result, err := p.Complete(context.Background(), "autoclaw:glm-5.3-flash", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "ok" || len(seen) != 2 {
		t.Fatalf("rotation failed: text=%q attempts=%v", result.Text, seen)
	}
}

// TestProviderSkipsDisabledAccounts keeps a paused account out of rotation.
func TestProviderSkipsDisabledAccounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("disabled account reached upstream")
	}))
	defer upstream.Close()
	vault, err := accountvault.Open(filepath.Join(t.TempDir(), "vault"))
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	blob, _ := json.Marshal(stored{AccessToken: "disabled-token"})
	if _, err := vault.Put(accountvault.Account{ID: "acct-off", Provider: "autoclaw", Enabled: false}, blob, 0); err != nil {
		t.Fatal(err)
	}
	p := &Provider{Vault: vault, Client: &http.Client{Transport: rewriteHost{base: upstream.URL}}}
	if _, err := p.Complete(context.Background(), "autoclaw:glm-5.3-flash", "hi"); err == nil {
		t.Fatal("disabled-only pool reported success")
	}
}

// rewriteHost sends the fixed https upstream URL to a local test server while
// leaving the path and headers untouched.
type rewriteHost struct{ base string }

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(r.base, "http://")
	return http.DefaultTransport.RoundTrip(clone)
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}
