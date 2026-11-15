package taskengine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
	"workbuddy2api/internal/taskengine"
)

type discoveryAdapter struct {
	behaviorAdapter
	catalog taskengine.Catalog
}

func (d *discoveryAdapter) DiscoverCatalog(context.Context, taskengine.Scope, string) (taskengine.Catalog, error) {
	return d.catalog, nil
}
func TestPartialDiscoveryHTTPExecutionAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e := openBehaviorEngine(t, taskengine.Options{Path: path})
	task := syntheticTask("synthetic", "account-a", "read-only")
	task.SideEffect = false
	a := &discoveryAdapter{behaviorAdapter: behaviorAdapter{inspect: func(context.Context, taskengine.Task) (taskengine.Observation, error) { return phase("done"), nil }}, catalog: taskengine.Catalog{Tasks: []taskengine.Task{task}, Issues: []taskengine.DiscoveryIssue{{ProviderID: "synthetic", AccountID: "account-b", ModuleID: "failed-module", ErrorCode: "module_discovery_failed"}}}}
	if err := e.Register("synthetic", a); err != nil {
		t.Fatal(err)
	}
	if tasks, err := e.Catalog(context.Background(), "synthetic", taskengine.Scope{}); err == nil || len(tasks) != 1 {
		t.Fatal("legacy catalog accessor hid partial discovery")
	}
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/workbench/catalog?provider=synthetic", nil))
	var c taskengine.Catalog
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &c) != nil || len(c.Tasks) != 1 || len(c.Issues) != 1 {
		t.Fatal("HTTP partial result lost")
	}
	r, err := e.Submit(taskengine.Request{ProviderID: "synthetic", TaskKey: "*"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs := e.Runs()
		if len(runs) == 1 && runs[0].FinishedAt != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	runs := e.Runs()
	if len(runs) != 1 || runs[0].Status != taskengine.StatusFailed || len(runs[0].Steps) != 1 || len(runs[0].DiscoveryIssues) != 1 {
		t.Fatal("partial run discarded available task or reported success")
	}
	e.Close()
	restored, err := taskengine.Open(taskengine.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	rows := restored.Runs()
	if len(rows) != 1 || rows[0].ID != r.ID || rows[0].ErrorCode != "partial_discovery_failed" || len(rows[0].DiscoveryIssues) != 1 {
		t.Fatal("partial failure lost after restart")
	}
}
func TestDiscoveryIssueCannotEscapeAccountScopeOrExposeRawError(t *testing.T) {
	for _, code := range []string{"module_discovery_failed", "synthetic-private-raw-error"} {
		e := openBehaviorEngine(t, taskengine.Options{})
		a := &discoveryAdapter{catalog: taskengine.Catalog{Issues: []taskengine.DiscoveryIssue{{ProviderID: "synthetic", AccountID: "foreign", ModuleID: "module", ErrorCode: code}}}}
		e.Register("synthetic", a)
		if _, err := e.CatalogDetails(context.Background(), "synthetic", taskengine.Scope{AccountIDs: []string{"allowed"}}, ""); err == nil {
			t.Fatal("foreign account issue admitted")
		}
		if code != "module_discovery_failed" {
			if _, err := e.CatalogDetails(context.Background(), "synthetic", taskengine.Scope{}, ""); err == nil {
				t.Fatal("raw error admitted")
			}
		}
	}
}
