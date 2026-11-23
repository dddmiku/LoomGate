package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/autoclaw"
	"workbuddy2api/internal/requestlog"
)

func TestAccountActivityMergesPrivateLegacyAndPublicIdentity(t *testing.T) {
	vault, e := accountvault.Open(filepath.Join(t.TempDir(), "vault"))
	if e != nil {
		t.Fatal(e)
	}
	defer vault.Close()
	provider := &autoclaw.Provider{Vault: vault}
	legacy := "autoclaw-" + "1" + strings.Repeat("0", 10)
	public := provider.PublicAccountID(legacy)
	first := facetsRecord("fixture-legacy", "key-fixture", "fixture", "autoclaw:fixture")
	start := time.Now().Add(-time.Minute)
	first.Attempts = []requestlog.Attempt{{Number: 1, AccountID: legacy, StartedAt: start, FinishedAt: start.Add(time.Second), Status: requestlog.StatusError}}
	second := facetsRecord("fixture-public", "key-fixture", "fixture", "autoclaw:fixture")
	second.Attempts = []requestlog.Attempt{{Number: 1, AccountID: public, StartedAt: start.Add(2 * time.Second), FinishedAt: start.Add(3 * time.Second), Status: requestlog.StatusSuccess}}
	h := facetsFixture(t, []requestlog.Record{first, second})
	h.cfg.Inference = map[string]inference.Provider{"autoclaw": provider}
	code, page := facetsGet(t, h, "/requests/accounts?provider=autoclaw")
	items, _ := page["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatal("same account was counted twice after identity projection")
	}
	row := items[0].(map[string]any)
	if row["account_id"] != public || row["attempts_24h"] != float64(2) || row["last_request_id"] != second.RequestID {
		t.Fatal("account activity projection leaked private identity or lost the newest attempt")
	}
}

func TestRequestSnapshotRevisionAvoidsPayloadAndChangesOnNewRecord(t *testing.T) {
	r := facetsRecord("fixture-first", "key-fixture", "fixture", "autoclaw:fixture")
	h := facetsFixture(t, []requestlog.Record{r})
	code, first := facetsGet(t, h, "/requests?limit=30")
	revision, _ := first["revision"].(string)
	if code != 200 || len(revision) != 64 {
		t.Fatal("snapshot revision missing")
	}
	code, unchanged := facetsGet(t, h, "/requests?limit=30&revision="+revision)
	if code != 200 || unchanged["not_modified"] != true || unchanged["items"] != nil {
		t.Fatal("unchanged snapshot resent the full table")
	}
	next := facetsRecord("fixture-next", "key-fixture", "fixture", "autoclaw:fixture")
	if e := h.cfg.Requests.Append(next); e != nil {
		t.Fatal(e)
	}
	code, changed := facetsGet(t, h, "/requests?limit=30&revision="+revision)
	if code != 200 || changed["revision"] == revision || changed["items"] == nil {
		t.Fatal("new request did not invalidate snapshot revision")
	}
	for _, query := range []string{"revision=bad", "revision=" + strings.Repeat("x", 64), "revision=" + revision + "&revision=" + revision} {
		code, _ = facetsGet(t, h, "/requests?"+query)
		if code != 400 {
			t.Fatal("invalid revision bypassed validation")
		}
	}
}
func TestAccountActivityIsPrivateReadOnlyAndRejectsBadScope(t *testing.T) {
	r := facetsRecord("fixture-activity", "key-fixture", "fixture", "autoclaw:fixture")
	start := time.Now().Add(-time.Second)
	r.Attempts = []requestlog.Attempt{{Number: 1, AccountID: "fixture-account", StartedAt: start, FinishedAt: time.Now(), Status: requestlog.StatusSuccess}}
	h := facetsFixture(t, []requestlog.Record{r})
	code, page := facetsGet(t, h, "/requests/accounts?provider=autoclaw")
	items, _ := page["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["attempts_24h"] != float64(1) {
		t.Fatal("account activity was routed as request detail or omitted real usage")
	}
	for _, query := range []string{"provider=unknown", "provider=autoclaw&provider=trae", "limit=999", "revision=bad"} {
		code, _ = facetsGet(t, h, "/requests/accounts?"+query)
		if code != 400 {
			t.Fatal("activity query escaped validation")
		}
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/requests/accounts", nil))
	if w.Code != 401 {
		t.Fatal("account activity became accessible outside internal management")
	}
}
