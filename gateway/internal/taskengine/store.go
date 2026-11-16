package taskengine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type persisted struct {
	Version         int                      `json:"version"`
	Schedules       map[string]Schedule      `json:"schedules"`
	Runs            []Run                    `json:"runs"`
	Completed       map[string]time.Time     `json:"completed"`
	Uncertain       map[string]Stage         `json:"uncertain"`
	PendingTasks    map[string]Task          `json:"pending_tasks,omitempty"`
	Receipts        map[string]actionReceipt `json:"receipts,omitempty"`
	LegacyCompleted map[string]time.Time     `json:"legacy_completed,omitempty"`
	Work            map[string]runWork       `json:"work,omitempty"`
}

type runWork struct {
	Tasks           []Task          `json:"tasks"`
	Next            int             `json:"next"`
	Status          Status          `json:"status"`
	ErrorCode       string          `json:"error_code,omitempty"`
	Confirmed       map[string]bool `json:"confirmed,omitempty"`
	CancelRequested bool            `json:"cancel_requested,omitempty"`
}

func newState() persisted {
	return persisted{Version: 3, Schedules: map[string]Schedule{}, Runs: []Run{}, Completed: map[string]time.Time{}, Uncertain: map[string]Stage{}, PendingTasks: map[string]Task{}, Receipts: map[string]actionReceipt{}, Work: map[string]runWork{}}
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
	if len(b) > 32<<20 || json.Unmarshal(b, &s) != nil || (s.Version != 1 && s.Version != 2 && s.Version != 3) || s.Schedules == nil || s.Completed == nil || s.Uncertain == nil {
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
	s.Version = 3
	if s.Work == nil {
		s.Work = map[string]runWork{}
	}
	if len(s.Work) > 64 {
		return s, errors.New("task_work_limit")
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
		if run == nil || run.FinishedAt != nil {
			return s, errors.New("task_work_orphan")
		}
		if run.Status != StatusQueued && run.Status != StatusRunning {
			return s, errors.New("task_work_status_invalid")
		}
		switch work.Status {
		case StatusSkipped, StatusSucceeded, StatusManual, StatusCanceled, StatusFailed, StatusUncertain:
		default:
			return s, errors.New("task_work_status_invalid")
		}
		keys := map[string]bool{}
		for _, t := range work.Tasks {
			if t.ProviderID != run.ProviderID || !IDPattern.MatchString(t.AccountID) || t.Key == "" || len(t.Key) > 512 || len(t.PeriodKey) > 512 || len(run.AccountIDs) > 0 && !contains(run.AccountIDs, t.AccountID) || t.Once && t.OncePerDay || t.OncePerDay && t.PeriodKey == "" || keys[taskKey(t)] {
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
	for i := range s.Runs {
		if s.Runs[i].Status == StatusRunning || s.Runs[i].Status == StatusQueued {
			if work, ok := s.Work[s.Runs[i].ID]; ok {
				onlyUnknown := len(work.Tasks) > work.Next
				for _, t := range work.Tasks[work.Next:] {
					if _, unknown := s.Uncertain[taskKey(t)]; !unknown {
						onlyUnknown = false
					}
				}
				if onlyUnknown {
					delete(s.Work, s.Runs[i].ID)
				} else {
					if work.CancelRequested {
						s.Runs[i].Status = StatusCanceled
						now := time.Now().UTC()
						s.Runs[i].FinishedAt = &now
						delete(s.Work, s.Runs[i].ID)
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
	if err == nil {
		err = writeState(path, s)
	}
	if err != nil {
		lock.Close()
		return nil, persisted{}, err
	}
	return lock, s, nil
}
