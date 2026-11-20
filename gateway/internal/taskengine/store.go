package taskengine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type persisted struct {
	Version                  int                      `json:"version"`
	Schedules                map[string]Schedule      `json:"schedules"`
	Runs                     []Run                    `json:"runs"`
	Completed                map[string]time.Time     `json:"completed"`
	Uncertain                map[string]Stage         `json:"uncertain"`
	PendingTasks             map[string]Task          `json:"pending_tasks,omitempty"`
	Receipts                 map[string]actionReceipt `json:"receipts,omitempty"`
	LegacyCompleted          map[string]time.Time     `json:"legacy_completed,omitempty"`
	Work                     map[string]runWork       `json:"work,omitempty"`
	Attempts                 map[string]actionReceipt `json:"attempts,omitempty"`
	LegacyScheduleImportDone bool                     `json:"legacy_schedule_import_done,omitempty"`
	ScheduleDefaultImports   map[string]bool          `json:"schedule_default_imports,omitempty"`
	DeletedSchedules         map[string]bool          `json:"deleted_schedules,omitempty"`
}

type runWork struct {
	Tasks            []Task                        `json:"tasks"`
	Next             int                           `json:"next"`
	SequenceAccounts []string                      `json:"sequence_accounts"`
	SequenceAccount  int                           `json:"sequence_account,omitempty"`
	SequenceIndex    int                           `json:"sequence_index,omitempty"`
	SequenceCount    int                           `json:"sequence_count,omitempty"`
	SequenceHolds    map[string]sequenceCheckpoint `json:"sequence_holds,omitempty"`
	SequenceDone     map[string]bool               `json:"sequence_done,omitempty"`
	Status           Status                        `json:"status"`
	ErrorCode        string                        `json:"error_code,omitempty"`
	Confirmed        map[string]bool               `json:"confirmed,omitempty"`
	CancelRequested  bool                          `json:"cancel_requested,omitempty"`
}

type sequenceCheckpoint struct {
	Tasks     []Task          `json:"tasks"`
	Next      int             `json:"next"`
	Index     int             `json:"index"`
	Confirmed map[string]bool `json:"confirmed,omitempty"`
}

func cloneWork(work runWork) runWork {
	raw, _ := json.Marshal(work)
	var copy runWork
	_ = json.Unmarshal(raw, &copy)
	return copy
}

// errStoreCorrupt marks bytes that are not valid JSON at all. Only this class
// is safe to isolate: no semantic check ever ran, so there is no receipt to
// trust or distrust. Every other readState error stays fail-closed.
var errStoreCorrupt = errors.New("task_store_corrupt")

func newState() persisted {
	return persisted{Version: 4, Schedules: map[string]Schedule{}, Runs: []Run{}, Completed: map[string]time.Time{}, Uncertain: map[string]Stage{}, PendingTasks: map[string]Task{}, Receipts: map[string]actionReceipt{}, Work: map[string]runWork{}, Attempts: map[string]actionReceipt{}, ScheduleDefaultImports: map[string]bool{}, DeletedSchedules: map[string]bool{}}
}
func readState(path string) (persisted, error) {
	s := newState()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, errors.New("task_store_unreadable")
	}
	if len(b) > 32<<20 || !json.Valid(b) {
		return s, errStoreCorrupt
	}
	if json.Unmarshal(b, &s) != nil || (s.Version < 1 || s.Version > 4) || s.Schedules == nil || s.Completed == nil || s.Uncertain == nil {
		return s, errors.New("task_store_invalid")
	}
	if s.Version == 1 {
		// v1 inferred acceptance from nil errors (F11). Preserve history and
		// uncertainty, but require authoritative inspection instead of trusting
		// its accepted_stage or cached completion markers.
		s.LegacyCompleted = s.Completed
		s.Completed = map[string]time.Time{}
		s.Receipts = map[string]actionReceipt{}
	}
	if s.Version < 4 {
		// Only existing bound receipts carry a recoverable legacy attempt.
		// Unknown v3 operations without one stay unresolved, with no invented ID.
		s.Attempts = map[string]actionReceipt{}
		for key, receipt := range s.Receipts {
			s.Attempts[key] = receipt
		}
	}
	s.Version = 4
	if s.Attempts == nil {
		s.Attempts = map[string]actionReceipt{}
	}
	if s.ScheduleDefaultImports == nil {
		s.ScheduleDefaultImports = map[string]bool{}
	}
	if s.DeletedSchedules == nil {
		s.DeletedSchedules = map[string]bool{}
	}
	if s.Work == nil {
		s.Work = map[string]runWork{}
	}
	if len(s.Work) > 64 {
		return s, errors.New("task_work_limit")
	}
	for id, schedule := range s.Schedules {
		if id != schedule.ID || !validAccountScope(schedule.AccountIDs) {
			return s, errors.New("task_schedule_scope_invalid")
		}
		if !validRunTaskKey(schedule.TaskKey, schedule.TaskSequence) || !validateTaskSequence(schedule.TaskSequence) {
			return s, errors.New("task_schedule_sequence_invalid")
		}
	}
	for id, work := range s.Work {
		if len(work.Tasks) > 1000 || work.Next < 0 || work.Next > len(work.Tasks) {
			return s, errors.New("task_work_invalid")
		}
		var run *Run
		for i := range s.Runs {
			if s.Runs[i].ID == id {
				run = &s.Runs[i]
				break
			}
		}
		if run == nil || run.FinishedAt != nil && !(run.Status == StatusUncertain && len(run.TaskSequence) > 0) {
			return s, errors.New("task_work_orphan")
		}
		if run.FinishedAt == nil && run.Status != StatusQueued && run.Status != StatusRunning || run.FinishedAt != nil && run.Status != StatusUncertain {
			return s, errors.New("task_work_status_invalid")
		}
		if len(run.TaskSequence) > 0 {
			if work.SequenceIndex < 0 || work.SequenceIndex >= len(run.TaskSequence) || work.SequenceCount < len(work.Tasks) || work.SequenceCount > 1000 || work.SequenceAccount < 0 || work.SequenceAccount > len(work.SequenceAccounts) || len(work.SequenceAccounts) > 500 {
				return s, errors.New("task_sequence_cursor_invalid")
			}
			accounts := map[string]bool{}
			for _, account := range work.SequenceAccounts {
				if !IDPattern.MatchString(account) || accounts[account] || len(run.AccountIDs) > 0 && !contains(run.AccountIDs, account) {
					return s, errors.New("task_sequence_scope_invalid")
				}
				accounts[account] = true
			}
			if work.SequenceAccounts != nil && len(run.AccountIDs) > 0 && !sameAccountScope(run.AccountIDs, work.SequenceAccounts) {
				return s, errors.New("task_sequence_scope_invalid")
			}
			if work.SequenceAccount == len(work.SequenceAccounts) && (work.Tasks != nil || work.SequenceIndex != 0 || work.Next != 0) {
				return s, errors.New("task_sequence_cursor_invalid")
			}
			if len(work.SequenceHolds)+len(work.SequenceDone) > len(work.SequenceAccounts) {
				return s, errors.New("task_sequence_scope_invalid")
			}
			totalTasks := len(work.Tasks)
			for account, done := range work.SequenceDone {
				if !done || !accounts[account] {
					return s, errors.New("task_sequence_scope_invalid")
				}
				if work.SequenceAccount < len(work.SequenceAccounts) && work.SequenceAccounts[work.SequenceAccount] == account {
					return s, errors.New("task_sequence_cursor_invalid")
				}
			}
			for account, hold := range work.SequenceHolds {
				if !accounts[account] || work.SequenceDone[account] || hold.Index < 0 || hold.Index >= len(run.TaskSequence) || hold.Next < 0 || hold.Next >= len(hold.Tasks) {
					return s, errors.New("task_sequence_checkpoint_invalid")
				}
				if work.SequenceAccount < len(work.SequenceAccounts) && work.SequenceAccounts[work.SequenceAccount] == account {
					return s, errors.New("task_sequence_checkpoint_invalid")
				}
				totalTasks += len(hold.Tasks)
				if totalTasks > work.SequenceCount {
					return s, errors.New("task_sequence_checkpoint_invalid")
				}
				keys := map[string]bool{}
				for _, task := range hold.Tasks {
					if task.AccountID != account || task.ProviderID != run.ProviderID || task.Key == "" || len(task.Key) > 512 || len(task.PeriodKey) > 512 || !TaskKeyMatches(run.TaskSequence[hold.Index], task.Key) || task.Once && task.OncePerDay || task.OncePerDay && task.PeriodKey == "" || keys[taskKey(task)] {
						return s, errors.New("task_sequence_checkpoint_invalid")
					}
					keys[taskKey(task)] = true
				}
				for key, done := range hold.Confirmed {
					if !done || !keys[key] {
						return s, errors.New("task_work_confirmation_invalid")
					}
				}
			}
		} else if work.SequenceIndex != 0 || work.SequenceCount != 0 || work.SequenceAccount != 0 || work.SequenceAccounts != nil || len(work.SequenceHolds) > 0 || len(work.SequenceDone) > 0 {
			return s, errors.New("task_sequence_cursor_invalid")
		}
		switch work.Status {
		case StatusSkipped, StatusSucceeded, StatusManual, StatusCanceled, StatusFailed, StatusUncertain:
		default:
			return s, errors.New("task_work_status_invalid")
		}
		keys := map[string]bool{}
		for _, t := range work.Tasks {
			if t.ProviderID != run.ProviderID || !IDPattern.MatchString(t.AccountID) || t.Key == "" || len(t.Key) > 512 || len(t.PeriodKey) > 512 || !taskIncluded(run.TaskKey, run.TaskSequence, t.Key) || len(run.TaskSequence) > 0 && (work.SequenceAccount >= len(work.SequenceAccounts) || t.AccountID != work.SequenceAccounts[work.SequenceAccount] || work.SequenceIndex >= len(run.TaskSequence) || !TaskKeyMatches(run.TaskSequence[work.SequenceIndex], t.Key)) || len(run.AccountIDs) > 0 && !contains(run.AccountIDs, t.AccountID) || t.Once && t.OncePerDay || t.OncePerDay && t.PeriodKey == "" || keys[taskKey(t)] {
				return s, errors.New("task_work_invalid")
			}
			keys[taskKey(t)] = true
		}
		for key, done := range work.Confirmed {
			if !done || !keys[key] {
				return s, errors.New("task_work_confirmation_invalid")
			}
		}
	}
	if s.Receipts == nil {
		s.Receipts = map[string]actionReceipt{}
	}
	if s.PendingTasks == nil {
		s.PendingTasks = map[string]Task{}
	}
	if len(s.PendingTasks) > 1000 {
		return s, errors.New("task_store_pending_limit")
	}
	for key, task := range s.PendingTasks {
		if task.AcceptedStage != "" && task.AcceptedStage != StageExecute && task.AcceptedStage != StageClaim {
			return s, errors.New("task_store_receipt_invalid")
		}
		if key != taskKey(task) || !IDPattern.MatchString(task.ProviderID) || !IDPattern.MatchString(task.AccountID) || task.Key == "" || len(task.Key) > 512 || len(task.PeriodKey) > 512 || task.Once && task.OncePerDay || task.OncePerDay && task.PeriodKey == "" {
			return s, errors.New("task_store_pending_invalid")
		}
		task.AcceptedStage = ""
		if receipt, ok := s.Receipts[key]; ok {
			task.AcceptedStage = receiptStage(task, receipt)
			if task.AcceptedStage == "" || s.Uncertain[key] != task.AcceptedStage {
				return s, errors.New("task_store_receipt_invalid")
			}
		}
		s.PendingTasks[key] = task
	}
	for key := range s.Receipts {
		if _, ok := s.PendingTasks[key]; !ok {
			return s, errors.New("task_store_receipt_orphan")
		}
	}
	for key, attempt := range s.Attempts {
		task, ok := s.PendingTasks[key]
		if !ok || receiptStage(task, attempt) == "" || s.Uncertain[key] != attempt.Stage {
			return s, errors.New("task_attempt_invalid")
		}
		if receipt, ok := s.Receipts[key]; ok && receipt != attempt {
			return s, errors.New("task_attempt_receipt_mismatch")
		}
	}
	if err := validateOutput(s); err != nil {
		return s, err
	}
	for i := range s.Runs {
		if !validateTaskSequence(s.Runs[i].TaskSequence) || len(s.Runs[i].TaskSequence) > 0 && !IDPattern.MatchString(s.Runs[i].TaskKey) {
			return s, errors.New("task_run_sequence_invalid")
		}
		if s.Runs[i].Status == StatusRunning || s.Runs[i].Status == StatusQueued {
			if work, ok := s.Work[s.Runs[i].ID]; ok {
				onlyUnknown := len(work.Tasks) > work.Next
				for _, t := range work.Tasks[work.Next:] {
					if _, unknown := s.Uncertain[taskKey(t)]; !unknown {
						onlyUnknown = false
					}
				}
				if onlyUnknown && len(s.Runs[i].TaskSequence) == 0 {
					delete(s.Work, s.Runs[i].ID)
				} else {
					if work.CancelRequested {
						unknown := len(work.SequenceHolds) > 0
						for _, task := range work.Tasks {
							if _, pending := s.Uncertain[taskKey(task)]; pending {
								unknown = true
							}
						}
						s.Runs[i].Status = StatusCanceled
						if len(s.Runs[i].TaskSequence) > 0 && unknown {
							s.Runs[i].Status = StatusUncertain
							s.Runs[i].ErrorCode = "verification_required"
						} else {
							delete(s.Work, s.Runs[i].ID)
						}
						now := time.Now().UTC()
						s.Runs[i].FinishedAt = &now
					} else {
						s.Runs[i].Status = StatusQueued
					}
					continue
				}
			}
			s.Runs[i].Status = StatusUncertain
			s.Runs[i].ErrorCode = "interrupted_restart"
			now := time.Now().UTC()
			s.Runs[i].FinishedAt = &now
		}
	}
	return s, nil
}

// Write the new state before publishing it in memory. A failed write cannot
// silently turn a requested schedule into the active schedule.
func writeState(path string, s persisted) error {
	b, err := json.Marshal(s)
	if err != nil {
		return errors.New("task_store_encode_failed")
	}
	if len(b) > 32<<20 {
		return errors.New("task_store_limit")
	}
	tmp := path + ".pending"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return errors.New("task_store_write_failed")
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return errors.New("task_store_write_failed")
	}
	return nil
}
func cloneState(s persisted) persisted {
	b, _ := json.Marshal(s)
	var copy persisted
	_ = json.Unmarshal(b, &copy)
	if copy.ScheduleDefaultImports == nil {
		copy.ScheduleDefaultImports = map[string]bool{}
	}
	if copy.DeletedSchedules == nil {
		copy.DeletedSchedules = map[string]bool{}
	}
	return copy
}

func openState(path string) (*os.File, persisted, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, persisted{}, errors.New("task_store_directory_failed")
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, persisted{}, errors.New("task_store_lock_failed")
	}
	if err = lockFile(lock); err != nil {
		lock.Close()
		return nil, persisted{}, ErrBusy
	}
	s, err := readState(path)
	if errors.Is(err, errStoreCorrupt) {
		// Syntax-level corruption (unparseable bytes) cannot be trusted for
		// anything, but it also cannot be a forged receipt — every semantic
		// check below never ran. Preserve the bytes and start clean so the
		// gateway still boots. Semantic failures (bad binding, orphan receipt,
		// invalid work) stay fail-closed: those can indicate tampering.
		if isolateErr := isolateCorruptStore(path); isolateErr != nil {
			lock.Close()
			return nil, persisted{}, err
		}
		s, err = newState(), nil
	}
	if err == nil {
		err = writeState(path, s)
	}
	if err != nil {
		lock.Close()
		return nil, persisted{}, err
	}
	return lock, s, nil
}

// isolateCorruptStore moves a damaged state file aside so the engine can start
// with an empty state. One fixed backup slot is used (no unbounded timestamps):
// the first corrupt file is kept for audit, later ones replace that single slot
// only after the previous one was already unusable anyway.
func isolateCorruptStore(path string) error {
	backup := path + ".corrupt"
	_ = os.Remove(backup)
	return os.Rename(path, backup)
}
