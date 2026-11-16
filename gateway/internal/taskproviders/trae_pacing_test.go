package taskproviders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream/trae"
)

func TestTraePacedWorkSurvivesTimeoutBudgetRestartAndCancellation(t *testing.T) {
	for _, mode := range []string{"group_exceeds_task_timeout", "restart_after_unknown", "cancel_during_gap", "disabled_during_gap", "day_changes_during_gap"} {
		t.Run(mode, func(t *testing.T) {
			p := pool.New("")
			defer p.Close()
			ids := []string{"pace-first", "pace-second"}
			for _, id := range ids {
				raw, _ := json.Marshal(map[string]any{"uid": id, "realm": "trae", "accessToken": "synthetic-only", "domain": "api.trae.cn"})
				a, _ := auth.Parse(raw)
				p.Add(a)
			}
			dir := t.TempDir()
			vault, err := accountvault.Open(filepath.Join(dir, "vault"))
			if err != nil {
				t.Fatal(err)
			}
			defer vault.Close()
			store, err := NewTraeTaskStore(vault)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			calls := map[string]int{}
			times := []time.Time{}
			checked := map[string]bool{}
			offset := time.Duration(0)
			now := func() time.Time { mu.Lock(); defer mu.Unlock(); return time.Now().Add(offset) }
			first := make(chan struct{})
			api := &traeModuleStub{
				status: func(_ context.Context, a *auth.Auth) (trae.CheckinStatus, error) {
					mu.Lock()
					defer mu.Unlock()
					return trae.CheckinStatus{Enabled: true, CheckedIn: checked[a.UID]}, nil
				},
				claim: func(_ context.Context, a *auth.Auth) (trae.CheckinStatus, error) {
					mu.Lock()
					defer mu.Unlock()
					calls[a.UID]++
					times = append(times, time.Now())
					if len(times) == 1 {
						close(first)
					}
					if mode == "restart_after_unknown" && a.UID == ids[0] {
						return trae.CheckinStatus{}, errors.New("synthetic_lost_response")
					}
					checked[a.UID] = true
					return trae.CheckinStatus{CheckedIn: true}, nil
				},
			}
			adapter, err := NewAccounts(p, nil, "trae", time.Hour, AccountsOptions{TraeStore: store, TraeClient: api, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "engine.json")
			open := func() *taskengine.Engine {
				e, err := taskengine.Open(taskengine.Options{Path: path, Timeout: 200 * time.Millisecond, PollMin: time.Millisecond, PollMax: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				if err = e.Register("trae", adapter); err != nil {
					t.Fatal(err)
				}
				return e
			}
			e := open()
			defer func() { e.Close() }()
			s, err := e.SaveSchedule(taskengine.Schedule{ID: "paced", ProviderID: "trae", TaskKey: "checkin", AccountIDs: ids, Hours: []int{0}, Minute: 5, Timezone: "Asia/Shanghai", OncePerDay: true, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Tick(*s.NextRun); err != nil {
				t.Fatal(err)
			}
			id := e.Runs()[0].ID
			select {
			case <-first:
			case <-time.After(3 * time.Second):
				t.Fatal("first claim absent")
			}
			// Wait for the first result and second-account pacing, using observable steps.
			deadline := time.Now().Add(time.Second)
			for {
				r := e.Runs()[0]
				found := false
				for _, step := range r.Steps {
					if step.Stage == taskengine.StageVerify || mode == "restart_after_unknown" && step.ActionOutcome != "" {
						found = true
					}
				}
				if found {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("action not recorded")
				}
				time.Sleep(time.Millisecond)
			}
			switch mode {
			case "restart_after_unknown":
				e.Close()
				vault.Close()
				vault, err = accountvault.Open(filepath.Join(dir, "vault"))
				if err != nil {
					t.Fatal(err)
				}
				defer vault.Close()
				store, err = NewTraeTaskStore(vault)
				if err != nil {
					t.Fatal(err)
				}
				adapter.Modules[0].(*traeCheckinModule).store = store
				e = open()
			case "cancel_during_gap":
				if !e.Cancel(id) {
					t.Fatal("cancel rejected")
				}
			case "disabled_during_gap":
				p.Disable(ids[1], "manual")
			case "day_changes_during_gap":
				mu.Lock()
				offset = 24 * time.Hour
				mu.Unlock()
			}
			run := waitModuleRun(t, e, id)
			mu.Lock()
			defer mu.Unlock()
			if calls[ids[0]] != 1 {
				t.Fatal("first mutation replayed")
			}
			want := 1
			if mode == "cancel_during_gap" || mode == "disabled_during_gap" || mode == "day_changes_during_gap" {
				want = 0
			}
			if calls[ids[1]] != want {
				t.Fatalf("second account claims=%d want=%d", calls[ids[1]], want)
			}
			if len(times) == 2 && times[1].Sub(times[0]) < 2900*time.Millisecond {
				t.Fatal("global pacing removed")
			}
			if mode == "group_exceeds_task_timeout" && run.Status != taskengine.StatusSucceeded {
				t.Fatal("group timeout dropped pending account")
			}
			if mode == "restart_after_unknown" && run.Status != taskengine.StatusUncertain {
				t.Fatal("unknown prior claim falsely resolved")
			}
			if mode == "cancel_during_gap" && run.Status != taskengine.StatusCanceled {
				t.Fatal("cancel not preserved")
			}
			if len(e.Runs()) != 1 {
				t.Fatal(fmt.Sprintf("daily plan duplicated runs: %d", len(e.Runs())))
			}
		})
	}
}
