package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTransitionConfig(t *testing.T, dir, repo string) string {
	t.Helper()
	document := map[string]any{
		"listen": ":7863",
		"update": map[string]any{"enabled": true, "repo": repo},
		"auth_dir": "./auths",
	}
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readRepo(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Update struct {
			Repo string `json:"repo"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document.Update.Repo
}

// v3.17.0 transitional rule: an old deployment must be handed to the LoomGate
// release repository exactly once, with the original bytes kept for rollback.
func TestTransitionSwitchesLegacyRepoOnce(t *testing.T) {
	dir := t.TempDir()
	path := writeTransitionConfig(t, dir, "dddmiku/workbuddy2api-maintenance")
	before, _ := os.ReadFile(path)
	changed, err := migrateUpdateRepo(path)
	if err != nil || !changed {
		t.Fatalf("migration did not run: changed=%v err=%v", changed, err)
	}
	if got := readRepo(t, path); got != loomGateRepo {
		t.Fatalf("repo=%q want %q", got, loomGateRepo)
	}
	backup, err := os.ReadFile(path + transitionBackupSuffix)
	if err != nil || string(backup) != string(before) {
		t.Fatal("original config bytes were not preserved for rollback")
	}
	marker, err := os.ReadFile(filepath.Join(dir, transitionMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	var record transitionRecord
	if err := json.Unmarshal(marker, &record); err != nil || record.From != "dddmiku/workbuddy2api-maintenance" || record.To != loomGateRepo {
		t.Fatalf("transition marker is incomplete: %+v", record)
	}
	// Second start is a no-op, not a second rewrite.
	if changed, err := migrateUpdateRepo(path); err != nil || changed {
		t.Fatalf("migration repeated: changed=%v err=%v", changed, err)
	}
}

// An operator who deliberately points at a private mirror must never be
// silently redirected.
func TestTransitionLeavesDeliberateRepoAlone(t *testing.T) {
	for _, repo := range []string{loomGateRepo, "corp/internal-mirror"} {
		dir := t.TempDir()
		path := writeTransitionConfig(t, dir, repo)
		changed, err := migrateUpdateRepo(path)
		if err != nil || changed {
			t.Fatalf("%s was rewritten: changed=%v err=%v", repo, changed, err)
		}
		if got := readRepo(t, path); got != repo {
			t.Fatalf("repo changed to %q", got)
		}
		if _, err := os.Stat(filepath.Join(dir, transitionMarkerName)); !os.IsNotExist(err) {
			t.Fatal("marker written for an untouched config")
		}
	}
}

// After the one-time hand-off, an operator switching back to a legacy value is
// respected: the marker proves the transition already happened.
func TestTransitionDoesNotUndoOperatorChoiceAfterMarker(t *testing.T) {
	dir := t.TempDir()
	path := writeTransitionConfig(t, dir, "dddmiku/workbuddy2api-maintenance")
	if changed, err := migrateUpdateRepo(path); err != nil || !changed {
		t.Fatalf("first migration failed: %v %v", changed, err)
	}
	if err := os.WriteFile(path, []byte(`{"update":{"repo":"dddmiku/workbuddy2api-maintenance"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := migrateUpdateRepo(path); err != nil || changed {
		t.Fatalf("post-marker operator choice was overwritten: %v %v", changed, err)
	}
	if got := readRepo(t, path); got != "dddmiku/workbuddy2api-maintenance" {
		t.Fatalf("operator value lost: %q", got)
	}
}
