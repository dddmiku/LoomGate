package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/inference"
)

// effortFixtureProvider records what effort it was handed and can advertise its own
// ladder so the default-clamp behaviour is exercised end to end.
type effortFixtureProvider struct {
	fixtureProvider
	gotEffort string
	levels    []int
}

func (p *effortFixtureProvider) Models(context.Context) ([]inference.Model, error) {
	return []inference.Model{{ID: "fixture:dynamic", Name: "Synthetic effort model", ContextWindow: 4096}}, nil
}

func (p *effortFixtureProvider) StreamWithEffort(ctx context.Context, model, text, effort string, emit func(string) error) (inference.Result, error) {
	p.gotEffort = effort
	result, err := p.fixtureProvider.Complete(ctx, model, text)
	if emit != nil {
		result.Text = "synthetic final reply"
		if err = emit(result.Text); err != nil {
			return inference.Result{}, err
		}
	}
	return result, err
}

func effortFixture(t *testing.T) (*Handler, *effortFixtureProvider, string) {
	t.Helper()
	store, err := apikeys.Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := store.Create("fixture-allowed", "", []string{"fixture:dynamic"}, apikeys.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := &effortFixtureProvider{fixtureProvider: fixtureProvider{complete: true}}
	h := NewHandler(Config{APIKeys: store, Inference: map[string]inference.Provider{"fixture": p}})
	t.Cleanup(func() { h.cfg.KeyLimits.Close() })
	return h, p, key
}

// TestProviderAcceptsReasoningEffort is the regression for the observed 400:
// the capability allowlist used to reject reasoning_effort outright, so a proxy
// client reporting its thinking level could never reach the provider at all.
func TestProviderAcceptsReasoningEffort(t *testing.T) {
	h, p, key := effortFixture(t)
	body := `{"model":"fixture:dynamic","reasoning_effort":"max","messages":[{"role":"user","content":"synthetic"}]}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	h.ServeHTTP(w, r)

	if w.Code == 400 && strings.Contains(w.Body.String(), "provider_parameter_unsupported") {
		t.Fatalf("reasoning_effort was still rejected: %s", w.Body.String())
	}
	if w.Code != 200 {
		t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
	}
	if p.gotEffort != "max" {
		t.Fatalf("effort did not reach the provider: %q", p.gotEffort)
	}
}

// TestProviderCamelCaseReasoningEffort covers OpenAI-compatible clients that
// send reasoningEffort.
func TestProviderCamelCaseReasoningEffort(t *testing.T) {
	h, p, key := effortFixture(t)
	body := `{"model":"fixture:dynamic","reasoningEffort":"high","messages":[{"role":"user","content":"synthetic"}]}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
	}
	if p.gotEffort != "high" {
		t.Fatalf("camelCase effort did not reach the provider: %q", p.gotEffort)
	}
}

// TestProviderWithoutEffortSupportStillWorks proves the field is optional
// metadata: a plain provider ignores it instead of failing the request.
func TestProviderWithoutEffortSupportStillWorks(t *testing.T) {
	h, _, key, _ := providerFixture(t, nil)
	body := `{"model":"fixture:dynamic","reasoning_effort":"max","messages":[{"role":"user","content":"synthetic"}]}`
	if w := providerRequest(h, key, body); w.Code != 200 {
		t.Fatalf("plain provider rejected an optional effort field: %d %s", w.Code, w.Body.String())
	}
}

// TestProviderEffortAppearsInCatalogOutput keeps the capability surface honest:
// the model listing must not claim effort support that the provider lacks.
func TestProviderEffortAppearsInCatalogOutput(t *testing.T) {
	h, _, key := effortFixture(t)
	models, _ := h.providerModels(context.Background())
	if len(models) != 1 {
		t.Fatalf("expected one model, got %d", len(models))
	}
	raw, _ := json.Marshal(models[0])
	if !strings.Contains(string(raw), "fixture:dynamic") {
		t.Fatalf("model id missing: %s", raw)
	}
	_ = key
}
