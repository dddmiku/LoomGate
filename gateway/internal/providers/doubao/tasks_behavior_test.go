package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

func activityFixture(group string) map[string]any {
	return map[string]any{
		"campaign_code": group,
		"tasks": []any{map[string]any{"task_id": "synthetic-task-instance", "campaign_code": "dynamic-task-code", "campaign_group_code": group,
			"task_kind": "future-task-kind", "title": "Synthetic task", "subtitle": "Synthetic condition", "task_status": 77, "reward_mode": 2,
			"future_task_metadata": map[string]any{"new_field": true}, "reward_summaries": []any{map[string]any{"reward_amount": 25, "reward_type": "synthetic-quota"}},
			"daily_checkin": map[string]any{"checked_in_today": true, "last_checkin_date": "2099-01-01", "reward_status": 3,
				"reward": map[string]any{"reward_amount": 25, "reward_type": "synthetic-quota"}}}},
		"milestones": []any{map[string]any{"campaign_code": "dynamic-milestone-code", "campaign_group_code": group, "title": "Synthetic milestone", "task_status": 1, "milestone_threshold": 500}},
		"progress":   map[string]any{"total_amount": 200, "future_progress": true}, "future_detail": []int{7, 8},
	}
}

func TestDoubaoActivitiesDiscoverChangingGroupAndRetainFutureTaskKinds(t *testing.T) {
	var launches, details atomic.Int32
	c := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Host != "www.doubao.com" {
			t.Error("wrong official activity request target")
		}
		switch r.URL.Path {
		case "/alice/basic/launch":
			group := fmt.Sprintf("synthetic-dynamic-group-%d", launches.Add(1))
			return fixtureResponse(r, 200, jsonFixture(t, map[string]any{"code": 0, "data": map[string]any{"ug_campaign_info": map[string]any{"campaign_group_code": group, "future_launch_field": true}}})), nil
		case "/alice/ug/campaign/v3/detail/":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error("invalid activity request JSON")
			}
			group := fmt.Sprintf("synthetic-dynamic-group-%d", launches.Load())
			if body["campaign_group_code"] != group {
				t.Error("detail request did not use the group from this launch response")
			}
			details.Add(1)
			return fixtureResponse(r, 200, jsonFixture(t, map[string]any{"code": 0, "data": activityFixture(group)})), nil
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			return fixtureResponse(r, 404, `{}`), nil
		}
	}))
	for i := 1; i <= 2; i++ {
		got, err := c.Activities(context.Background(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		if got.Group != fmt.Sprintf("synthetic-dynamic-group-%d", i) || len(got.Tasks) != 1 || len(got.Milestones) != 1 || got.TotalAmount != 200 {
			t.Fatalf("dynamic activity response lost: %+v", got)
		}
		task := got.Tasks[0]
		if task.ID != "synthetic-task-instance" || task.Kind != "future-task-kind" || task.Status != 77 || task.RewardMode != 2 {
			t.Error("future task kind/status or returned instance identity changed")
		}
		if task.Daily == nil || !task.Daily.Checked || task.Daily.Reward.Amount != 25 || len(task.Rewards) != 1 || got.Milestones[0].Threshold != 500 {
			t.Error("daily or milestone reward metadata was lost")
		}
	}
	if launches.Load() != 2 || details.Load() != 2 {
		t.Error("activity discovery reused a fixed historical group instead of reading current launch")
	}
}

func TestDoubaoActivitiesRejectEmptySchemaAndMismatchedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(map[string]any) any
	}{
		{"empty-schema", func(map[string]any) any { return map[string]any{} }},
		{"progress-only-schema", func(map[string]any) any { return map[string]any{"progress": map[string]any{"total_amount": 0}} }},
		{"null-task-arrays", func(map[string]any) any {
			return map[string]any{"tasks": nil, "milestones": nil, "progress": map[string]any{"total_amount": 0}}
		}},
		{"wrong-top-level-group", func(d map[string]any) any { d["campaign_code"] = "another-group"; return d }},
		{"task-without-code", func(d map[string]any) any { d["tasks"].([]any)[0].(map[string]any)["campaign_code"] = ""; return d }},
		{"task-with-blank-code", func(d map[string]any) any { d["tasks"].([]any)[0].(map[string]any)["campaign_code"] = " \t "; return d }},
		{"wrong-task-group", func(d map[string]any) any {
			d["tasks"].([]any)[0].(map[string]any)["campaign_group_code"] = "another-group"
			return d
		}},
		{"wrong-milestone-group", func(d map[string]any) any {
			d["milestones"].([]any)[0].(map[string]any)["campaign_group_code"] = "another-group"
			return d
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/alice/basic/launch" {
					return fixtureResponse(r, 200, `{"code":0,"data":{"ug_campaign_info":{"campaign_group_code":"synthetic-group"}}}`), nil
				}
				return fixtureResponse(r, 200, jsonFixture(t, map[string]any{"code": 0, "data": tc.alter(activityFixture("synthetic-group"))})), nil
			}))
			got, err := c.Activities(context.Background(), "account-a")
			if err == nil {
				t.Error("invalid activity response accepted as a successful account task list")
			}
			if len(got.Tasks) != 0 || len(got.Milestones) != 0 {
				t.Error("invalid account/group identity exposed usable tasks")
			}
		})
	}
}

func TestDoubaoActivitiesCannotMixSessionRevisionsBetweenLaunchAndDetail(t *testing.T) {
	var sessionReads atomic.Int32
	source := sessionSourceFunc(func(_ context.Context, account string) (Session, error) {
		revision := "revision-a"
		if sessionReads.Add(1) > 1 {
			revision = "revision-b"
		}
		return fixtureSession(account, revision), nil
	})
	var mu sync.Mutex
	var revisions []string
	c := New(source, syntheticTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		revisions = append(revisions, r.Header.Get("X-Test-Revision"))
		mu.Unlock()
		if r.URL.Path == "/alice/basic/launch" {
			return fixtureResponse(r, 200, `{"code":0,"data":{"ug_campaign_info":{"campaign_group_code":"synthetic-group"}}}`), nil
		}
		return fixtureResponse(r, 200, jsonFixture(t, map[string]any{"code": 0, "data": activityFixture("synthetic-group")})), nil
	}))
	_, err := c.Activities(context.Background(), "account-a")
	mu.Lock()
	observed := append([]string(nil), revisions...)
	mu.Unlock()
	if len(observed) == 2 && observed[0] != observed[1] {
		t.Error("launch group from one login revision was requested with another login's session")
	}
	if err == nil && len(observed) != 2 {
		t.Error("activity succeeded without both discovery and detail responses")
	}
}

func TestDoubaoActivityHTTP200BusinessErrorOrMissingCodeIsNotSuccess(t *testing.T) {
	for _, body := range []string{`{"code":19,"data":{}}`, `{"data":{"tasks":[]}}`, `{"code":0}`, `{"code":0,"data":null}`} {
		t.Run(body, func(t *testing.T) {
			c := New(fixedSessions(), syntheticTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/alice/basic/launch" {
					return fixtureResponse(r, 200, `{"code":0,"data":{"ug_campaign_info":{"campaign_group_code":"synthetic-group"}}}`), nil
				}
				return fixtureResponse(r, 200, body), nil
			}))
			if _, err := c.Activities(context.Background(), "account-a"); err == nil {
				t.Error("HTTP 200 with a failed/missing business envelope was accepted")
			}
		})
	}
}
