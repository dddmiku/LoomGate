package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func preflightServer(t *testing.T) (*Server, string) {
	t.Helper()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "tools", "migration"), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(base, "tools", "migration", "migrate.py")
	if err := os.WriteFile(script, []byte("import json,sys\nprint(json.dumps({'format':2,'mode':'dry-run','file_count':1}))\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{opts: Options{BaseDir: base}}
	return s, base
}

func TestMigrationPreflightRejectsUnsafePathsBeforeRunning(t *testing.T) {
	s, base := preflightServer(t)
	source := filepath.Join(t.TempDir(), "old")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(base, "copy")
	cases := map[string][2]string{
		"relative":        {"old", filepath.Join(t.TempDir(), "new")},
		"inside-runtime":  {source, inside},
		"existing-target": {source, source},
		"overlap":         {source, filepath.Join(source, "nested")},
		"missing-source":  {filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "new")},
	}
	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			body := map[string]any{"source": pair[0], "destination": pair[1]}
			s.migrationPreflight(recorder, httptest.NewRequest(http.MethodPost, "/api/migration/preflight", nil), body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), pair[0]) && name != "existing-target" {
				t.Fatal("error response should not echo full private paths")
			}
		})
	}
}

func TestMigrationPreflightRunsDryRunAndNeverWritesDestination(t *testing.T) {
	if migrationPython() == "" {
		t.Skip("python3 unavailable")
	}
	s, base := preflightServer(t)
	root := t.TempDir()
	source := filepath.Join(root, "old")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "new")
	recorder := httptest.NewRecorder()
	s.migrationPreflight(recorder, httptest.NewRequest(http.MethodPost, "/api/migration/preflight", nil),
		map[string]any{"source": source, "destination": destination})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("dry-run must not create the destination")
	}
	if !strings.Contains(recorder.Body.String(), "apply_requires_offline_confirmation") {
		t.Fatal("response must state that apply needs offline confirmation")
	}
	_ = base
}
