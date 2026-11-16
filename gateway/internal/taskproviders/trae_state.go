package taskproviders

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
	"workbuddy2api/internal/accountvault"
)

type traeTaskState struct {
	CheckedDay string    `json:"checked_day,omitempty"`
	RetryAt    time.Time `json:"retry_at,omitempty"`
	Failures   int       `json:"failures,omitempty"`
	Code       int       `json:"code,omitempty"`
}
type traeTaskStateData struct {
	Accounts  map[string]traeTaskState `json:"accounts"`
	NextClaim time.Time                `json:"next_claim"`
}
type TraeTaskStore struct {
	mu        sync.Mutex
	vault     *accountvault.Vault
	state     traeTaskStateData
	revision  uint64
	claimGate chan struct{}
}

func NewTraeTaskStore(v *accountvault.Vault) (*TraeTaskStore, error) {
	s := &TraeTaskStore{vault: v, state: traeTaskStateData{Accounts: map[string]traeTaskState{}}, claimGate: make(chan struct{}, 1)}
	if v == nil {
		return s, nil
	}
	rows, err := v.List()
	if err != nil {
		return nil, err
	}
	for _, a := range rows {
		if a.ID != "trae-task-state" {
			continue
		}
		if a.Provider != "trae" {
			return nil, errors.New("trae_task_state_invalid")
		}
		_, raw, err := v.Get(a.ID)
		if err != nil {
			return nil, err
		}
		defer func() {
			for i := range raw {
				raw[i] = 0
			}
		}()
		if json.Unmarshal(raw, &s.state) != nil || s.state.Accounts == nil || len(s.state.Accounts) > 1000 {
			return nil, errors.New("trae_task_state_invalid")
		}
		s.revision = a.Revision
	}
	for key, a := range s.state.Accounts {
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(key) {
			return nil, errors.New("trae_task_state_invalid")
		}
		if a.CheckedDay != "" {
			if _, err := time.Parse("2006-01-02", a.CheckedDay); err != nil {
				return nil, errors.New("trae_task_state_invalid")
			}
		}
		if a.Failures < 0 || a.Failures > 1000 || a.CheckedDay != "" && len(a.CheckedDay) != 10 {
			return nil, errors.New("trae_task_state_invalid")
		}
	}
	return s, nil
}
func traeStateKey(id string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(id))) }
func (s *TraeTaskStore) read(id string) traeTaskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Accounts[traeStateKey(id)]
}
func (s *TraeTaskStore) change(id string, fn func(*traeTaskState, *traeTaskStateData) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := traeStateKey(id)
	next := traeTaskStateData{Accounts: map[string]traeTaskState{}, NextClaim: s.state.NextClaim}
	for k, v := range s.state.Accounts {
		next.Accounts[k] = v
	}
	item := next.Accounts[key]
	if err := fn(&item, &next); err != nil {
		return err
	}
	next.Accounts[key] = item
	if len(next.Accounts) > 1000 {
		return errors.New("trae_task_state_limit")
	}
	if s.vault != nil {
		raw, _ := json.Marshal(next)
		defer func() {
			for i := range raw {
				raw[i] = 0
			}
		}()
		a, err := s.vault.Put(accountvault.Account{ID: "trae-task-state", Provider: "trae", Enabled: true}, raw, s.revision)
		if err != nil {
			return err
		}
		s.revision = a.Revision
	}
	s.state = next
	return nil
}
