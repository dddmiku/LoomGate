package panel

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWorkbenchInProcessManagementCarriesTimeoutAndParentDeadline(t *testing.T) {
	for _, earlierParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "configured-budget", true: "earlier-parent"}[earlierParent], func(t *testing.T) {
			ctx := context.Background()
			budget := 100 * time.Millisecond
			if earlierParent {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
				budget = 20 * time.Millisecond
			}
			start := time.Now()
			client := &ManagementClient{Timeout: 100 * time.Millisecond, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				deadline, ok := r.Context().Deadline()
				if !ok || deadline.After(start.Add(budget+10*time.Millisecond)) {
					t.Errorf("internal catalog handler did not inherit bounded management deadline: present=%v", ok)
				}
				_, _ = w.Write([]byte(`{"tasks":[]}`))
			})}
			code, _ := client.Request(ctx, http.MethodGet, "/workbench/catalog?provider=fake", nil)
			if code != http.StatusOK {
				t.Fatalf("synthetic catalog request failed: %d", code)
			}
		})
	}
}

func TestWorkbenchPanelRequiresSessionAndSameOriginWriteMarker(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	for _, path := range []string{"/api/workbench", "/api/workbench/catalog?provider=fake&accounts=account-a"} {
		w := h.do(http.MethodGet, path, "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s status=%d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/workbench/run", "/api/workbench/cancel", "/api/workbench/schedule", "/api/workbench/schedule/delete"} {
		for _, tc := range []struct {
			name    string
			headers map[string]string
			code    int
		}{
			{"no-session", map[string]string{"X-Admin-Request": "1"}, http.StatusUnauthorized},
			{"no-write-marker", map[string]string{"Cookie": CookieName + "=" + h.token(doc)}, http.StatusForbidden},
			{"foreign-origin", map[string]string{"Cookie": CookieName + "=" + h.token(doc), "X-Admin-Request": "1", "Origin": "https://not-the-panel.invalid"}, http.StatusForbidden},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				before := len(h.transport.recorded())
				w := h.do(http.MethodPost, path, `{}`, tc.headers)
				if w.Code != tc.code || len(h.transport.recorded()) != before {
					t.Errorf("unauthorized request forwarded: status=%d calls=%d", w.Code, len(h.transport.recorded())-before)
				}
			})
		}
	}
	if len(h.transport.recorded()) != 0 {
		t.Fatal("an unauthorized workbench request reached management transport")
	}
}

func TestWorkbenchPanelPreservesAsyncAcceptanceAndResult(t *testing.T) {
	for _, path := range []string{"/workbench/run", "/workbench/cancel"} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t)
			doc := h.writeCredentials()
			h.transport.replies[http.MethodPost+" "+path] = stubReply{Code: http.StatusAccepted, Body: `{"ok":true,"run":{"id":"synthetic-run-id","status":"queued"},"cancel_requested":true}`}
			w := h.postJSON(doc, "/api"+path, map[string]any{})
			body := decode(t, w)
			if w.Code < 200 || w.Code >= 300 || body["ok"] != true {
				t.Fatalf("202 acceptance was converted into failure: status=%d body=%v", w.Code, body)
			}
			if path == "/workbench/run" && jsonString(jsonObject(body["run"])["id"]) != "synthetic-run-id" {
				t.Error("accepted run ID was lost by panel forwarding")
			}
			if path == "/workbench/cancel" && body["cancel_requested"] != true {
				t.Error("cancel acknowledgement was lost by panel forwarding")
			}
		})
	}
}

func TestWorkbenchPanelRejectsTrailingJSONInsteadOfSilentlyReencodingPrefix(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	w := h.do(http.MethodPost, "/api/workbench/run", `{"provider_id":"fake"} {"unexpected":true}`, map[string]string{
		"Cookie": CookieName + "=" + h.token(doc), "X-Admin-Request": "1",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("trailing JSON status=%d, want 400", w.Code)
	}
	if len(h.transport.recorded()) != 0 {
		t.Error("panel dropped trailing JSON and forwarded a mutation")
	}
}

func TestWorkbenchPanelKeepsValidatedQueryAndBackendFailureMeaning(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	w := h.do(http.MethodGet, "/api/workbench/catalog?provider=fake&accounts=account-a%2Caccount-b", "", map[string]string{"Cookie": CookieName + "=" + h.token(doc)})
	if w.Code != http.StatusOK {
		t.Fatal("authenticated catalog request rejected")
	}
	calls := h.transport.recorded()
	if len(calls) != 1 || calls[0].Endpoint != "/workbench/catalog?provider=fake&accounts=account-a%2Caccount-b" {
		t.Fatalf("catalog account filter changed during forwarding: %+v", calls)
	}
	h.transport.replies[http.MethodPost+" /workbench/run"] = stubReply{Code: http.StatusConflict, Body: `{"error":"run_rejected"}`}
	w = h.postJSON(doc, "/api/workbench/run", map[string]any{"provider_id": "fake"})
	body := decode(t, w)
	if body["ok"] == true {
		t.Fatal("backend rejection was reported as success")
	}
	if body["error"] == nil && strings.Contains(jsonString(body["message"]), "网关未响应") {
		t.Error("known backend rejection erased and misreported as offline/old gateway")
	}
}
