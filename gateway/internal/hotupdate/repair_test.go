package hotupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/version"
)

func repairNativeFixture(t *testing.T) ([]byte, RepairOptions) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	native, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := signedStagingManager(t, bundleSupported(), native)
	oldVersion := version.Version
	version.Version = "v99.0.2"
	t.Cleanup(func() { version.Version = oldVersion })
	options := RepairOptions{Update: manager.opts, APIBase: manager.client.APIBase}
	options.Update.Listener = nil
	return native, options
}

func TestRepairStagesSameVersionWithoutOpeningRunningState(t *testing.T) {
	_, opts := repairNativeFixture(t)
	root := t.TempDir()
	enginePath := filepath.Join(root, "task-engine.json")
	engine, err := taskengine.Open(taskengine.Options{Path: enginePath})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	vaultPath := filepath.Join(root, "provider-accounts")
	vault, err := accountvault.Open(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	stateBefore, _ := os.ReadFile(enginePath)
	status, err := RepairUpdate(t.Context(), opts, "stage")
	if err != nil || !status.RestartRequired || status.Target != version.Version {
		t.Fatalf("signed same-tag recovery failed: %v", err)
	}
	staged := CurrentBinary(opts.Update.Dir)
	if staged == "" {
		t.Fatal("recovery did not commit the next-start pointer")
	}
	for _, action := range []string{"status", "stage"} {
		status, err = RepairUpdate(t.Context(), opts, action)
		if err != nil || status.State != "staged" || CurrentBinary(opts.Update.Dir) != staged {
			t.Fatal("repeated repair did not reuse the committed candidate")
		}
	}
	if other, err := taskengine.Open(taskengine.Options{Path: enginePath}); err == nil {
		other.Close()
		t.Fatal("running task ownership was changed")
	}
	if other, err := accountvault.Open(vaultPath); err == nil {
		other.Close()
		t.Fatal("running vault ownership was changed")
	}
	stateAfter, _ := os.ReadFile(enginePath)
	if string(stateBefore) != string(stateAfter) {
		t.Fatal("repair changed task state")
	}
	status, err = RepairUpdate(t.Context(), opts, "rollback")
	if err != nil || status.State != "rolled_back" || CurrentBinary(opts.Update.Dir) != "" {
		t.Fatal("rollback did not restore an originally absent pointer")
	}
}

func TestRepairRollbackOnlyTouchesItsOwnUnchangedPointer(t *testing.T) {
	for _, mutation := range []string{"current", "previous"} {
		t.Run(mutation, func(t *testing.T) {
			native, opts := repairNativeFixture(t)
			name, _ := assetName()
			previous := filepath.Join(opts.Update.Dir, "v0.0.1-"+name)
			if err := os.WriteFile(previous, native, 0700); err != nil {
				t.Fatal(err)
			}
			if err := writeCurrentPointer(opts.Update.Dir, previous); err != nil {
				t.Fatal(err)
			}
			if _, err := RepairUpdate(t.Context(), opts, "stage"); err != nil {
				t.Fatal(err)
			}
			if mutation == "current" {
				if err := writeCurrentPointer(opts.Update.Dir, previous); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(previous, append(native, '\n'), 0700); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(filepath.Join(opts.Update.Dir, "current"))
			if _, err := RepairUpdate(t.Context(), opts, "rollback"); err == nil {
				t.Fatal("rollback overwrote an externally changed deployment")
			}
			after, _ := os.ReadFile(filepath.Join(opts.Update.Dir, "current"))
			if string(before) != string(after) {
				t.Fatal("rejected rollback mutated the pointer")
			}
		})
	}
}

func TestRepairRefusesUnsignedBuildBeforeCreatingDirectory(t *testing.T) {
	oldKey := version.ReleaseKey
	version.ReleaseKey = ""
	defer func() { version.ReleaseKey = oldKey }()
	dir := filepath.Join(t.TempDir(), "not-created")
	if _, err := RepairUpdate(t.Context(), RepairOptions{Update: Options{Dir: dir}}, "stage"); err == nil {
		t.Fatal("unsigned repair was accepted")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("unsigned repair wrote a directory")
	}
}

func TestRepairRejectsOutOfDirectoryCurrentPointer(t *testing.T) {
	native, opts := repairNativeFixture(t)
	name, _ := assetName()
	outside := filepath.Join(t.TempDir(), "v0.0.1-"+name)
	if err := os.WriteFile(outside, native, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentPointer(opts.Update.Dir, outside); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(opts.Update.Dir, "current"))
	if _, err := RepairUpdate(t.Context(), opts, "stage"); err == nil {
		t.Fatal("out-of-directory startup pointer was trusted")
	}
	after, _ := os.ReadFile(filepath.Join(opts.Update.Dir, "current"))
	if string(before) != string(after) {
		t.Fatal("invalid pointer was changed")
	}
}

func TestRepairRecoveryLockPreventsConcurrentStaging(t *testing.T) {
	_, opts := repairNativeFixture(t)
	lock, err := os.OpenFile(filepath.Join(opts.Update.Dir, "transition-repair.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := lockRepair(lock); err != nil {
		t.Fatal(err)
	}
	if _, err := RepairUpdate(t.Context(), opts, "stage"); err == nil {
		t.Fatal("parallel repair acquired the existing recovery lock")
	}
}

func TestRepairRollsBackInterruptedPreparationWithoutChangingCurrent(t *testing.T) {
	native, opts := repairNativeFixture(t)
	name, _ := assetName()
	candidate := filepath.Join(opts.Update.Dir, "v99.0.2-"+name)
	if err := os.WriteFile(candidate, native, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(native)
	record := repairJournal{Format: 1, State: "preparing", Candidate: candidate, Digest: hex.EncodeToString(sum[:]), Target: "v99.0.2"}
	if err := writeRepairJournal(opts.Update.Dir, record); err != nil {
		t.Fatal(err)
	}
	if _, err := RepairUpdate(t.Context(), opts, "stage"); err == nil {
		t.Fatal("interrupted preparation was automatically replayed")
	}
	status, err := RepairUpdate(t.Context(), opts, "rollback")
	if err != nil || status.State != "rolled_back" || CurrentBinary(opts.Update.Dir) != "" {
		t.Fatal("interrupted preparation could not restore its original pointer")
	}
}

func TestRepairRejectsIncorrectSignatureWithoutCommitting(t *testing.T) {
	_, opts := repairNativeFixture(t)
	version.ReleaseKey = strings.Repeat("00", 32)
	if _, err := RepairUpdate(t.Context(), opts, "stage"); err == nil {
		t.Fatal("incorrect release signature accepted")
	}
	if CurrentBinary(opts.Update.Dir) != "" {
		t.Fatal("incorrect signature changed startup pointer")
	}
}

func TestRepairStatusAndRollbackRejectChangedRuntimeHelpers(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	native, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := signedStagingManager(t, true, native)
	opts := RepairOptions{Update: manager.opts, APIBase: manager.client.APIBase}
	opts.Update.Listener = nil
	if _, err := RepairUpdate(t.Context(), opts, "stage"); err != nil {
		t.Fatal(err)
	}
	pointer := CurrentBinary(opts.Update.Dir)
	if err := os.WriteFile(filepath.Join(filepath.Dir(pointer), "credit"), []byte("changed fixture helper"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"status", "rollback"} {
		if _, err := RepairUpdate(t.Context(), opts, action); err == nil {
			t.Fatal("changed helper was presented as a verified runtime")
		}
		if CurrentBinary(opts.Update.Dir) != pointer {
			t.Fatal("rejected recovery changed startup pointer")
		}
	}
}
