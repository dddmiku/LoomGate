package taskproviders

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestTraeDailyPlanProcessesEveryEligibleAccount(t *testing.T) {
	for _, size := range []int{1, 2} {
		t.Run(fmt.Sprintf("accounts_%d", size), func(t *testing.T) {
			p := pool.New("")
			defer p.Close()
			ids := []string{}
			for i := 0; i < size; i++ {
				id := fmt.Sprintf("review-trae-%d", i)
				ids = append(ids, id)
				raw, _ := json.Marshal(map[string]any{"uid": id, "realm": "trae", "domain": "api.trae.cn", "accessToken": "synthetic-only"})
				a, err := auth.Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				p.Add(a)
			}
			v, err := accountvault.Open(filepath.Join(t.TempDir(), "provider-state"))
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			store, err := NewTraeTaskStore(v)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			checked := map[string]bool{}
			calls := map[string]int{}
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
					checked[a.UID] = true
					return trae.CheckinStatus{CheckedIn: true}, nil
				},
			}
			clockBase := time.Date(2099, 1, 3, 4, 0, 0, 0, time.UTC)
			started := time.Now()
			offset := time.Duration(0)
			now := func() time.Time { return clockBase.Add(time.Since(started)).Add(offset) }
			adapter, err := NewAccounts(p, nil, "trae", time.Hour, AccountsOptions{TraeStore: store, TraeClient: api, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			e, err := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "engine.json"), Timeout: 8 * time.Second, PollMin: time.Millisecond, PollMax: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if err = e.Register("trae", adapter); err != nil {
				t.Fatal(err)
			}
			schedule, err := e.SaveSchedule(taskengine.Schedule{ID: "review-daily", ProviderID: "trae", TaskKey: "checkin", AccountIDs: ids, Hours: []int{0}, Minute: 5, Timezone: "Asia/Shanghai", OncePerDay: true, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			due := *schedule.NextRun
			if err = e.Tick(due); err != nil {
				t.Fatal(err)
			}
			runs := e.Runs()
			if len(runs) != 1 {
				t.Fatal("first tick did not dispatch")
			}
			run := waitModuleRun(t, e, runs[0].ID)
			// This is a subsequent scheduler tick after the local gap expired,
			// not a second manual submission or a retry of an unknown write.
			offset = 4 * time.Second
			if err = e.Tick(due.Add(4 * time.Second)); err != nil {
				t.Fatal(err)
			}
			for _, r := range e.Runs() {
				if r.FinishedAt == nil {
					waitModuleRun(t, e, r.ID)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			total := 0
			for _, n := range calls {
				total += n
			}
			next := e.Schedules()[0].NextRun
			t.Logf("eligible_accounts=%d actual_claims=%d first_run_status=%s runs_after_gap=%d next_plan_delay_hours=%.2f", size, total, run.Status, len(e.Runs()), next.Sub(due).Hours())
			for _, id := range ids {
				if calls[id] != 1 {
					t.Errorf("eligible account %s received %d claims; daily plan discarded it instead of pacing or preserving deferred work", id, calls[id])
				}
			}
		})
	}
}

func TestTraeMalformedNestedPackPreservesKnownExpiry(t *testing.T) {
	for _, mode := range []string{"valid", "valid_empty_usage", "missing_quota", "wrong_quota_type"} {
		t.Run(mode, func(t *testing.T) {
			p := pool.New("")
			defer p.Close()
			account, err := auth.Parse([]byte(`{"uid":"review-trae-balance","realm":"trae","domain":"api.trae.cn","accessToken":"synthetic-only"}`))
			if err != nil {
				t.Fatal(err)
			}
			p.Add(account)
			p.SetCreditsDetailed(account.UID, 800, 400)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				r.Body.Close()
				quota := map[string]any{"credits_limit": 500}
				usage := map[string]any{"credits_amount": 100}
				if mode == "missing_quota" {
					quota = map[string]any{}
				}
				if mode == "wrong_quota_type" {
					quota["credits_limit"] = map[string]any{"changed_schema": 500}
				}
				if mode == "valid_empty_usage" {
					usage = map[string]any{}
				}
				data := map[string]any{"code": 0, "usage_summary": map[string]any{"total_amount": 1000, "consumed_amount": 200}, "user_entitlement_pack_list": []any{map[string]any{"entitlement_base_info": map[string]any{"end_time": time.Now().Add(time.Hour).Unix(), "product_extra": map[string]any{"package_extra": map[string]any{"quota": quota}}}, "usage": usage}}}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(data)
			}))
			defer server.Close()
			adapter, err := NewAccounts(p, nil, "trae", 2*time.Hour, AccountsOptions{TraeClient: &trae.TaskClient{HTTP: server.Client(), Origin: server.URL}})
			if err != nil {
				t.Fatal(err)
			}
			e := traeEngine(t, adapter, filepath.Join(t.TempDir(), "engine.json"))
			defer e.Close()
			r, err := e.Submit(taskengine.Request{ProviderID: "trae", TaskKey: "balance", AccountIDs: []string{account.UID}})
			if err != nil {
				t.Fatal(err)
			}
			run := waitModuleRun(t, e, r.ID)
			status, _ := p.Status(account.UID)
			t.Logf("mode=%s run_status=%s pool_credits=%d pool_expiring=%d", mode, run.Status, status.Credits, status.CreditsExpiring)
			if mode == "valid" || mode == "valid_empty_usage" {
				want := int64(400)
				if mode == "valid_empty_usage" {
					want = 500
				}
				if run.Status != taskengine.StatusSucceeded || status.Credits != 800 || status.CreditsExpiring != want {
					t.Fatal("valid pack control failed")
				}
			} else {
				if run.Status == taskengine.StatusSucceeded {
					t.Error("unparseable nested quota was reported as a valid balance refresh")
				}
				if status.CreditsExpiring != 400 {
					t.Errorf("unknown quota replaced known expiry=400 with %d", status.CreditsExpiring)
				}
			}
		})
	}
}
