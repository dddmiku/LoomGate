package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/version"
)

func writeTransitionConfig(t *testing.T, dir, repo string) string {
	t.Helper()
	document := map[string]any{
		"listen":   ":7863",
		"update":   map[string]any{"enabled": true, "repo": repo},
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

func TestTransitionCanResumeAfterBackupWasWritten(t *testing.T) {
	dir := t.TempDir()
	path := writeTransitionConfig(t, dir, "dddmiku/workbuddy2api-maintenance")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A bind mount can reject replacing config.json after the fixed backup
	// has already been created. An operator retry must not fail with EEXIST.
	if err := os.WriteFile(path+transitionBackupSuffix, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := migrateUpdateRepo(path); err != nil || !changed {
		t.Fatalf("interrupted migration could not resume: changed=%v error=%v", changed, err)
	}
	backup, _ := os.ReadFile(path + transitionBackupSuffix)
	if string(backup) != string(before) {
		t.Fatal("retry changed the rollback bytes")
	}
}

func TestEffectiveUpdateRepoNeedsNoWritableConfigAndDoesNotMutateLegacyState(t *testing.T) {
	previous := version.Version
	t.Cleanup(func() { version.Version = previous })
	for _, current := range []string{"v3.17.1", "v1.0.6"} {
		for _, repo := range []string{"", "dddmiku/workbuddy2api-maintenance", "dddmiku/workbuddy2api", loomGateRepo, "corp/internal-mirror"} {
			t.Run(current+"/"+repo, func(t *testing.T) {
				version.Version = current
				dir := t.TempDir()
				path := writeTransitionConfig(t, dir, repo)
				before, _ := os.ReadFile(path)
				if err := os.Chmod(path, 0o400); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
				want := repo
				if want == "" || legacyRepo(want) {
					want = loomGateRepo
				}
				if got := effectiveUpdateRepo(path, repo); got != want {
					t.Fatalf("effective feed=%q want %q", got, want)
				}
				after, _ := os.ReadFile(path)
				files, _ := os.ReadDir(dir)
				if string(after) != string(before) || len(files) != 1 {
					t.Fatal("candidate startup changed legacy config or left migration sidecars")
				}
			})
		}
	}
}

func TestEffectiveUpdateRepoPreservesPostMigrationOperatorChoice(t *testing.T) {
	previous := version.Version
	version.Version = "v3.17.1"
	t.Cleanup(func() { version.Version = previous })
	dir := t.TempDir()
	path := writeTransitionConfig(t, dir, "dddmiku/workbuddy2api-maintenance")
	if changed, err := migrateUpdateRepo(path); err != nil || !changed {
		t.Fatal("synthetic completed migration failed")
	}
	if got := effectiveUpdateRepo(path, "dddmiku/workbuddy2api-maintenance"); got != "dddmiku/workbuddy2api-maintenance" {
		t.Fatal("the operator's post-marker choice was overwritten")
	}
}

func TestEffectiveUpdateRepoKeepsUnmodifiedLegacyBuildOnItsBridgeFeed(t *testing.T) {
	previous := version.Version
	version.Version = "v3.16.1"
	t.Cleanup(func() { version.Version = previous })
	repo := "dddmiku/workbuddy2api-maintenance"
	if got := effectiveUpdateRepo("", repo); got != repo {
		t.Fatal("unmodified legacy build skipped its bridge release")
	}
}

func TestTransitionDoesNotReplaceADifferentRollbackCopy(t *testing.T) {
	dir := t.TempDir()
	path := writeTransitionConfig(t, dir, "dddmiku/workbuddy2api-maintenance")
	if err := os.WriteFile(path+transitionBackupSuffix, []byte("different rollback bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := migrateUpdateRepo(path); err == nil || changed {
		t.Fatal("migration overwrote a different rollback copy")
	}
}
