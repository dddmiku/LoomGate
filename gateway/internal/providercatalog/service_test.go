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
)

type accountList struct {
	rows []accountvault.Account
	err  error
}

func (a accountList) List() ([]accountvault.Account, error) { return a.rows, a.err }

// fixtureSource is a minimal ProviderSource used to exercise the neutral
// catalog pipeline. It replaces the old Doubao-specific fetchFunc so these
// tests no longer depend on any provider package.
type fixtureSource struct {
	id      string
	catalog func(context.Context, string, bool) (Catalog, error)
	static  []Model
	inf     bool
}

func (f fixtureSource) ID() string                          { return f.id }
func (f fixtureSource) Name() string                        { return f.id }
func (f fixtureSource) CatalogScope() string                { return "account" }
func (f fixtureSource) Inference() bool                     { return f.inf }
func (f fixtureSource) Login() bool                         { return false }
func (f fixtureSource) StaticModels() []Model               { return f.static }
func (f fixtureSource) Display(string) (string, string, string, time.Time, bool) {
	return "", "", "", time.Time{}, false
}
// liveFixture adds the optional live-catalog half.
type liveFixture struct{ fixtureSource }

func (f liveFixture) Catalog(ctx context.Context, account string, force bool) (Catalog, error) {
	return f.catalog(ctx, account, force)
}

func (f fixtureSource) StaticModelsOrNil() []Model { return f.static }

func acct(id string, enabled bool) accountvault.Account {
	return accountvault.Account{ID: id, Provider: "fixture", Enabled: enabled}
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

// A provider error code must survive to the row so the console can render an
// actionable message, without the raw upstream error text leaking through.
func TestProviderErrorCodeRemainsActionableWithoutRawDetails(t *testing.T) {
	for _, code := range []string{"login_required", "identity_mismatch", "timeout", "busy", "upstream_rejected"} {
		t.Run(code, func(t *testing.T) {
			s := New(Options{Accounts: accountList{rows: []accountvault.Account{acct("synthetic", true)}}, Sources: []ProviderSource{
				liveFixture{fixtureSource{id: "fixture", inf: true, catalog: func(context.Context, string, bool) (Catalog, error) {
					return Catalog{ErrorCode: code}, errors.New("PRIVATE_UPSTREAM_ERROR")
				}}},
			}})
			p := decodePage(t, request(t, s, "/workbench/models"))
			if len(p.Catalogs) != 1 || p.Catalogs[0].ErrorCode != code || p.Catalogs[0].Status != "unavailable" {
				t.Fatalf("provider failure became generic or successful: %+v", p.Catalogs)
			}
		})
	}
}

func TestCatalogAccountScopeAndDisabledNeverFetched(t *testing.T) {
	var calls atomic.Int32
	s := New(Options{Accounts: accountList{rows: []accountvault.Account{acct("disabled", false), acct("active", true), {ID: "another", Provider: "other", Enabled: true}}}, Sources: []ProviderSource{
		liveFixture{fixtureSource{id: "fixture", inf: true, catalog: func(_ context.Context, id string, force bool) (Catalog, error) {
			calls.Add(1)
			if id != "active" || !force {
				t.Error("wrong scope or refresh")
			}
			return Catalog{Status: "ready", Models: []Model{{ID: "fixture:model", Name: "Synthetic"}}, FetchedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}, nil
		}}},
	}})
	p := decodePage(t, request(t, s, "/workbench/models?provider=fixture&account=disabled"))
	if p.Total != 1 || p.Catalogs[0].Status != "disabled" || calls.Load() != 0 {
		t.Fatal("disabled account fetched")
	}
	p = decodePage(t, request(t, s, "/workbench/models?account=active&refresh=1"))
	row := p.Catalogs[0]
	if row.Status != "ready" || row.RoutingStatus != "single_turn_text" || len(row.Models) != 1 || row.FetchedAt == nil || calls.Load() != 1 {
		t.Fatalf("live catalog meaning lost: %+v", row)
	}
	if request(t, s, "/workbench/models?account=missing").Code != 404 {
		t.Fatal("unknown account treated as all accounts")
	}
	if len(decodePage(t, request(t, s, "/workbench/models?offset=200")).Catalogs) != 0 {
		t.Fatal("pagination outside scope")
	}
}

func TestCatalogPartialFailureKeepsOtherAccountsAndNeverLeaksError(t *testing.T) {
	s := New(Options{Accounts: accountList{rows: []accountvault.Account{acct("good", true), acct("bad", true), acct("stale", true), acct("panic", true)}}, Sources: []ProviderSource{
		liveFixture{fixtureSource{id: "fixture", inf: true, catalog: func(_ context.Context, id string, _ bool) (Catalog, error) {
			switch id {
			case "good":
				return Catalog{Status: "ready", Models: []Model{{ID: "ok"}}}, nil
			case "stale":
				return Catalog{Models: []Model{{ID: "old"}}, Status: "stale", RetryAt: time.Now().Add(time.Second)}, errors.New("PRIVATE_ERROR_FIXTURE")
			case "panic":
				panic("PRIVATE_PANIC_FIXTURE")
			default:
				return Catalog{ErrorCode: "login_required"}, errors.New("PRIVATE_ERROR_FIXTURE")
			}
		}}},
	}})
	w := request(t, s, "/workbench/models")
	p := decodePage(t, w)
	statuses := map[string]string{}
	for _, r := range p.Catalogs {
		statuses[r.AccountID] = r.Status
	}
	if statuses["good"] != "ready" || statuses["stale"] != "stale" || statuses["bad"] != "unavailable" || statuses["panic"] != "unavailable" {
		t.Fatalf("partial failure semantics lost: %v", statuses)
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
	s := New(Options{Accounts: accountList{rows: rows}, Concurrency: 2, Timeout: 30 * time.Millisecond, Sources: []ProviderSource{
		liveFixture{fixtureSource{id: "fixture", inf: true, catalog: func(ctx context.Context, _ string, _ bool) (Catalog, error) {
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
			return Catalog{}, ctx.Err()
		}}},
	}})
	start := time.Now()
	p := decodePage(t, request(t, s, "/workbench/models?offset=2&limit=4"))
	if time.Since(start) > time.Second {
		t.Fatal("deadline ignored")
	}
	if p.Total != 25 || !p.HasMore || len(p.Catalogs) != 4 || peak.Load() > 2 {
		t.Fatalf("page or concurrency cap broken: total=%d hasMore=%v rows=%d peak=%d", p.Total, p.HasMore, len(p.Catalogs), peak.Load())
	}
	for _, r := range p.Catalogs {
		if r.Status != "unavailable" || r.ErrorCode != "catalog_timeout" {
			t.Fatalf("timeout state lost: %+v", r)
		}
	}
}

func TestCatalogRejectsAmbiguousInputBeforeFetching(t *testing.T) {
	var calls atomic.Int32
	s := New(Options{Accounts: accountList{rows: []accountvault.Account{acct("fixture", true)}}, Sources: []ProviderSource{
		liveFixture{fixtureSource{id: "fixture", inf: true, catalog: func(context.Context, string, bool) (Catalog, error) {
			calls.Add(1)
			return Catalog{}, nil
		}}},
	}})
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

func TestProviderRegistryReflectsCapabilitiesAndStorageFailure(t *testing.T) {
	s := New(Options{
		Accounts:     accountList{err: errors.New("PRIVATE_STORAGE_FIXTURE")},
		LegacyCounts: func() map[string]Count { return map[string]Count{"trae": {Total: 2, Enabled: 1}} },
		LegacyProviders: []Provider{
			{ID: "workbuddy", Name: "WorkBuddy", CatalogScope: "provider", Inference: true, Login: true},
			{ID: "trae", Name: "Trae", CatalogScope: "provider", Inference: true, Login: true},
		},
	})
	w := request(t, s, "/workbench/providers")
	var body struct {
		Providers []Provider `json:"providers"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Providers) != 2 {
		t.Fatalf("provider descriptors lost: %+v", body.Providers)
	}
	if body.Providers[1].ID != "trae" || body.Providers[1].Accounts.Total != 2 {
		t.Fatal("legacy provider counts lost")
	}
	w = request(t, s, "/workbench/accounts")
	if w.Code != 503 || strings.Contains(w.Body.String(), "PRIVATE_") {
		t.Fatal("storage failure hidden or leaked")
	}
}

// A provider with a fixed catalog must answer without any network call, and the
// rows must be marked callable so the console does not show "catalog only".
func TestStaticCatalogProviderIsCallable(t *testing.T) {
	s := New(Options{Accounts: accountList{rows: []accountvault.Account{acct("synthetic", true)}}, Sources: []ProviderSource{
		fixtureSource{id: "fixture", inf: true, static: []Model{{ID: "fixture:a"}, {ID: "fixture:b"}}},
	}})
	p := decodePage(t, request(t, s, "/workbench/models"))
	if len(p.Catalogs) != 1 || p.Catalogs[0].Status != "ready" || len(p.Catalogs[0].Models) != 2 {
		t.Fatalf("static catalog lost: %+v", p.Catalogs)
	}
	for _, m := range p.Catalogs[0].Models {
		if !m.InferenceAvailable {
			t.Fatalf("static model not marked callable: %+v", m)
		}
	}
	if p.Catalogs[0].StreamingMode != "incremental" {
		t.Fatal("static provider streaming mode lost")
	}
}
