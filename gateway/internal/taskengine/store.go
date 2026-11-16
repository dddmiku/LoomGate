package taskengine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type persisted struct {
	Version      int                  `json:"version"`
	Schedules    map[string]Schedule  `json:"schedules"`
	Runs         []Run                `json:"runs"`
	Completed    map[string]time.Time `json:"completed"`
	Uncertain    map[string]Stage     `json:"uncertain"`
	PendingTasks map[string]Task      `json:"pending_tasks,omitempty"`
}

func newState() persisted {
	return persisted{Version: 1, Schedules: map[string]Schedule{}, Runs: []Run{}, Completed: map[string]time.Time{}, Uncertain: map[string]Stage{}, PendingTasks: map[string]Task{}}
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
	if len(b) > 32<<20 || json.Unmarshal(b, &s) != nil || s.Version != 1 || s.Schedules == nil || s.Completed == nil || s.Uncertain == nil {
		return s, errors.New("task_store_invalid")
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
	}
	for i := range s.Runs {
		if s.Runs[i].Status == StatusRunning || s.Runs[i].Status == StatusQueued {
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
