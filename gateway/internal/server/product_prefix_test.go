package server

// NarraFork sends the model prefixed with the product name, e.g.
// workbuddy:doubao:deepseek-v4-flash. That prefix is not a provider namespace:
// before this was handled the request fell through to the legacy pool under a
// name it could not serve, and the caller saw a 502 from upstream. These tests
// pin both halves: routing and key binding must accept the prefixed form.

import (
	"context"
	"testing"

	"workbuddy2api/internal/apikeys"
)

func TestProviderNamespaceSkipsProductPrefix(t *testing.T) {
	h, provider, key, _ := providerFixture(t, nil)
	body := `{"model":"workbuddy:fixture:dynamic","messages":[{"role":"user","content":"synthetic"}]}`
	w := providerRequest(h, key, body)
	if w.Code != 200 {
		t.Fatalf("prefixed provider model was not routed: status=%d body=%s", w.Code, w.Body.String())
	}
	if provider.calls.Load() == 0 {
		t.Fatal("provider never received the prefixed request")
	}
}

func TestKeyBindingAcceptsProductPrefixedModel(t *testing.T) {
	info := apikeys.Info{Models: []string{"doubao:deepseek-v4-flash"}}
	for _, model := range []string{
		"doubao:deepseek-v4-flash",
		"workbuddy:doubao:deepseek-v4-flash",
	} {
		if !modelAllowedByKey(info, model) {
			t.Errorf("%q rejected by a key that allows the bare id", model)
		}
	}
	if modelAllowedByKey(info, "workbuddy:cn:other-model") {
		t.Fatal("an unrelated model was accepted")
	}
}

func TestProductPrefixWithoutProviderFallsBackToLegacy(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), APIKey: "synthetic-key"})
	// No provider is registered under "fixture"; the name must not be rejected
	// as an unknown provider merely because it carries the product prefix.
	w := providerRequest(h, "synthetic-key", `{"model":"workbuddy:cn:deepseek-v4.1-flash","messages":[{"role":"user","content":"x"}]}`)
	if w.Code == 400 {
		t.Fatalf("product-prefixed legacy model was rejected: %s", w.Body.String())
	}
	_ = context.Background()
}
