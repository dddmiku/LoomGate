// Package providercatalog is the read-only management view of provider accounts
// and catalogs. It never exports sessions or adds unimplemented inference routes.
package providercatalog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
)

type Accounts interface {
	List() ([]accountvault.Account, error)
}
type DoubaoCatalog interface {
	Catalog(context.Context, string, bool) (doubao.Catalog, error)
}
type Count struct {
	Total   int `json:"total"`
	Enabled int `json:"enabled"`
}
type Provider struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	CatalogScope string `json:"catalog_scope"`
	Inference    bool   `json:"inference_available"`
	Login        bool   `json:"login_available"`
	Accounts     Count  `json:"accounts"`
	ErrorCode    string `json:"error_code,omitempty"`
}
type Options struct {
	Accounts        Accounts
	Doubao          DoubaoCatalog
	LegacyCounts    func() map[string]Count
	Timeout         time.Duration
	Concurrency     int
	DoubaoInference bool
}
type Service struct {
	opts  Options
	slots chan struct{}
}
type Row struct {
	AccountID     string         `json:"account_id"`
	ProviderID    string         `json:"provider_id"`
	Status        string         `json:"status"`
	RoutingStatus string         `json:"routing_status"`
	Models        []doubao.Model `json:"models"`
	FetchedAt     *time.Time     `json:"fetched_at,omitempty"`
	ExpiresAt     *time.Time     `json:"expires_at,omitempty"`
	RetryAt       *time.Time     `json:"retry_at,omitempty"`
	ErrorCode     string         `json:"error_code,omitempty"`
}
type Page struct {
	Catalogs []Row `json:"catalogs"`
	Total    int   `json:"total"`
	Offset   int   `json:"offset"`
	Limit    int   `json:"limit"`
	HasMore  bool  `json:"has_more"`
}

func New(opts Options) *Service {
	if opts.Timeout <= 0 {
		opts.Timeout = 12 * time.Second
	}
	if opts.Concurrency <= 0 || opts.Concurrency > 8 {
		opts.Concurrency = 4
	}
	return &Service{opts: opts, slots: make(chan struct{}, opts.Concurrency)}
}
func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Service) accounts() ([]accountvault.Account, error) {
	if s.opts.Accounts == nil {
		return nil, errors.New("accounts_unavailable")
	}
	rows, err := s.opts.Accounts.List()
	if err != nil {
		return nil, errors.New("accounts_unavailable")
	}
	out := []accountvault.Account{}
	for _, a := range rows {
		if a.Provider == "doubao" {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Route reports whether it owns the management path; authorization is applied
// once by the caller's internal management boundary.
func (s *Service) Route(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/workbench/account/toggle" {
		s.toggleAccount(w, r)
		return true
	}
	if r.URL.Path != "/workbench/providers" && r.URL.Path != "/workbench/accounts" && r.URL.Path != "/workbench/models" {
		return false
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		write(w, 405, map[string]string{"error": "method_not_allowed"})
		return true
	}
	accounts, err := s.accounts()
	if r.URL.Path == "/workbench/providers" {
		counts := map[string]Count{}
		if s.opts.LegacyCounts != nil {
			counts = s.opts.LegacyCounts()
		}
		db := Count{Total: len(accounts)}
		for _, a := range accounts {
			if a.Enabled {
				db.Enabled++
			}
		}
		out := []Provider{{ID: "workbuddy", Name: "WorkBuddy", CatalogScope: "provider", Inference: true, Login: true, Accounts: counts["workbuddy"]}, {ID: "trae", Name: "Trae", CatalogScope: "provider", Inference: true, Login: true, Accounts: counts["trae"]}, {ID: "doubao", Name: "豆包工作", CatalogScope: "account", Accounts: db, Inference: s.opts.DoubaoInference}}
		if err != nil {
			out[2].ErrorCode = "accounts_unavailable"
		}
		write(w, 200, map[string]any{"providers": out})
		return true
	}
	if err != nil {
		write(w, 503, map[string]string{"error": "accounts_unavailable"})
		return true
	}
	if r.URL.Path == "/workbench/accounts" {
		write(w, 200, map[string]any{"accounts": accounts})
		return true
	}
	s.models(w, r, accounts)
	return true
}

func (s *Service) toggleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		write(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	var req struct {
		ID       string `json:"id"`
		Enabled  *bool  `json:"enabled"`
		Revision uint64 `json:"revision"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF || !accountID.MatchString(req.ID) || req.Revision == 0 || req.Enabled == nil {
		write(w, 400, map[string]string{"error": "invalid_account_request"})
		return
	}
	store, ok := s.opts.Accounts.(interface {
		SetEnabled(string, bool, uint64) (accountvault.Account, error)
	})
	if !ok {
		write(w, 503, map[string]string{"error": "accounts_read_only"})
		return
	}
	a, err := store.SetEnabled(req.ID, *req.Enabled, req.Revision)
	if err != nil {
		if errors.Is(err, accountvault.ErrConflict) {
			write(w, 409, map[string]string{"error": "account_revision_conflict"})
		} else {
			write(w, 503, map[string]string{"error": "accounts_unavailable"})
		}
		return
	}
	write(w, 200, map[string]any{"ok": true, "account": a})
}

var accountID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (s *Service) models(w http.ResponseWriter, r *http.Request, accounts []accountvault.Account) {
	q, err := parseQuery(r)
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid_catalog_query"})
		return
	}
	if q.account != "" {
		filtered := []accountvault.Account{}
		for _, a := range accounts {
			if a.ID == q.account {
				filtered = append(filtered, a)
			}
		}
		if len(filtered) == 0 {
			write(w, 404, map[string]string{"error": "account_not_found"})
			return
		}
		accounts = filtered
	}
	page := Page{Catalogs: []Row{}, Total: len(accounts), Offset: q.offset, Limit: q.limit}
	if q.offset >= len(accounts) {
		write(w, 200, page)
		return
	}
	end := q.offset + q.limit
	if end > len(accounts) {
		end = len(accounts)
	}
	page.HasMore = end < len(accounts)
	selected := accounts[q.offset:end]
	page.Catalogs = make([]Row, len(selected))
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.Timeout)
	defer cancel()
	var wg sync.WaitGroup
	for i, a := range selected {
		wg.Add(1)
		go func(i int, a accountvault.Account) { defer wg.Done(); page.Catalogs[i] = s.fetch(ctx, a, q.refresh) }(i, a)
	}
	wg.Wait()
	write(w, 200, page)
}

type query struct {
	account       string
	offset, limit int
	refresh       bool
}

func parseQuery(r *http.Request) (query, error) {
	out := query{limit: 20}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return out, err
	}
	for k, v := range values {
		if len(v) != 1 {
			return out, errors.New("duplicate_query")
		}
		switch k {
		case "provider", "account", "offset", "limit", "refresh":
		default:
			return out, errors.New("unknown_query")
		}
	}
	if p := values.Get("provider"); p != "" && p != "doubao" {
		return out, errors.New("unsupported_provider")
	}
	out.account = values.Get("account")
	if out.account != "" && !accountID.MatchString(out.account) {
		return out, errors.New("invalid_account")
	}
	if v := values.Get("offset"); v != "" {
		out.offset, err = strconv.Atoi(v)
		if err != nil || out.offset < 0 || out.offset > 100000 {
			return out, errors.New("invalid_offset")
		}
	}
	if v := values.Get("limit"); v != "" {
		out.limit, err = strconv.Atoi(v)
		if err != nil || out.limit < 1 || out.limit > 20 {
			return out, errors.New("invalid_limit")
		}
	}
	switch values.Get("refresh") {
	case "1":
		out.refresh = true
	case "", "0":
	default:
		return out, errors.New("invalid_refresh")
	}
	return out, nil
}
func (s *Service) fetch(ctx context.Context, a accountvault.Account, force bool) (row Row) {
	row = Row{AccountID: a.ID, ProviderID: "doubao", Status: "unavailable", RoutingStatus: "catalog_only", Models: []doubao.Model{}}
	defer func() {
		if recover() != nil {
			row.Models = []doubao.Model{}
			row.Status = "unavailable"
			row.ErrorCode = "catalog_failed"
		}
	}()
	if !a.Enabled {
		row.Status = "disabled"
		return
	}
	if s.opts.Doubao == nil {
		row.ErrorCode = "catalog_unavailable"
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		row.ErrorCode = "catalog_timeout"
		return
	}
	snapshot, err := s.opts.Doubao.Catalog(ctx, a.ID, force)
	if !snapshot.FetchedAt.IsZero() {
		row.FetchedAt = &snapshot.FetchedAt
	}
	if !snapshot.ExpiresAt.IsZero() {
		row.ExpiresAt = &snapshot.ExpiresAt
	}
	if !snapshot.RetryAt.IsZero() {
		row.RetryAt = &snapshot.RetryAt
	}
	if len(snapshot.Models) > 1000 {
		row.ErrorCode = "catalog_limit"
		return
	}
	if len(snapshot.Models) > 0 {
		row.Models = snapshot.Models
		if s.opts.DoubaoInference && snapshot.RuntimeBound {
			row.RoutingStatus = "single_turn_text"
		}
	}
	if err != nil || snapshot.Stale || snapshot.ErrorCode != "" {
		row.ErrorCode = "catalog_failed"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			row.ErrorCode = "catalog_timeout"
		}
		var apiErr *doubao.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Kind {
			case "login_required", "desktop_unavailable", "desktop_identity_mismatch", "desktop_login_required", "desktop_version_unsupported", "desktop_timeout", "desktop_busy", "desktop_upstream_rejected", "desktop_cleanup_uncertain", "desktop_ownership_lost", "desktop_submission_uncertain", "composer_not_empty":
				row.ErrorCode = apiErr.Kind
			}
		}
		if len(row.Models) > 0 {
			row.Status = "stale"
		}
		return
	}
	if len(row.Models) == 0 {
		row.ErrorCode = "catalog_empty"
		return
	}
	row.Status = "ready"
	for i := range row.Models {
		row.Models[i].InferenceAvailable = s.opts.DoubaoInference && snapshot.RuntimeBound && !row.Models[i].NativeTools
	}
	return
}
