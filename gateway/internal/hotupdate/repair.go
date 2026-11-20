package hotupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"debug/elf"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"workbuddy2api/internal/version"
)

const repairJournalName = "transition-repair.json"

var repairTagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type RepairOptions struct {
	Update Options
	Target string
	// APIBase is an injected release fixture seam; the installed CLI leaves it
	// empty. Every staged asset still requires the embedded release key.
	APIBase string
}

type RepairStatus struct {
	State           string `json:"state"`
	Target          string `json:"target,omitempty"`
	RestartRequired bool   `json:"restart_required"`
	Message         string `json:"message"`
}

type repairJournal struct {
	Format         int    `json:"format"`
	State          string `json:"state"`
	Previous       string `json:"previous,omitempty"`
	PreviousDigest string `json:"previous_sha256,omitempty"`
	Candidate      string `json:"candidate"`
	Digest         string `json:"sha256"`
	Target         string `json:"target"`
}

// RepairUpdate runs before any task engine, account vault or listener opens. It
// repairs the next-start pointer while the old deployment continues serving.
// It never signals the old process or executes the downloaded candidate.
func RepairUpdate(ctx context.Context, opts RepairOptions, action string) (RepairStatus, error) {
	dir, err := filepath.Abs(opts.Update.Dir)
	if err != nil || strings.TrimSpace(opts.Update.Dir) == "" {
		return RepairStatus{}, errors.New("repair_directory_invalid")
	}
	if action != "stage" && action != "status" && action != "rollback" {
		return RepairStatus{}, errors.New("repair_action_invalid")
	}
	if opts.Target != "" && !repairTagPattern.MatchString(opts.Target) {
		return RepairStatus{}, errors.New("repair_target_invalid")
	}
	if action == "stage" && !version.ReleaseVerificationEnabled() {
		return RepairStatus{}, errors.New("repair_signing_key_required")
	}
	if action == "stage" {
		key, err := hex.DecodeString(strings.TrimSpace(version.ReleaseKey))
		if err != nil || len(key) != ed25519.PublicKeySize {
			return RepairStatus{}, errors.New("repair_signing_key_required")
		}
	}
	if action == "stage" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return RepairStatus{}, errors.New("repair_directory_unavailable")
		}
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return RepairStatus{}, errors.New("repair_directory_unavailable")
	}
	if action != "status" {
		lockPath := filepath.Join(dir, "transition-repair.lock")
		if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
			return RepairStatus{}, errors.New("repair_lock_invalid")
		}
		lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			return RepairStatus{}, errors.New("repair_lock_unavailable")
		}
		defer lock.Close()
		if lockRepair(lock) != nil {
			return RepairStatus{}, errors.New("repair_busy")
		}
	}
	current, err := repairCurrent(dir)
	if err != nil {
		return RepairStatus{}, err
	}
	record, exists, err := readRepairJournal(dir)
	if err != nil {
		return RepairStatus{}, err
	}
	if exists {
		if err := validateRepairPrevious(dir, record); err != nil {
			return RepairStatus{}, err
		}
		if !(current == record.Previous && (record.State == "preparing" || record.State == "rolled_back")) {
			if err := validateRepairJournal(dir, record); err != nil {
				return RepairStatus{}, err
			}
		}
		if record.State == "rolled_back" {
			if current != record.Previous {
				return RepairStatus{}, errors.New("repair_pointer_conflict")
			}
			if action != "stage" {
				return RepairStatus{State: "rolled_back", Target: record.Target, Message: "已恢复原启动指针；运行版本请从服务读回核验。"}, nil
			}
		} else if current == record.Candidate {
			if action == "rollback" {
				return rollbackRepair(dir, current, record)
			}
			if opts.Target != "" && opts.Target != record.Target {
				return RepairStatus{}, errors.New("repair_pending_restart")
			}
			return RepairStatus{State: "staged", Target: record.Target, RestartRequired: true, Message: "已验签并准备好启动指针；请在维护窗口重启网关或容器，再核验实际运行版本。"}, nil
		} else if current == record.Previous && record.State == "preparing" {
			if action == "rollback" {
				record.State = "rolled_back"
				if err := writeRepairJournal(dir, record); err != nil {
					return RepairStatus{}, err
				}
				return RepairStatus{State: "rolled_back", Message: "暂存未提交，原启动指针保持不变。"}, nil
			}
			return RepairStatus{}, errors.New("repair_interrupted_rollback_required")
		} else {
			return RepairStatus{}, errors.New("repair_pointer_conflict")
		}
	}
	if action == "status" {
		return RepairStatus{State: "not_prepared", Message: "没有本工具准备的过渡修复。"}, nil
	}
	if action == "rollback" {
		return RepairStatus{}, errors.New("repair_not_prepared")
	}
	managerOptions := opts.Update
	managerOptions.Dir, managerOptions.Enabled = dir, true
	managerOptions.Repair, managerOptions.RequireRestart = true, true
	managerOptions.StageInstaller, managerOptions.OnSwitched = nil, nil
	managerOptions.BeforeStageCommit = func(candidate string) error {
		// Check the old pointer again immediately before the atomic commit.
		observed, err := repairCurrent(dir)
		if err != nil || observed != current {
			return errors.New("repair_pointer_conflict")
		}
		digest, err := repairBinaryDigest(dir, candidate)
		if err != nil {
			return err
		}
		target := opts.Target
		if managerOptions.Bundle {
			raw, err := os.ReadFile(filepath.Join(filepath.Dir(candidate), "manifest.json"))
			var manifest runtimeManifest
			if err != nil || json.Unmarshal(raw, &manifest) != nil {
				return errors.New("repair_candidate_invalid")
			}
			target = manifest.Version
		}
		previousDigest := ""
		if current != "" {
			previousDigest, err = repairBinaryDigest(dir, current)
			if err != nil {
				return err
			}
		}
		record = repairJournal{Format: 1, State: "preparing", Previous: current, PreviousDigest: previousDigest, Candidate: candidate, Digest: digest, Target: target}
		return writeRepairJournal(dir, record)
	}
	manager := NewManager(managerOptions)
	manager.client.APIBase = opts.APIBase
	status, err := manager.Apply(ctx, opts.Target)
	if err != nil {
		return RepairStatus{}, errors.New("repair_stage_failed")
	}
	record.Target, record.State = status.StagedVersion, "staged"
	if err := writeRepairJournal(dir, record); err != nil {
		return RepairStatus{}, err
	}
	return RepairStatus{State: "staged", Target: record.Target, RestartRequired: true, Message: "修复更新已验签、完整校验并暂存；旧服务继续运行。请在维护窗口重启网关或容器，再核验实际运行版本。"}, nil
}

func repairCurrent(dir string) (string, error) {
	path := filepath.Join(dir, "current")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return "", errors.New("repair_pointer_invalid")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("repair_pointer_unavailable")
	}
	value := strings.TrimSpace(string(raw))
	if _, err = repairBinaryDigest(dir, value); err != nil {
		return "", err
	}
	abs, _ := filepath.Abs(value)
	return abs, nil
}

func repairBinaryDigest(dir, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil || strings.TrimSpace(path) == "" {
		return "", errors.New("repair_candidate_invalid")
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("repair_pointer_outside_update_directory")
	}
	for p := abs; p != dir; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("repair_candidate_invalid")
		}
	}
	name, _ := assetName()
	base := filepath.Base(abs)
	var runtimeRaw []byte
	var manifest runtimeManifest
	if base != "wb2api" && !strings.HasSuffix(base, "-"+name) {
		return "", errors.New("repair_platform_invalid")
	}
	if base == "wb2api" {
		info, err := os.Lstat(filepath.Join(filepath.Dir(abs), "manifest.json"))
		if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
			return "", errors.New("repair_manifest_invalid")
		}
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(abs), "manifest.json"))
		if err != nil || json.Unmarshal(raw, &manifest) != nil || manifest.Format != 1 || manifest.OS != "linux" || (manifest.Arch != runtime.GOARCH && manifest.Arch != runtimeArchName()) || manifest.Files["wb2api"] == "" {
			return "", errors.New("repair_manifest_invalid")
		}
		runtimeRaw = raw
	}
	file, err := os.Open(abs)
	if err != nil {
		return "", errors.New("repair_candidate_unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxReleaseBytes {
		return "", errors.New("repair_candidate_invalid")
	}
	if runtime.GOOS == "windows" {
		image, err := pe.NewFile(file)
		if err != nil || (runtime.GOARCH == "amd64" && image.FileHeader.Machine != pe.IMAGE_FILE_MACHINE_AMD64) {
			return "", errors.New("repair_platform_invalid")
		}
	} else {
		image, err := elf.NewFile(file)
		want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64, "arm": elf.EM_ARM}[runtime.GOARCH]
		if err != nil || image.Machine != want {
			return "", errors.New("repair_platform_invalid")
		}
	}
	if _, err = file.Seek(0, 0); err != nil {
		return "", errors.New("repair_candidate_unavailable")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", errors.New("repair_candidate_unavailable")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if base == "wb2api" {
		return repairRuntimeDigest(filepath.Dir(abs), manifest, runtimeRaw, digest)
	}
	return digest, nil
}

// Validate both the old 18-member runtime and the current complete runtime.
// A modified helper makes a staged or rollback target unusable even when the
// main executable has not changed, so the journal binds the whole package.
func repairRuntimeDigest(dir string, manifest runtimeManifest, raw []byte, binaryDigest string) (string, error) {
	present := 0
	for name := range doubaoRuntimeFiles {
		if manifest.Files[name] != "" {
			present++
		}
	}
	expected := len(runtimeFiles)
	if present == 0 {
		expected = legacyRuntimeFileCount()
	} else if present != len(doubaoRuntimeFiles) {
		return "", errors.New("repair_manifest_invalid")
	}
	if len(manifest.Files) != expected {
		return "", errors.New("repair_manifest_invalid")
	}
	names := make([]string, 0, len(manifest.Files))
	for name, want := range manifest.Files {
		if _, ok := runtimeFiles[name]; !ok || len(want) != 64 {
			return "", errors.New("repair_manifest_invalid")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	fingerprint := sha256.New()
	_, _ = fingerprint.Write(raw)
	for _, name := range names {
		want := manifest.Files[name]
		got := binaryDigest
		if name != "wb2api" {
			path := filepath.Join(dir, filepath.FromSlash(name))
			for cursor := path; cursor != dir; cursor = filepath.Dir(cursor) {
				info, err := os.Lstat(cursor)
				if err != nil || info.Mode()&os.ModeSymlink != 0 {
					return "", errors.New("repair_runtime_changed")
				}
			}
			file, err := os.Open(path)
			if err != nil {
				return "", errors.New("repair_runtime_changed")
			}
			info, statErr := file.Stat()
			if statErr != nil || !info.Mode().IsRegular() || info.Size() > maxReleaseBytes {
				file.Close()
				return "", errors.New("repair_runtime_changed")
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return "", errors.New("repair_runtime_changed")
			}
			got = hex.EncodeToString(hash.Sum(nil))
		}
		if !strings.EqualFold(want, got) {
			return "", errors.New("repair_runtime_changed")
		}
		_, _ = fingerprint.Write([]byte(name + ":" + got + "\n"))
	}
	return hex.EncodeToString(fingerprint.Sum(nil)), nil
}

func readRepairJournal(dir string) (repairJournal, bool, error) {
	path := filepath.Join(dir, repairJournalName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return repairJournal{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 32768 {
		return repairJournal{}, false, errors.New("repair_journal_invalid")
	}
	raw, err := os.ReadFile(path)
	var record repairJournal
	if err != nil || json.Unmarshal(raw, &record) != nil || record.Format != 1 ||
		(record.State != "preparing" && record.State != "staged" && record.State != "rolled_back") ||
		(record.Target != "" && !repairTagPattern.MatchString(record.Target)) {
		return repairJournal{}, false, errors.New("repair_journal_invalid")
	}
	return record, true, nil
}

func validateRepairJournal(dir string, record repairJournal) error {
	if record.State != "preparing" && record.State != "staged" && record.State != "rolled_back" {
		return errors.New("repair_journal_invalid")
	}
	digest, err := repairBinaryDigest(dir, record.Candidate)
	if err != nil || digest != record.Digest {
		return errors.New("repair_candidate_changed")
	}
	return validateRepairPrevious(dir, record)
}

func validateRepairPrevious(dir string, record repairJournal) error {
	if record.Previous == "" {
		if record.PreviousDigest != "" {
			return errors.New("repair_previous_invalid")
		}
		return nil
	}
	digest, err := repairBinaryDigest(dir, record.Previous)
	if err != nil || digest != record.PreviousDigest {
		return errors.New("repair_previous_changed")
	}
	return nil
}

func rollbackRepair(dir, expected string, record repairJournal) (RepairStatus, error) {
	current, err := repairCurrent(dir)
	if err != nil || current != expected || current != record.Candidate {
		return RepairStatus{}, errors.New("repair_pointer_conflict")
	}
	if record.Previous == "" {
		err = os.Remove(filepath.Join(dir, "current"))
	} else {
		err = writeCurrentPointer(dir, record.Previous)
	}
	if err != nil {
		return RepairStatus{}, errors.New("repair_rollback_failed")
	}
	record.State = "rolled_back"
	if err = writeRepairJournal(dir, record); err != nil {
		return RepairStatus{}, err
	}
	return RepairStatus{State: "rolled_back", Target: record.Target, Message: "已还原修复前的启动指针；当前服务未被终止，实际运行版本请单独读回核验。"}, nil
}

func writeRepairJournal(dir string, record repairJournal) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return errors.New("repair_journal_invalid")
	}
	file, err := os.CreateTemp(dir, ".transition-repair-")
	if err != nil {
		return errors.New("repair_journal_unavailable")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Close()
	}
	if err == nil {
		err = os.Rename(file.Name(), filepath.Join(dir, repairJournalName))
	}
	if err != nil {
		return errors.New("repair_journal_unavailable")
	}
	return nil
}
