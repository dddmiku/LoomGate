// ═══ 更新日志 ═══
// 2026-09-26：锁定更新目录清理：保留当前与上一个运行目录，不动指针与非本工具文件。
package hotupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneUpdateDirKeepsCurrentAndPrevious(t *testing.T) {
	dir := t.TempDir()
	mkRuntime := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "wb2api"), []byte("bin"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, managedRuntimeMarker), []byte(managedRuntimeMarkerContent), 0600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		_ = os.Chtimes(path, old, old)
		return filepath.Join(path, "wb2api")
	}
	current := mkRuntime("runtime-aaa", 0)
	previous := mkRuntime("runtime-bbb", time.Hour)
	older := mkRuntime("runtime-ccc", 2*time.Hour)
	for _, name := range []string{"v2.4.0-wb2api-linux-arm64", "v2.4.1-wb2api-runtime-linux-arm64.tar.gz", ".download-123.part", "current"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pruneUpdateDir(dir, current)
	for _, want := range []string{filepath.Dir(current), filepath.Dir(previous), filepath.Join(dir, "current")} {
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("kept path missing: %v", err)
		}
	}
	for _, gone := range []string{older, filepath.Join(dir, "v2.4.0-wb2api-linux-arm64"), filepath.Join(dir, "v2.4.1-wb2api-runtime-linux-arm64.tar.gz")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("expected removal of %s", gone)
		}
	}
}

func TestPruneUpdateDirLeavesForeignFilesAndMissingDirAlone(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "backup.tar.gz")
	if err := os.WriteFile(archive, []byte("user archive"), 0600); err != nil {
		t.Fatal(err)
	}
	pruneUpdateDir(dir, "")
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("unrelated file was removed")
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal("unrelated archive was removed")
	}
	pruneUpdateDir(filepath.Join(dir, "missing"), "")
}

func TestPruneUpdateDirBoundsRepeatedStagingWithoutRemovingActiveRuntime(t *testing.T) {
	dir := t.TempDir()
	active := ""
	for generation := range 5 {
		runtimeDir := filepath.Join(dir, fmt.Sprintf("runtime-generation-%d", generation))
		if err := os.Mkdir(runtimeDir, 0700); err != nil {
			t.Fatal(err)
		}
		staged := filepath.Join(runtimeDir, "wb2api")
		if err := os.WriteFile(filepath.Join(runtimeDir, managedRuntimeMarker), []byte(managedRuntimeMarkerContent), 0600); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{staged, filepath.Join(runtimeDir, "credit")} {
			if err := os.WriteFile(path, []byte("fixture runtime"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		// Deliberately make each staged directory older than the live one:
		// mtime must never decide which of the two owners is safe to delete.
		old := time.Now().Add(-time.Duration(generation+1) * time.Hour)
		if err := os.Chtimes(runtimeDir, old, old); err != nil {
			t.Fatal(err)
		}
		if err := writeCurrentPointer(dir, staged); err != nil {
			t.Fatal(err)
		}
		pruneUpdateDir(dir, staged, active)
		for _, binary := range []string{staged, active} {
			if binary != "" {
				if _, err := os.Stat(filepath.Join(filepath.Dir(binary), "credit")); err != nil {
					t.Fatal("cleanup removed a running or staged helper")
				}
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range entries {
			if entry.IsDir() {
				count++
			}
		}
		if count > 2 {
			t.Fatalf("successive upgrades left %d runtime directories", count)
		}
		active = staged // simulate the next normal gateway restart
	}
}

func TestPruneUpdateDirIgnoresUnmanagedDirectoriesAndMarkerLinks(t *testing.T) {
	dir := t.TempDir()
	for index, marker := range []string{"", "foreign marker", managedRuntimeMarkerContent} {
		path := filepath.Join(dir, fmt.Sprintf("runtime-unmanaged-%d", index))
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "notes.txt"), []byte("user file"), 0600); err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			if err := os.WriteFile(filepath.Join(path, managedRuntimeMarker), []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if index == 2 {
			target := filepath.Join(dir, "user-marker")
			if err := os.WriteFile(target, []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
			// Hosts without symlink privilege still test an absent marker.
			_ = os.Symlink(target, filepath.Join(path, managedRuntimeMarker))
		}
		if managedRuntime(path) {
			t.Fatal("unverified or linked marker accepted")
		}
	}
	for index := range 3 {
		path := filepath.Join(dir, fmt.Sprintf("runtime-owned-%d", index))
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, managedRuntimeMarker), []byte(managedRuntimeMarkerContent), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pruneUpdateDir(dir, filepath.Join(dir, "runtime-owned-2", "wb2api"))
	for index := range 3 {
		path := filepath.Join(dir, fmt.Sprintf("runtime-unmanaged-%d", index), "notes.txt")
		if _, err := os.Stat(path); err != nil {
			t.Fatal("unmanaged directory was pruned")
		}
	}
}
