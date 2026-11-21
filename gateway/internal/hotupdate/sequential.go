package hotupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/version"
)

const switchJournalName = "auto-update.json"

// One fixed journal survives a process change. It contains deployment paths and
// public version identifiers only; no account state or request data is copied.
type switchJournal struct {
	Format    int    `json:"format"`
	State     State  `json:"state"`
	Target    string `json:"target"`
	Candidate string `json:"candidate"`
	Previous  string `json:"previous"`
	Error     string `json:"error,omitempty"`
}

func writeSwitchJournal(dir string, journal switchJournal) error {
	path := filepath.Join(dir, switchJournalName)
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("update_journal_invalid")
	}
	raw, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".auto-update-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func readSwitchJournal(dir string) (switchJournal, bool, error) {
	var journal switchJournal
	path := filepath.Join(dir, switchJournalName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return journal, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return journal, false, errors.New("update_journal_invalid")
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &journal) != nil || journal.Format != 1 {
		return journal, false, errors.New("update_journal_invalid")
	}
	return journal, true, nil
}

func (m *Manager) journalStatus(status Status) Status {
	if m.busy || status.State != StateIdle {
		return status
	}
	journal, exists, err := readSwitchJournal(m.opts.Dir)
	if err != nil {
		status.State = StateFailed
		status.LastError = "更新恢复记录不可读"
		return status
	}
	if !exists {
		return status
	}
	status.TargetVersion = journal.Target
	if journal.State == StateFailed && m.checkedAt.IsZero() {
		status.State = StateFailed
		status.LastError = journal.Error
	} else if journal.State == StateHandover {
		active, _ := os.Executable()
		if filepath.Clean(active) == filepath.Clean(journal.Candidate) {
			status.State = StateHandover
		} else {
			status.State = StateFailed
			status.LastError = "上次自动更新未完成，当前运行上一版本"
		}
	}
	return status
}

func (m *Manager) saveSwitchJournal(journal switchJournal) error {
	if m.journalWriter != nil {
		return m.journalWriter(m.opts.Dir, journal)
	}
	return writeSwitchJournal(m.opts.Dir, journal)
}

func (m *Manager) switchPointer(binary string) error {
	if m.pointerWriter != nil {
		return m.pointerWriter(m.opts.Dir, binary)
	}
	return writeCurrentPointer(m.opts.Dir, binary)
}

// Only this child's duplicated listener is accepting at this point: the old
// HTTP servers have drained and every old state owner has closed. Account
// availability (including a legitimate empty pool) is not startup readiness.
func candidateHealth(ctx context.Context, addr, expected, panelPath string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	base := "http://" + net.JoinHostPort(host, port)
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("candidate_health_unreachable")
	}
	defer response.Body.Close()
	var health struct {
		Service string `json:"service"`
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if (response.StatusCode != http.StatusOK && response.StatusCode != http.StatusServiceUnavailable) || response.Header.Get("X-Service") != "workbuddy2api" || json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&health) != nil || health.Service != "workbuddy2api" || strings.TrimPrefix(health.Version, "v") != strings.TrimPrefix(expected, "v") || strings.TrimSpace(health.Commit) == "" {
		return errors.New("candidate_health_identity_mismatch")
	}
	if panelPath != "" {
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, base+panelPath, nil)
		if err != nil {
			return err
		}
		response, err = client.Do(request)
		if err != nil {
			return errors.New("candidate_panel_unreachable")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return errors.New("candidate_panel_not_ready")
		}
	}
	return nil
}

func (m *Manager) applySequential(ctx context.Context, binary, target string, committed *bool) (Status, error) {
	// Duplicates must be made BEFORE Shutdown closes the original listeners.
	file, err := listenerFile(m.opts.Listener)
	if err != nil {
		m.fail(err)
		return m.Status(), err
	}
	defer file.Close()
	var admin *os.File
	if m.opts.AdminListener != nil {
		admin, err = listenerFile(m.opts.AdminListener)
		if err != nil {
			m.fail(err)
			return m.Status(), err
		}
		defer admin.Close()
	}
	previous := m.opts.Binary
	if previous == "" {
		previous, err = os.Executable()
	}
	if err != nil || previous == "" {
		err = errors.New("rollback_binary_unavailable")
		m.fail(err)
		return m.Status(), err
	}
	journal := switchJournal{Format: 1, State: StateHandover, Target: target, Candidate: binary, Previous: previous}
	if err = m.saveSwitchJournal(journal); err != nil {
		m.fail(err)
		return m.Status(), err
	}
	if unix, ok := m.opts.AdminListener.(*net.UnixListener); ok {
		unix.SetUnlinkOnClose(false)
	}
	addr := m.opts.Listener.Addr().String()
	panelPath := ""
	if m.opts.Bundle {
		prefix := strings.TrimRight(m.opts.AdminPrefix, "/")
		if prefix == "" {
			prefix = "/admin"
		}
		panelPath = prefix + "/login"
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout())
	drainErr := m.opts.PrepareSwitch(drainCtx)
	cancel()
	if drainErr == nil {
		err = handoverFiles(binary, m.opts.Args, file, admin, readyTimeout, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := candidateHealth(healthCtx, addr, target, panelPath); err != nil {
				return err
			}
			if err := m.switchPointer(binary); err != nil {
				return err
			}
			// Even if journal/rollback I/O subsequently fails, a durable current
			// pointer must never refer to a directory deferred cleanup deletes.
			*committed = true
			journal.State = StateIdle
			return m.saveSwitchJournal(journal)
		}, ShutdownTimeout())
	} else {
		err = fmt.Errorf("old_runtime_drain: %w", drainErr)
	}
	if err == nil {
		*committed = true
		pruneUpdateDir(m.opts.Dir, binary, previous)
		if m.opts.OnSwitched != nil {
			m.opts.OnSwitched()
		}
		return m.Status(), nil
	}
	// A drained handler/vault cannot be resurrected. Restore the durable pointer
	// and launch an actual old-version process while retaining both listeners.
	journal.State, journal.Error = StateFailed, "新版未通过启动核验，已自动恢复上一版本"
	pointerErr := m.switchPointer(previous)
	if pointerErr == nil {
		*committed = false
	} else {
		journal.Error = "新版启动失败，已恢复旧实例；启动指针恢复失败，更新文件已保留"
	}
	journalErr := m.saveSwitchJournal(journal)
	rollbackErr := handoverFiles(previous, m.opts.Args, file, admin, readyTimeout, func() error {
		healthCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return candidateHealth(healthCtx, addr, version.Version, panelPath)
	}, ShutdownTimeout())
	if rollbackErr != nil {
		journal.Error = "新版启动失败，上一版本自动恢复也失败；请检查更新恢复记录"
		journalErr = errors.Join(journalErr, m.saveSwitchJournal(journal))
	}
	err = errors.Join(err, pointerErr, journalErr, rollbackErr)
	m.fail(errors.New(journal.Error))
	if rollbackErr == nil {
		if m.opts.OnSwitched != nil {
			m.opts.OnSwitched()
		}
	} else if m.opts.OnSwitchFailed != nil {
		m.opts.OnSwitchFailed()
	}
	return m.Status(), err
}
