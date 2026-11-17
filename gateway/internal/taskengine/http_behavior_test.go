package taskengine_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

func workbenchHTTP(t *testing.T, e *taskengine.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, r)
	return w
}

func TestWorkbenchHTTPRejectsUnknownAndTrailingJSONBeforeMutation(t *testing.T) {
	cases := []struct{ name, body string }{
		{"unknown-field", `{"provider_id":"fake","unsupported":true}`},
		{"trailing-object", `{"provider_id":"fake"} {"another_request":true}`},
		{"trailing-invalid-token", `{"provider_id":"fake"} not-json`},
		{"body-limit-after-valid-prefix", `{"provider_id":"fake"}` + strings.Repeat(" ", 70<<10)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := openBehaviorEngine(t, taskengine.Options{})
			registerBehavior(t, e, "fake", singleAdapter(syntheticTask("fake", "account-a", "task-a")))
			w := workbenchHTTP(t, e, http.MethodPost, "/workbench/run", tc.body)
			if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("status=%d, malformed/oversized input accepted", w.Code)
			}
			if len(e.Runs()) != 0 {
				t.Error("invalid input submitted a run before validation finished")
			}
		})
	}
}

func TestWorkbenchDoesNotExposeGeneralDoubaoWorkProduct(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	for _, path := range []string{"/workbench/doubao/works", "/workbench/doubao/work-status", "/workbench/doubao/artifact", "/workbench/doubao/work-input"} {
		for _, method := range []string{"GET", "POST"} {
			if response := workbenchHTTP(t, e, method, path, `{}`); response.Code != 404 {
				t.Fatal("general work route remains exposed", response.Code)
			}
		}
	}
	if len(e.Runs()) != 0 {
		t.Fatal("unsupported route dispatched work")
	}
}

func TestWorkbenchHTTPOnlyAcceptsJSONObjectsForCommands(t *testing.T) {
	for _, path := range []string{"/workbench/run", "/workbench/cancel", "/workbench/schedule", "/workbench/schedule/delete"} {
		t.Run(path, func(t *testing.T) {
			e := openBehaviorEngine(t, taskengine.Options{})
			registerBehavior(t, e, "fake", singleAdapter(syntheticTask("fake", "account-a", "task-a")))
			for _, body := range []string{"null", "[]", "123"} {
				w := workbenchHTTP(t, e, http.MethodPost, path, body)
				if w.Code != http.StatusBadRequest {
					t.Errorf("body=%s status=%d, want 400", body, w.Code)
				}
			}
		})
	}
}

func TestWorkbenchHTTPDoesNotTrustClientScheduleRuntimeState(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	registerBehavior(t, e, "fake", singleAdapter(syntheticTask("fake", "account-a", "task-a")))
	stamp := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body, err := json.Marshal(map[string]any{
		"id": "scheduled", "provider_id": "fake", "enabled": true, "hours": []int{9}, "minute": 0, "timezone": "UTC",
		"last_run_id": "client-invented-run", "last_dispatched": stamp, "next_run": "2099-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	w := workbenchHTTP(t, e, http.MethodPost, "/workbench/schedule", string(body))
	if w.Code == http.StatusBadRequest {
		return
	}
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected response code %d", w.Code)
	}
	schedules := e.Schedules()
	if len(schedules) != 1 {
		t.Fatal("schedule missing after accepted command")
	}
	if schedules[0].LastRunID != "" || schedules[0].LastDispatched != nil {
		t.Error("new schedule trusted client-invented execution state")
	}
}

func TestWorkbenchHTTPPreservesAcceptedRunAndValidTrailingWhitespace(t *testing.T) {
	e := openBehaviorEngine(t, taskengine.Options{})
	registerBehavior(t, e, "fake", singleAdapter(syntheticTask("fake", "account-a", "task-a")))
	w := workbenchHTTP(t, e, http.MethodPost, "/workbench/run", "{\"provider_id\":\"fake\"} \r\n\t")
	if w.Code != http.StatusAccepted {
		t.Fatalf("run acceptance status=%d", w.Code)
	}
	var body struct {
		OK  bool           `json:"ok"`
		Run taskengine.Run `json:"run"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Run.ID == "" || body.Run.Trigger != "manual" || body.Run.ScheduleID != "" {
		t.Errorf("invalid accepted-run metadata: %+v", body.Run)
	}
}
