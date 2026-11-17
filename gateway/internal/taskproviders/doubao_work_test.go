package taskproviders

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/doubao"
	"workbuddy2api/internal/taskengine"
)

type workFixture struct {
	mu                        sync.Mutex
	status, generated, claims int
	withhold, fail            bool
	records                   []doubao.RewardRecord
}

func (f *workFixture) Catalog(context.Context, doubao.RuntimeBinding, string) (json.RawMessage, error) {
	return nil, errors.New("unused_catalog")
}
func (f *workFixture) snapshot() doubao.Activity {
	return doubao.Activity{Group: "synthetic-group", Tasks: []doubao.Task{{Code: "synthetic-sheet", Group: "synthetic-group", ID: "synthetic-instance", Type: "GenExcel", Work: &doubao.WorkCapability{Available: true, Reason: "available"}, Kind: "chat_activity", Title: "Synthetic workbook", Status: f.status, RewardMode: 1, Rewards: []doubao.Reward{{Amount: 100, Type: "quota_grant"}}}}, Milestones: []doubao.Task{}}
}
func (f *workFixture) Activities(context.Context, doubao.RuntimeBinding) (doubao.Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot(), nil
}
func (f *workFixture) TaskAction(_ context.Context, _ doubao.RuntimeBinding, op, group, task, instance string) (doubao.Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if op != "task_claim" || group != "synthetic-group" || task != "synthetic-sheet" || instance != "synthetic-instance" || f.status != 2 {
		return doubao.Activity{}, errors.New("wrong_action")
	}
	f.claims++
	f.status = 3
	if !f.withhold {
		f.records = append(f.records, doubao.RewardRecord{ID: "new-record", TaskCode: task, Amount: "100", Unit: "points", CreatedMS: time.Now().UnixMilli()})
	}
	a := f.snapshot()
	a.OperationStatus = "submitted"
	return a, nil
}
func (f *workFixture) RewardRecords(context.Context, doubao.RuntimeBinding) (doubao.RewardPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return doubao.RewardPage{Records: append([]doubao.RewardRecord{}, f.records...)}, nil
}
func (f *workFixture) Models(context.Context) ([]inference.Model, error) { return nil, nil }
func (f *workFixture) Complete(context.Context, string, string) (inference.Result, error) {
	return inference.Result{}, errors.New("wrong_entry")
}
func (f *workFixture) CompleteTask(_ context.Context, q doubao.TaskWorkRequest) (inference.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q.AccountID != "synthetic-account" || q.Revision != "1" || q.Instruction == "" || q.Instruction != "Synthetic workbook requirement" {
		return inference.Result{}, errors.New("account_or_input_mismatch")
	}
	f.generated++
	if f.fail {
		return inference.Result{Attempted: true}, errors.New("synthetic_unknown")
	}
	f.status = 2
	return inference.Result{Text: "Synthetic output", Attempted: true, Acknowledged: true, Complete: true, ConversationRef: "dialog-synthetic"}, nil
}
func workAdapter(t *testing.T) (*Doubao, *workFixture) {
	t.Helper()
	v, err := accountvault.Open(filepath.Join(t.TempDir(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	state, err := accountvault.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	raw, _ := json.Marshal(doubao.StoredProfile{Runtime: &doubao.RuntimeBinding{Identity: strings.Repeat("a", 64)}, BotID: "synthetic-bot"})
	if _, err = v.Put(accountvault.Account{ID: "synthetic-account", Provider: "doubao", Enabled: true}, raw, 0); err != nil {
		t.Fatal(err)
	}
	f := &workFixture{status: 1, records: []doubao.RewardRecord{{ID: "old-record", TaskCode: "synthetic-sheet", Amount: "100", Unit: "points", CreatedMS: time.Now().Add(-time.Hour).UnixMilli()}}}
	a := &Doubao{Vault: v, Client: doubao.NewWithRuntime(doubao.VaultSessions{Vault: v}, f), Work: &DoubaoWork{Vault: state, Provider: f}}
	input, _ := json.Marshal(workInput{Account: "synthetic-account", Task: "synthetic-group:synthetic-sheet", Model: "doubao:synthetic:work", Instruction: "Synthetic workbook requirement", Enabled: true})
	if _, err = state.Put(accountvault.Account{ID: workKey("dw-input-", "synthetic-account", "synthetic-group:synthetic-sheet"), Provider: "doubao-work-input", Enabled: true}, input, 0); err != nil {
		t.Fatal(err)
	}
	return a, f
}
func workEngine(t *testing.T, a *Doubao, path string) *taskengine.Engine {
	t.Helper()
	e, err := taskengine.Open(taskengine.Options{Path: path, Timeout: 180 * time.Millisecond, PollMin: time.Millisecond, PollMax: 3 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Register("doubao", a); err != nil {
		t.Fatal(err)
	}
	return e
}
func submitWork(t *testing.T, e *taskengine.Engine) taskengine.Run {
	t.Helper()
	r, err := e.Submit(taskengine.Request{ProviderID: "doubao", TaskKey: "synthetic-group:synthetic-sheet", AccountIDs: []string{"synthetic-account"}})
	if err != nil {
		t.Fatal(err)
	}
	return waitModuleRun(t, e, r.ID)
}
func TestDoubaoWorkActualGenerateClaimAndNewRewardRecord(t *testing.T) {
	a, f := workAdapter(t)
	e := workEngine(t, a, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	if run := submitWork(t, e); run.Status != taskengine.StatusSucceeded {
		t.Fatalf("work result %s", run.Status)
	}
	f.mu.Lock()
	if f.generated != 1 || f.claims != 1 {
		t.Fatal("missing generation or claim")
	}
	f.mu.Unlock()
	if run := submitWork(t, e); run.Status != taskengine.StatusSkipped {
		t.Fatalf("completed work replayed %s", run.Status)
	}
}
func TestDoubaoRewardStatusAndOldRecordCannotConfirmNewClaimAcrossRestart(t *testing.T) {
	a, f := workAdapter(t)
	f.withhold = true
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := workEngine(t, a, path)
	if run := submitWork(t, e); run.Status != taskengine.StatusUncertain {
		t.Fatalf("missing reward was reported %s", run.Status)
	}
	e.Close()
	e = workEngine(t, a, path)
	defer e.Close()
	catalog, err := e.CatalogDetails(context.Background(), "doubao", taskengine.Scope{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Tasks) != 1 || catalog.Tasks[0].ManualOnly || !catalog.Tasks[0].PendingVerification {
		t.Fatal("pending operation lost read-only verification action")
	}
	if run := submitWork(t, e); run.Status != taskengine.StatusUncertain {
		t.Fatalf("restart cleared reward guard %s", run.Status)
	}
	f.mu.Lock()
	if f.generated != 1 || f.claims != 1 {
		t.Fatal("restart replayed operation")
	}
	f.records = append(f.records, doubao.RewardRecord{ID: "later-authoritative-record", TaskCode: "synthetic-sheet", Amount: "100", Unit: "points", CreatedMS: time.Now().UnixMilli()})
	f.mu.Unlock()
	if run := submitWork(t, e); run.Status != taskengine.StatusSucceeded {
		t.Fatalf("authoritative new record not reconciled %s", run.Status)
	}
}
func TestDoubaoUnknownGenerationIsNotReplayedOrMistakenForReward(t *testing.T) {
	a, f := workAdapter(t)
	f.fail = true
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := workEngine(t, a, path)
	if run := submitWork(t, e); run.Status != taskengine.StatusUncertain {
		t.Fatal("unknown generation lost")
	}
	e.Close()
	e = workEngine(t, a, path)
	defer e.Close()
	if run := submitWork(t, e); run.Status != taskengine.StatusUncertain {
		t.Fatal("unknown cleared")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.generated != 1 || f.claims != 0 {
		t.Fatal("unknown replayed or claimed")
	}
}
