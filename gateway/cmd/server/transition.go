package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Transitional release support (v3.17.0).
//
// A v3.16.1 deployment has update.repo pointing at the legacy maintenance
// repository. After upgrading to this build it would keep reading that repo and
// stay on 3.17.0 forever. The upgrade must be a one-way, reversible hand-off:
// rewrite the configured repo to the LoomGate release repository exactly once,
// keep the original value in a sidecar, and never rewrite a value the operator
// set deliberately later.
const (
	loomGateRepo          = "dddmiku/LoomGate"
	legacyRepos           = "dddmiku/workbuddy2api-maintenance,dddmiku/workbuddy2api"
	transitionMarkerName  = "loomgate-transition.json"
	transitionBackupSuffix = ".pre-loomgate"
)

type transitionRecord struct {
	From      string `json:"from"`
	To        string `json:"to"`
	AtUTC     string `json:"at_utc"`
	ConfigSHA string `json:"config_sha256_before"`
}

// migrateUpdateRepo points the update source at LoomGate once.
//
// It is deliberately conservative:
//   - only an empty repo (built-in default) or a known legacy repo is rewritten;
//   - an already-LoomGate or operator-chosen third repo is left untouched;
//   - the original config bytes are preserved beside it for rollback;
//   - an existing marker means the hand-off already ran, so a later operator
//     change back to a legacy value is respected rather than re-migrated.
func migrateUpdateRepo(configPath string) (bool, error) {
	if strings.TrimSpace(configPath) == "" {
		return false, nil
	}
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return false, err
	}
	dir := filepath.Dir(absolute)
	markerPath := filepath.Join(dir, transitionMarkerName)
	if _, err := os.Stat(markerPath); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	// #nosec G304 -- configPath comes from the -config flag, same trust as Load.
	raw, err := os.ReadFile(absolute)
	if err != nil {
		return false, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return false, err
	}
	var update map[string]json.RawMessage
	if section, ok := document["update"]; ok {
		if err := json.Unmarshal(section, &update); err != nil {
			return false, err
		}
	}
	if update == nil {
		update = map[string]json.RawMessage{}
	}
	var current string
	if value, ok := update["repo"]; ok {
		_ = json.Unmarshal(value, &current)
	}
	current = strings.TrimSpace(current)
	if current != "" && !legacyRepo(current) {
		// Already pointing somewhere deliberate (LoomGate or a private mirror).
		return false, nil
	}
	encoded, err := json.Marshal(loomGateRepo)
	if err != nil {
		return false, err
	}
	update["repo"] = encoded
	section, err := json.Marshal(update)
	if err != nil {
		return false, err
	}
	document["update"] = section
	next, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return false, err
	}
	next = append(next, '\n')
	backup := absolute + transitionBackupSuffix
	if err := writeExclusive(backup, raw); err != nil {
		return false, err
	}
	temporary := absolute + ".transition-pending"
	if err := writeExclusive(temporary, next); err != nil {
		return false, err
	}
	if err := os.Rename(temporary, absolute); err != nil {
		_ = os.Remove(temporary)
		return false, err
	}
	record := transitionRecord{From: current, To: loomGateRepo,
		AtUTC: time.Now().UTC().Format(time.RFC3339), ConfigSHA: sha256Hex(raw)}
	payload, _ := json.MarshalIndent(record, "", "  ")
	if err := writeExclusive(markerPath, append(payload, '\n')); err != nil {
		return true, fmt.Errorf("repo switched but marker not written: %w", err)
	}
	return true, nil
}

func legacyRepo(value string) bool {
	for _, candidate := range strings.Split(legacyRepos, ",") {
		if strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func writeExclusive(path string, raw []byte) error {	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
