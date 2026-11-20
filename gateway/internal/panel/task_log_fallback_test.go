package panel

import (
	"net/http"
	"testing"
)

func TestNewConsoleDoesNotFallbackToLegacyTaskLog(t *testing.T) {
	h := newHarness(t)
	h.server.opts.Console = true
	doc := h.writeCredentials()
	h.transport.replies[http.MethodGet+" /workbench/task/log"] = stubReply{Code: http.StatusNotFound, Body: `{"error":"not_found"}`}
	before := len(h.transport.recorded())
	w := h.do(http.MethodGet, "/api/task/log?key=checkin&provider=workbuddy", "", map[string]string{"Cookie": CookieName + "=" + h.token(doc)})
	body := decode(t, w)
	if w.Code != http.StatusOK || body["ok"] != false || jsonString(body["message"]) == "" {
		t.Fatalf("missing new-engine log was not reported explicitly: %d %v", w.Code, body)
	}
	calls := h.transport.recorded()[before:]
	if len(calls) != 1 || calls[0].Endpoint != "/workbench/task/log?key=checkin&provider=workbuddy" {
		t.Fatalf("new console silently queried legacy scheduler: %+v", calls)
	}
}

func TestNewConsoleRejectsTaskLogWithoutProviderScope(t *testing.T) {
	h := newHarness(t)
	h.server.opts.Console = true
	doc := h.writeCredentials()
	before := len(h.transport.recorded())
	w := h.do(http.MethodGet, "/api/task/log?key=checkin", "", map[string]string{"Cookie": CookieName + "=" + h.token(doc)})
	body := decode(t, w)
	if body["ok"] != false || len(h.transport.recorded()) != before {
		t.Fatal("unscoped console log request was not rejected locally")
	}
}

func TestNewConsoleTaskLogForwardsProviderScope(t *testing.T) {
	h := newHarness(t)
	h.server.opts.Console = true
	doc := h.writeCredentials()
	h.transport.replies[http.MethodGet+" /workbench/task/log"] = stubReply{Code: http.StatusOK, Body: `{"lines":["fixture scoped output"],"running":true}`}
	before := len(h.transport.recorded())
	w := h.do(http.MethodGet, "/api/task/log?key=checkin&provider=trae", "", map[string]string{"Cookie": CookieName + "=" + h.token(doc)})
	body := decode(t, w)
	calls := h.transport.recorded()[before:]
	if w.Code != http.StatusOK || body["ok"] != true || len(calls) != 1 || calls[0].Endpoint != "/workbench/task/log?key=checkin&provider=trae" {
		t.Fatal("task output lost its provider scope in the panel bridge")
	}
}

func TestLegacyPanelKeepsLegacyTaskLogFallback(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	h.transport.replies[http.MethodGet+" /workbench/task/log"] = stubReply{Code: http.StatusNotFound, Body: `{"error":"not_found"}`}
	h.transport.replies[http.MethodGet+" /tasks/checkin/log"] = stubReply{Code: http.StatusOK, Body: `{"lines":["fixture legacy log"]}`}
	w := h.do(http.MethodGet, "/api/task/log?key=checkin&provider=workbuddy", "", map[string]string{"Cookie": CookieName + "=" + h.token(doc)})
	body := decode(t, w)
	if body["ok"] != true {
		t.Fatalf("legacy panel no longer queries its scheduler: %v", body)
	}
	calls := h.transport.recorded()
	if len(calls) < 2 || calls[len(calls)-1].Endpoint != "/tasks/checkin/log" {
		t.Fatalf("legacy scheduler fallback contract changed: %+v", calls)
	}
}
