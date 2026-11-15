package providercatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
)

type accountList struct {
	rows []accountvault.Account
	err  error
}

func (a accountList) List() ([]accountvault.Account, error) { return a.rows, a.err }

type fetchFunc func(context.Context, string, bool) (doubao.Catalog, error)

func (f fetchFunc) Catalog(ctx context.Context, id string, force bool) (doubao.Catalog, error) {
	return f(ctx, id, force)
}
func acct(id string, enabled bool) accountvault.Account {
	return accountvault.Account{ID: id, Provider: "doubao", Enabled: enabled}
}
func request(t *testing.T, s *Service, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	if !s.Route(w, httptest.NewRequest("GET", path, nil)) {
		t.Fatal("missing management route")
	}
	return w
}
func decodePage(t *testing.T, w *httptest.ResponseRecorder) Page {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("unexpected HTTP status %d", w.Code)
	}
	var p Page
	if json.Unmarshal(w.Body.Bytes(), &p) != nil {
		t.Fatal("invalid catalog page")
	}
	return p
}

func TestCatalogAccountScopeAndDisabledNeverFetched(t *testing.T) {
	var calls atomic.Int32
	s := New(Options{Accounts: accountList{rows: []accountvault.Account{acct("disabled", false), acct("active", true), {ID: "another", Provider: "other", Enabled: true}}}, Doubao: fetchFunc(func(_ context.Context, id string, force bool) (doubao.Catalog, error) {
		calls.Add(1)
		if id != "active" || !force {
			t.Error("wrong scope or refresh")
		}
		return doubao.Catalog{Models: []doubao.Model{{ID: "doubao:fixture", Name: "Synthetic"}}, FetchedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}, nil
	})})
	p := decodePage(t, request(t, s, "/workbench/models?provider=doubao&account=disabled"))
	if p.Total != 1 || p.Catalogs[0].Status != "disabled" || calls.Load() != 0 {
		t.Fatal("disabled account fetched")
	}
	p = decodePage(t, request(t, s, "/workbench/models?account=active&refresh=1"))
	row := p.Catalogs[0]
	if row.Status != "ready" || row.RoutingStatus != "catalog_only" || len(row.Models) != 1 || row.FetchedAt == nil || calls.Load() != 1 {
		t.Fatal("live catalog meaning lost")
	}
	if request(t, s, "/workbench/models?account=missing").Code != 404 {
		t.Fatal("unknown account treated as all accounts")
	}
	if len(decodePage(t, request(t, s, "/workbench/models?offset=200")).Catalogs) != 0 {
		t.Fatal("pagination outside scope")
	}
}
func TestCatalogPartialFailureKeepsOtherAccountsAndNeverLeaksError(t *testing.T) {
	s := New(Options{Accounts: accountList{rows: []accountvault.Account{acct("good", true), acct("bad", true), acct("stale", true), acct("panic", true)}}, Doubao: fetchFunc(func(_ context.Context, id string, _ bool) (doubao.Catalog, error) {
		switch id {
		case "good":
			return doubao.Catalog{Models: []doubao.Model{{ID: "ok"}}}, nil
		case "stale":
			return doubao.Catalog{Models: []doubao.Model{{ID: "old"}}, Stale: true, RetryAt: time.Now().Add(time.Second)}, errors.New("PRIVATE_ERROR_FIXTURE")
		case "panic":
			panic("PRIVATE_PANIC_FIXTURE")
		default:
			return doubao.Catalog{}, &doubao.APIError{Kind: "login_required"}
		}
	})})
	w := request(t, s, "/workbench/models")
	p := decodePage(t, w)
	statuses := map[string]string{}
	for _, r := range p.Catalogs {
		statuses[r.AccountID] = r.Status
	}
	if statuses["good"] != "ready" || statuses["stale"] != "stale" || statuses["bad"] != "unavailable" || statuses["panic"] != "unavailable" {
		t.Fatal("partial failure semantics lost")
	}
	if strings.Contains(w.Body.String(), "PRIVATE_") {
		t.Fatal("raw provider error leaked")
	}
}
func TestCatalogDeadlineConcurrencyAndPagination(t *testing.T) {
	rows := []accountvault.Account{}
	for i := 0; i < 25; i++ {
		rows = append(rows, acct(fmt.Sprintf("account-%02d", i), true))
	}
	var active, peak, calls atomic.Int32
	s := New(Options{Accounts: accountList{rows: rows}, Concurrency: 2, Timeout: 30 * time.Millisecond, Doubao: fetchFunc(func(ctx context.Context, _ string, _ bool) (doubao.Catalog, error) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-ctx.Done()
		return doubao.Catalog{}, ctx.Err()
	})})
	start := time.Now()
	p := decodePage(t, request(t, s, "/workbench/models?offset=2&limit=4"))
	if time.Since(start) > time.Second {
		t.Fatal("deadline ignored")
	}
	if p.Total != 25 || !p.HasMore || len(p.Catalogs) != 4 || peak.Load() > 2 {
		t.Fatal("page or concurrency cap broken")
	}
	for _, r := range p.Catalogs {
		if r.Status != "unavailable" || r.ErrorCode != "catalog_timeout" {
			t.Fatal("timeout state lost")
		}
	}
}
func TestCatalogRejectsAmbiguousInputBeforeFetching(t *testing.T) {
	var calls atomic.Int32
	s := New(Options{Accounts: accountList{rows: []accountvault.Account{acct("fixture", true)}}, Doubao: fetchFunc(func(context.Context, string, bool) (doubao.Catalog, error) {
		calls.Add(1)
		return doubao.Catalog{}, nil
	})})
	for _, q := range []string{"account=fixture&account=missing", "account=../../private", "offset=-1", "limit=21", "limit=0", "refresh=true", "provider=unknown", "unknown=1", "account=fixture;ignored=1", "account=%zz"} {
		if request(t, s, "/workbench/models?"+q).Code != 400 {
			t.Errorf("query accepted: %s", q)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid query reached provider")
	}
	w := httptest.NewRecorder()
	s.Route(w, httptest.NewRequest("POST", "/workbench/models", nil))
	if w.Code != 405 {
		t.Fatal("unexpected mutation surface")
	}
}
func TestCatalogProviderRegistryReflectsCapabilitiesAndStorageFailure(t *testing.T) {
	s := New(Options{Accounts: accountList{err: errors.New("PRIVATE_STORAGE_FIXTURE")}, LegacyCounts: func() map[string]Count { return map[string]Count{"trae": {Total: 2, Enabled: 1}} }})
	w := request(t, s, "/workbench/providers")
	var body struct {
		Providers []Provider `json:"providers"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Providers) != 3 {
		t.Fatal("provider descriptors lost")
	}
	for _, p := range body.Providers {
		if p.ID == "doubao" && (p.Inference || p.Login || p.ErrorCode != "accounts_unavailable") {
			t.Fatal("unsupported capability advertised")
		}
	}
	w = request(t, s, "/workbench/accounts")
	if w.Code != 503 || strings.Contains(w.Body.String(), "PRIVATE_") {
		t.Fatal("storage failure hidden or leaked")
	}
}
