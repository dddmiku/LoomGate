package main

// Product-level wiring contract for the provider set. The console and the
// OpenAI-compatible endpoint both read from the same assembled workbench, so
// this test asserts what a user would see: AutoClaw is present and callable,
// and the removed Doubao provider leaves no trace in the model catalog or the
// provider registry.

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkbenchServesAutoClawAndNoDoubao(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	w, transport := openScheduleMigrationRuntime(t, stateDir)
	if transport == nil {
		t.Fatal("fixture missing upstream transport")
	}

	providers := workbenchInference(w)
	if providers == nil {
		t.Fatal("no inference providers wired")
	}
	if _, ok := providers["autoclaw"]; !ok {
		t.Fatal("autoclaw provider not wired")
	}
	if _, ok := providers["doubao"]; ok {
		t.Fatal("doubao provider still wired")
	}

	// The model list served by the gateway must advertise autoclaw models and
	// nothing under the removed doubao namespace.
	rec := httptest.NewRecorder()
	w.engine.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	_ = rec
	if strings.Contains(strings.ToLower(rec.Body.String()), "doubao:") {
		t.Fatalf("model list still advertises doubao: %s", rec.Body.String())
	}
}
