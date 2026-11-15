package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWorkbenchManagementBoundaryCannotBeForgedWithHeaders(t *testing.T) {
	var calls atomic.Int32
	workbench := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := NewHandler(Config{Workbench: workbench, Pool: testPoolWith()})
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/workbench"}, {http.MethodGet, "/workbench/catalog?provider=fake"},
		{http.MethodGet, "/workbench/providers"}, {http.MethodGet, "/workbench/accounts"}, {http.MethodGet, "/workbench/models?account=fixture"},
		{http.MethodPost, "/workbench/run"}, {http.MethodPost, "/workbench/cancel"},
		{http.MethodPost, "/workbench/schedule"}, {http.MethodPost, "/workbench/schedule/delete"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			r := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
			r.Header.Set("X-Admin-Request", "1")
			r.Header.Set("X-Internal-Admin", "true")
			r.Header.Set("X-Forwarded-For", "127.0.0.1")
			r.RemoteAddr = "127.0.0.1:43210"
			before := calls.Load()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized || calls.Load() != before {
				t.Errorf("public request reached internal workbench: status=%d calls=%d", w.Code, calls.Load()-before)
			}
			w = httptest.NewRecorder()
			h.InternalHandler().ServeHTTP(w, httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`)))
			if w.Code != http.StatusOK || calls.Load() != before+1 {
				t.Errorf("internal management route failed: status=%d calls=%d", w.Code, calls.Load()-before)
			}
		})
	}
}
