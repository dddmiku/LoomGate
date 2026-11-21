package panel

import (
	"net/http"
	"strings"
	"testing"
)

func TestConsoleEntryAndAssetSessionBoundary(t *testing.T) {
	h := newHarness(t)
	h.server.opts.Console = true
	h.server.assets = defaultAssets()
	doc := h.writeCredentials()
	for _, path := range []string{"/console/style.css", "/console/login.js"} {
		w := h.do(http.MethodGet, path, "", nil)
		if w.Code != 200 || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("public console asset unavailable: %s %d", path, w.Code)
		}
	}
	w := h.do(http.MethodGet, "/login", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "console/login.js") {
		t.Fatal("new login not served")
	}
	for _, path := range []string{"/console/app.js", "/console/plan-editor.js", "/console/model-picker.js", "/console/token-format.js"} {
		w = h.do(http.MethodGet, path, "", nil)
		if w.Code != http.StatusFound {
			t.Fatal("application asset exposed without session")
		}
	}
	headers := map[string]string{"Cookie": CookieName + "=" + h.token(doc)}
	w = h.do(http.MethodGet, "/", "", headers)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "console/app.js") || strings.Contains(w.Body.String(), "id=\"tabs\"") {
		t.Fatal("independent console entry not served")
	}
	for _, path := range []string{"/console/app.js", "/console/style.css", "/console/plan-editor.js", "/console/model-picker.js", "/console/token-format.js"} {
		if h.do(http.MethodGet, path, "", headers).Code != 200 {
			t.Fatalf("console asset missing: %s", path)
		}
	}
	if h.do(http.MethodGet, "/console/not-allowed.js", "", headers).Code != 404 {
		t.Fatal("unexpected static route allowed")
	}
	h.server.opts.Console = false
	w = h.do(http.MethodGet, "/", "", headers)
	if strings.Contains(w.Body.String(), "console/app.js") {
		t.Fatal("legacy entry changed without opt-in")
	}
}
