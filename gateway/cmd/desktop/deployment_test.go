package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstalledLayoutKeepsExistingConfiguration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	layout := layoutFor(root, filepath.Join(filepath.Dir(root), "program"))
	if err := ensureLayout(layout); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(layout.Config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if json.Unmarshal(raw, &cfg) != nil {
		t.Fatal("invalid initial config")
	}
	if cfg["listen"] != "127.0.0.1:7863" {
		t.Fatal("fresh install must stay loopback")
	}
	cfg["listen"] = "127.0.0.1:17864"
	changed, _ := json.Marshal(cfg)
	os.WriteFile(layout.Config, changed, 0600)
	if err = ensureLayout(layout); err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(layout.Config)
	if string(actual) != string(changed) {
		t.Fatal("existing config overwritten")
	}
}

func TestDesktopReadyAcceptsEmptyAccountPanelButNotForeignService(t *testing.T) {
	for _, identity := range []string{"workbuddy2api", "other"} {
		t.Run(identity, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					w.Header().Set("X-Service", identity)
					w.WriteHeader(503)
					json.NewEncoder(w).Encode(map[string]string{"service": identity})
					return
				}
				w.WriteHeader(200)
			}))
			defer s.Close()
			if got := waitForHealth(strings.TrimPrefix(s.URL, "http://"), 350*time.Millisecond); got != (identity == "workbuddy2api") {
				t.Fatal("desktop readiness conflated empty account with stopped service")
			}
		})
	}
}
