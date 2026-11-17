package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func migrationFixture(t *testing.T, marker string) *Server {
	t.Helper()
	root := t.TempDir()
	if marker != "" {
		if err := os.WriteFile(filepath.Join(root, migrationMarkerName), []byte(marker), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{opts: Options{BaseDir: root}}
	return s
}

func TestMigrationStatusReportsNoMarkerWithoutPretending(t *testing.T) {
	s := migrationFixture(t, "")
	recorder := httptest.NewRecorder()
	s.migrationStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/migration", nil))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("body = %v", body)
	}
	if _, present := body["marker"]; present {
		t.Fatal("missing marker must not be reported as migrated")
	}
	tool, _ := body["tool"].(map[string]any)
	if tool["available"] != false {
		t.Fatalf("tool status = %v", tool)
	}
}

func TestMigrationStatusReadsValidMarkerAndNeverLeaksPaths(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s := migrationFixture(t, `{"format":2,"mode":"copied","file_count":86,`+
		`"inventory_sha256":"`+digest+`","source_config_sha256":"`+digest+`",`+
		`"destination_binding":"`+digest+`","source":"/secret/old-path"}`)
	recorder := httptest.NewRecorder()
	s.migrationStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/migration", nil))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	marker, _ := body["marker"].(map[string]any)
	if marker == nil || marker["file_count"] != float64(86) || marker["inventory_sha256"] != digest {
		t.Fatalf("marker = %v", marker)
	}
	if _, leaked := marker["source"]; leaked {
		t.Fatal("marker view must not echo private source paths")
	}
}

func TestMigrationStatusRejectsCorruptOrOversizedMarker(t *testing.T) {
	for name, marker := range map[string]string{
		"corrupt":  `{"format":2,"file_count":0}`,
		"wrongfmt": `{"format":1,"file_count":3,"inventory_sha256":"` + "a" + `","source_config_sha256":"` + "a" + `","destination_binding":"x"}`,
		"notjson":  `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			s := migrationFixture(t, marker)
			recorder := httptest.NewRecorder()
			s.migrationStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/migration", nil))
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if _, present := body["marker"]; present {
				t.Fatalf("%s marker must not be presented as valid: %v", name, body)
			}
		})
	}
}
