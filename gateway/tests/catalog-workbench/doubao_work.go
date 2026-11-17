package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/doubao"
	"workbuddy2api/internal/taskproviders"
)

type doubaoWorkState struct {
	Status            int `json:"status"`
	Generated, Claims int
	Records           []doubao.RewardRecord
}
type doubaoWorkFixture struct {
	mu       sync.Mutex
	path     string
	state    doubaoWorkState
	withhold bool
}

func (f *doubaoWorkFixture) save() error {
	b, _ := json.Marshal(f.state)
	return os.WriteFile(f.path, b, 0600)
}
func (f *doubaoWorkFixture) Catalog(context.Context, doubao.RuntimeBinding, string) (json.RawMessage, error) {
	return json.RawMessage(`{"entry_list":[{"active_switch_conf":{"menu_conf_v2":{"use_mode_model_list":true,"mode_list":{"item_list":[{"mode_id":"synthetic-work","name":"合成工作模式","model_list":{"item_list":[{"item_id":1,"model_item_key":"synthetic","name":"合成工作模型","show_task_tools":1,"reasoning_effort_config":{"default_level":1}}]}}]}}}}]}`), nil
}
func (f *doubaoWorkFixture) snapshot() doubao.Activity {
	return doubao.Activity{Group: "synthetic-work-group", Tasks: []doubao.Task{{Code: "synthetic-work-task", Group: "synthetic-work-group", ID: "synthetic-work-instance", Type: "GenExcel", Work: &doubao.WorkCapability{Available: true, Reason: "available"}, Kind: "chat_activity", Title: "合成验收工作簿", Description: "合成上游，仅用于面板与任务链验收。", Status: f.state.Status, RewardMode: 1, Rewards: []doubao.Reward{{Amount: 100, Type: "quota_grant"}}}}, Milestones: []doubao.Task{}}
}
func (f *doubaoWorkFixture) Activities(context.Context, doubao.RuntimeBinding) (doubao.Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot(), nil
}
func (f *doubaoWorkFixture) TaskAction(_ context.Context, _ doubao.RuntimeBinding, op, group, key, instance string) (doubao.Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if op != "task_claim" || group != "synthetic-work-group" || key != "synthetic-work-task" || instance != "synthetic-work-instance" || f.state.Status != 2 {
		return doubao.Activity{}, errors.New("synthetic_action_rejected")
	}
	f.state.Claims++
	f.state.Status = 3
	if !f.withhold {
		f.state.Records = append(f.state.Records, doubao.RewardRecord{ID: fmt.Sprintf("synthetic-record-%d", f.state.Claims), TaskCode: key, Amount: "100", Unit: "points", CreatedMS: time.Now().UnixMilli()})
	}
	if err := f.save(); err != nil {
		return doubao.Activity{}, err
	}
	out := f.snapshot()
	out.OperationStatus = "submitted"
	return out, nil
}
func (f *doubaoWorkFixture) RewardRecords(context.Context, doubao.RuntimeBinding) (doubao.RewardPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return doubao.RewardPage{Records: append([]doubao.RewardRecord{}, f.state.Records...)}, nil
}
func (f *doubaoWorkFixture) Models(context.Context) ([]inference.Model, error) {
	return []inference.Model{{ID: "doubao:synthetic-work:synthetic"}}, nil
}
func (f *doubaoWorkFixture) Complete(context.Context, string, string) (inference.Result, error) {
	return inference.Result{}, errors.New("synthetic_wrong_entry")
}
func (f *doubaoWorkFixture) CompleteTask(ctx context.Context, q doubao.TaskWorkRequest) (inference.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q.AccountID != "a-fixture" || q.Instruction == "" {
		return inference.Result{}, errors.New("synthetic_scope_mismatch")
	}
	if ctx.Err() != nil {
		return inference.Result{}, ctx.Err()
	}
	f.state.Generated++
	f.state.Status = 2
	if err := f.save(); err != nil {
		return inference.Result{Attempted: true}, err
	}
	return inference.Result{Text: "合成工作已完成", Attempted: true, Complete: true, Acknowledged: true, ConversationRef: "dialog-synthetic-work"}, nil
}
func newDoubaoWorkFixture(root string, vault *accountvault.Vault) (*taskproviders.Doubao, *doubaoWorkFixture, *accountvault.Vault, error) {
	state, err := accountvault.Open(filepath.Join(root, "data", "synthetic-work-state"))
	if err != nil {
		return nil, nil, nil, err
	}
	f := &doubaoWorkFixture{path: filepath.Join(root, "data", "synthetic-work-upstream.json"), state: doubaoWorkState{Status: 1, Records: []doubao.RewardRecord{}}}
	if raw, e := os.ReadFile(f.path); e == nil {
		if json.Unmarshal(raw, &f.state) != nil {
			state.Close()
			return nil, nil, nil, errors.New("synthetic_state_invalid")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		state.Close()
		return nil, nil, nil, e
	}
	rows, err := vault.List()
	if err != nil {
		state.Close()
		return nil, nil, nil, err
	}
	for _, a := range rows {
		if a.ID != "a-fixture" {
			continue
		}
		_, plain, e := vault.Get(a.ID)
		if e != nil {
			return nil, nil, nil, e
		}
		var old doubao.StoredProfile
		json.Unmarshal(plain, &old)
		if old.Runtime == nil {
			raw, _ := json.Marshal(doubao.StoredProfile{Runtime: &doubao.RuntimeBinding{Identity: strings.Repeat("a", 64)}, BotID: "synthetic-bot"})
			if _, e = vault.Put(a, raw, a.Revision); e != nil {
				return nil, nil, nil, e
			}
		}
	}
	client := doubao.NewWithRuntime(doubao.VaultSessions{Vault: vault}, f)
	// Synthetic task content is seeded internally; no general editor endpoint.
	inputID := "dw-input-" + fmt.Sprintf("%x", sha256.Sum256([]byte("a-fixture\x00synthetic-work-group:synthetic-work-task")))
	if _, _, e := state.Get(inputID); errors.Is(e, accountvault.ErrNotFound) {
		raw, _ := json.Marshal(map[string]any{"account": "a-fixture", "task": "synthetic-work-group:synthetic-work-task", "model": "doubao:synthetic-work:synthetic", "instruction": "Synthetic task input; no real upstream.", "enabled": true})
		if _, e = state.Put(accountvault.Account{ID: inputID, Provider: "doubao-work-input", Enabled: true}, raw, 0); e != nil {
			return nil, nil, nil, e
		}
	}
	return &taskproviders.Doubao{Vault: vault, Client: client, Work: &taskproviders.DoubaoWork{Vault: state, Provider: f}}, f, state, nil
}
