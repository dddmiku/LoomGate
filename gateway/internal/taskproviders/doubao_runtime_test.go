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
	"workbuddy2api/internal/providers/doubao"
	"workbuddy2api/internal/taskengine"
)

type taskRuntimeFixture struct {
	mu                          sync.Mutex
	checked, claimed, uncertain bool
	date                        string
	total                       int64
	actions                     int
}

func (f *taskRuntimeFixture) Catalog(context.Context, doubao.RuntimeBinding, string) (json.RawMessage, error) {
	return nil, errors.New("unused catalog")
}
func (f *taskRuntimeFixture) snapshot() doubao.Activity {
	return doubao.Activity{Group: "synthetic-group", TotalAmount: f.total, Tasks: []doubao.Task{{Code: "synthetic-signin", Group: "synthetic-group", Kind: "daily_signin", Title: "Synthetic sign in", Status: 1, Daily: &doubao.DailyCheckin{Checked: f.checked, Date: f.date, Reward: doubao.Reward{Amount: 400}}}, {Code: "synthetic-work", Group: "synthetic-group", Kind: "chat_activity", Title: "Synthetic real work", Type: "GenWord", Status: 1}}, Milestones: []doubao.Task{{Code: "synthetic-milestone", Group: "synthetic-group", Kind: "points_milestone", Title: "Synthetic milestone", Status: map[bool]int{false: 1, true: 3}[f.claimed], RewardMode: 2, Threshold: 500}}}
}
func (f *taskRuntimeFixture) Activities(context.Context, doubao.RuntimeBinding) (doubao.Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot(), nil
}
func (f *taskRuntimeFixture) TaskAction(_ context.Context, _ doubao.RuntimeBinding, op, group, task, instance string) (doubao.Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if group != "synthetic-group" {
		return doubao.Activity{}, errors.New("wrong group")
	}
	f.actions++
	if f.uncertain {
		return doubao.Activity{}, errors.New("synthetic unknown submission")
	}
	switch op {
	case "task_checkin":
		f.checked = true
		f.date = "2099-01-02"
		f.total += 400
	case "task_claim":
		if f.total < 500 {
			return doubao.Activity{}, errors.New("threshold not met")
		}
		f.claimed = true
		f.total += 200
	default:
		return doubao.Activity{}, errors.New("unexpected mutation")
	}
	return f.snapshot(), nil
}
func taskRuntimeAdapter(t *testing.T) (*Doubao, *taskRuntimeFixture) {
	t.Helper()
	vault, err := accountvault.Open(filepath.Join(t.TempDir(), "vault"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vault.Close() })
	plain, _ := json.Marshal(doubao.StoredProfile{Runtime: &doubao.RuntimeBinding{Identity: strings.Repeat("a", 64)}, BotID: "synthetic-bot"})
	if _, err = vault.Put(accountvault.Account{ID: "synthetic-account", Provider: "doubao", Enabled: true}, plain, 0); err != nil {
		t.Fatal(err)
	}
	runtime := &taskRuntimeFixture{date: "2099-01-01", total: 200}
	return &Doubao{Vault: vault, Client: doubao.NewWithRuntime(doubao.VaultSessions{Vault: vault}, runtime)}, runtime
}
func TestDoubaoRuntimeTaskEngineVerifyClaimAndDailyReset(t *testing.T) {
	adapter, runtime := taskRuntimeAdapter(t)
	engine, err := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "tasks.json"), PollMin: time.Millisecond, PollMax: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err = engine.Register("doubao", adapter); err != nil {
		t.Fatal(err)
	}
	submit := func(key string) taskengine.Run {
		run, err := engine.Submit(taskengine.Request{ProviderID: "doubao", TaskKey: "synthetic-group:" + key, AccountIDs: []string{"synthetic-account"}})
		if err != nil {
			t.Fatal(err)
		}
		return waitModuleRun(t, engine, run.ID)
	}
	if r := submit("synthetic-signin"); r.Status != taskengine.StatusSucceeded {
		t.Fatal("sign-in not verified")
	}
	if r := submit("synthetic-milestone"); r.Status != taskengine.StatusSucceeded {
		t.Fatal("eligible reward not verified")
	}
	if r := submit("synthetic-work"); r.Status != taskengine.StatusManual {
		t.Fatal("work requirement misreported")
	}
	submit("synthetic-signin")
	submit("synthetic-milestone")
	runtime.mu.Lock()
	if runtime.actions != 2 || runtime.total != 800 {
		t.Fatal("completed action replayed")
	}
	runtime.checked = false
	runtime.mu.Unlock()
	if r := submit("synthetic-signin"); r.Status != taskengine.StatusSucceeded {
		t.Fatal("authoritative new business period skipped")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.actions != 3 {
		t.Fatal("daily reset did not execute exactly once")
	}
}
func TestDoubaoRuntimeUnknownSubmissionIsNotReplayed(t *testing.T) {
	adapter, runtime := taskRuntimeAdapter(t)
	runtime.uncertain = true
	engine, err := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "tasks.json"), PollMin: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.Register("doubao", adapter)
	for i := 0; i < 2; i++ {
		run, err := engine.Submit(taskengine.Request{ProviderID: "doubao", TaskKey: "synthetic-group:synthetic-signin"})
		if err != nil {
			t.Fatal(err)
		}
		if done := waitModuleRun(t, engine, run.ID); done.Status != taskengine.StatusUncertain {
			t.Fatal("unknown submission lost protection")
		}
	}
	catalog, err := engine.CatalogDetails(context.Background(), "doubao", taskengine.Scope{}, "synthetic-group:synthetic-signin")
	if err != nil || len(catalog.Tasks) != 1 || !catalog.Tasks[0].PendingVerification {
		t.Fatal("unconfirmed mutation missing from management catalog")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.actions != 1 {
		t.Fatal("unknown mutation was replayed")
	}
}
