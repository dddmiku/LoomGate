package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/keylimit"
	"workbuddy2api/internal/requestlog"
)

type fixtureProvider struct {
	calls    atomic.Int32
	complete bool
	err      error
	panic    bool
}

func (p *fixtureProvider) Models(context.Context) ([]inference.Model, error) {
	if p.panic {
		panic("PRIVATE_SYNTHETIC_PANIC")
	}
	return []inference.Model{{ID: "fixture:dynamic", Name: "Synthetic dynamic", ContextWindow: 4096}}, nil
}
func (p *fixtureProvider) Complete(ctx context.Context, model, text string) (inference.Result, error) {
	if p.panic {
		panic("PRIVATE_SYNTHETIC_PANIC")
	}
	p.calls.Add(1)
	return inference.Result{Text: "synthetic final reply", Attempted: true, Acknowledged: true, Complete: p.complete, AccountRef: "synthetic-ref", AttemptRecords: []inference.Attempt{{StartedAt: time.Now().Add(-time.Millisecond), FinishedAt: time.Now(), HTTPStatus: 200}}}, p.err
}
func providerFixture(t *testing.T, policy *keylimit.Policy) (*Handler, *fixtureProvider, string, string) {
	t.Helper()
	store, err := apikeys.Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	_, allowed, err := store.Create("fixture-allowed", "", []string{"fixture:dynamic"}, apikeys.Options{Limits: policy, LimitsSet: policy != nil})
	if err != nil {
		t.Fatal(err)
	}
	_, denied, err := store.Create("fixture-denied", "", []string{"fixture:other"})
	if err != nil {
		t.Fatal(err)
	}
	p := &fixtureProvider{complete: true}
	h := NewHandler(Config{Pool: testPoolWith(), APIKeys: store, Inference: map[string]inference.Provider{"fixture": p}})
	t.Cleanup(func() { h.cfg.KeyLimits.Close() })
	return h, p, allowed, denied
}
func providerRequest(h *Handler, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestProviderUsesExistingAuthModelPolicyAndAdmission(t *testing.T) {
	h, p, key, denied := providerFixture(t, &keylimit.Policy{RequestsPerMinute: 1, MaxConcurrent: 1})
	body := `{"model":"fixture:dynamic","messages":[{"role":"user","content":"synthetic"}]}`
	if w := providerRequest(h, "", body); w.Code != 401 {
		t.Fatal("missing authentication passed")
	}
	if w := providerRequest(h, denied, body); w.Code != 403 {
		t.Fatal("model allowlist bypassed")
	}
	if p.calls.Load() != 0 {
		t.Fatal("unauthorized provider call")
	}
	if w := providerRequest(h, key, body); w.Code != 200 || !strings.Contains(w.Body.String(), "synthetic final reply") || strings.Contains(w.Body.String(), `"usage"`) {
		t.Fatalf("completion/unknown usage contract failed: %d", w.Code)
	}
	if w := providerRequest(h, key, body); w.Code != 429 {
		t.Fatal("key rate policy bypassed")
	}
	if p.calls.Load() != 1 {
		t.Fatal("rate limited request invoked provider")
	}
}
func TestProviderIncompleteFailureAndUnsupportedInputDoNotSucceed(t *testing.T) {
	h, p, key, _ := providerFixture(t, nil)
	for _, body := range []string{`{"model":"fixture:dynamic","messages":[{"role":"system","content":"synthetic"}]}`, `{"model":"fixture:dynamic","messages":[{"role":"user","content":"synthetic"}],"temperature":1}`, `{"model":"fixture:dynamic","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://invalid.example/image"}}]}]}`} {
		if w := providerRequest(h, key, body); w.Code != 400 {
			t.Fatal("unsupported input was silently altered")
		}
	}
	if p.calls.Load() != 0 {
		t.Fatal("unsupported request sent upstream")
	}
	body := `{"model":"fixture:dynamic","messages":[{"role":"user","content":"synthetic"}],"stream":true}`
	p.complete = false
	w := providerRequest(h, key, body)
	if w.Code != 502 || strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal("incomplete upstream reported completion")
	}
	p.err = errors.New("PRIVATE_SYNTHETIC_UPSTREAM")
	w = providerRequest(h, key, body)
	if strings.Contains(w.Body.String(), "PRIVATE_SYNTHETIC") {
		t.Fatal("upstream exception leaked")
	}
	p.err = nil
	p.complete = true
	w = providerRequest(h, key, body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "[DONE]") || !strings.Contains(w.Body.String(), "synthetic final reply") {
		t.Fatal("valid streamed completion missing")
	}
}
func TestProviderDynamicModelsRespectCallingKey(t *testing.T) {
	h, _, allowed, denied := providerFixture(t, nil)
	for _, tc := range []struct {
		key     string
		visible bool
	}{{allowed, true}, {denied, false}} {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+tc.key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), "fixture:dynamic") != tc.visible {
			t.Fatal("dynamic provider catalog authorization failed")
		}
	}
}

func TestUnconfiguredDoubaoNeverFallsThroughToLegacy(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), APIKey: "synthetic-key"})
	w := providerRequest(h, "synthetic-key", `{"model":"doubao:synthetic:model","messages":[{"role":"user","content":"synthetic"}]}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "provider_unavailable") {
		t.Fatal("unconfigured provider fell through to legacy")
	}
}

func TestProviderPanicDoesNotEscapeOrDisableHealthyProvider(t *testing.T) {
	h, p, key, _ := providerFixture(t, nil)
	p.panic = true
	w := providerRequest(h, key, `{"model":"fixture:dynamic","messages":[{"role":"user","content":"synthetic"}]}`)
	if w.Code != 502 || strings.Contains(w.Body.String(), "PRIVATE_SYNTHETIC") {
		t.Fatal("provider panic was not isolated")
	}
	other := &fixtureProvider{complete: true}
	h.cfg.Inference["healthy"] = other
	models, issues := h.providerModels(context.Background())
	if len(models) != 1 || len(issues) != 1 {
		t.Fatal("failed provider hid healthy catalog")
	}
	p.panic = false
	w = providerRequest(h, key, `{"model":"fixture:dynamic","messages":[{"role":"user","content":"synthetic"}]}`)
	if w.Code != 200 {
		t.Fatal("provider recovery failed")
	}
}

func TestProviderAttemptAuditKeepsUsageUnknownAndExcludesContent(t *testing.T) {
	h, _, key, _ := providerFixture(t, nil)
	store, err := requestlog.Open(filepath.Join(t.TempDir(), "requests.jsonl"), requestlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h.cfg.Requests = store
	w := providerRequest(h, key, `{"model":"fixture:dynamic","messages":[{"role":"user","content":"synthetic private prompt"}]}`)
	if w.Code != 200 {
		t.Fatal("request failed")
	}
	page, err := store.List(requestlog.Query{})
	if err != nil || page.Total != 1 {
		t.Fatal("provider request missing from history")
	}
	record, err := store.Get(page.Items[0].RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.UpstreamStarted || record.AttemptCount != 1 || len(record.Attempts) != 1 || record.Attempts[0].UsageState != requestlog.UsageMissing || record.Attempts[0].InputTokens != nil || record.Attempts[0].OutputTokens != nil {
		t.Fatal("provider usage or attempt attribution incorrect")
	}
}
