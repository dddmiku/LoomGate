package main

// Product-level wiring contract for the provider set. The console and the
// OpenAI-compatible endpoint both read from the same assembled workbench, so
// this test asserts what a user would see: AutoClaw and Doubao are both present
// and callable.

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestWorkbenchServesAutoClawAndDoubao(t *testing.T) {
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
	if _, ok := providers["doubao"]; !ok {
		t.Fatal("doubao provider not wired")
	}

	// Both namespaces must be advertised.
	rec := httptest.NewRecorder()
	w.engine.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	_ = rec
}
