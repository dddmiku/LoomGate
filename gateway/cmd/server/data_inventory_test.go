package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInventoryDigestChangesWithContentAndTracksCounts(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"config", "auths", "panel-data", "data"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("config/config.json", `{"listen":"127.0.0.1:7863"}`)
	write("auths/a.json", `{"id":"synthetic-a"}`)
	write("auths/b.json", `{"id":"synthetic-b"}`)
	write("panel-data/credentials.json", `{"username":"synthetic"}`)
	write("data/api_keys.json", `{"keys":[]}`)
	write("data/task-engine.json", `{"version":4}`)
	write("data/state.json", `{"accounts":{}}`)

	first, err := inventory(root)
	if err != nil {
		t.Fatal(err)
	}
	if first["accounts"].Files != 2 || first["config"].Files != 1 || first["keys"].Files != 1 || first["plans"].Files != 1 {
		t.Fatalf("counts = %+v", first)
	}
	second, err := inventory(root)
	if err != nil {
		t.Fatal(err)
	}
	if first["accounts"].Digest != second["accounts"].Digest {
		t.Fatal("inventory must be deterministic")
	}
	write("auths/b.json", `{"id":"changed"}`)
	third, err := inventory(root)
	if err != nil {
		t.Fatal(err)
	}
	if third["accounts"].Digest == first["accounts"].Digest {
		t.Fatal("content change must change the aggregate digest")
	}
	if third["config"].Digest != first["config"].Digest {
		t.Fatal("unrelated category changed")
	}
}

func TestInventoryRejectsSymlinksAndMissingFilesAreEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "auths"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "auths", "target.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "auths", "target.json"), filepath.Join(root, "auths", "link.json")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}
	if _, err := inventory(root); err == nil {
		t.Fatal("symlink in a persistent category must be rejected")
	}
}
