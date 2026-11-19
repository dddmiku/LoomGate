// Package providercatalog is the read-only management view of provider accounts
// and their model catalogs.
//
// It owns no provider-specific types. Every provider plugs in through
// ProviderSource: it reports its own display name, its catalog scope, whether
// it can answer inference, and (optionally) a per-account model catalog. Adding
// a provider therefore never requires editing another provider's package, and
// deleting one leaves no dangling references.
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
)

// Model is the neutral catalog row. Provider packages translate their own
// wire types into this, so this package never imports one of them.
type Model struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	ContextWindow      int64  `json:"context_length,omitempty"`
	InferenceAvailable bool   `json:"inference_available"`
	SupportsMultiTurn  bool   `json:"supports_multi_turn"`
}

// Catalog is one account's model snapshot.
type Catalog struct {
	Models    []Model
	Status    string
	ErrorCode string
	FetchedAt time.Time
	ExpiresAt time.Time
	RetryAt   time.Time
	Streaming string
}

// AccountView is the safe account row DTO. It carries vault metadata plus only
// non-secret display facts a provider explicitly supplies.
type AccountView struct {
	ID         string     `json:"id"`
	Provider   string     `json:"provider"`
	Revision   uint64     `json:"revision"`
	UpdatedAt  time.Time  `json:"updated_at"`
	Enabled    bool       `json:"enabled"`
	Nickname   string     `json:"nickname,omitempty"`
	BotID      string     `json:"bot_id,omitempty"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Mobile     string     `json:"mobile,omitempty"`
}

// ProviderSource is everything the management view needs from one provider.
// A provider that only exposes a fixed catalog can leave Catalog nil.
type ProviderSource interface {
	// ID is the vault `provider` value this source owns.
	ID() string
	// Name is the console label.
	Name() string
	// CatalogScope describes how models are addressed ("provider" or "account").
	CatalogScope() string
	// Inference reports whether this provider can serve completions at all.
	Inference() bool
	// Login reports whether the console can start a login flow for it.
	Login() bool
	// Catalog returns one account's models. Providers that only publish a fixed
	// list implement LiveCatalog instead and leave this out; StaticModels then
	// backs the answer.
	StaticModels() []Model
	// Display returns non-secret display facts for an account.
	Display(account string) (mobile, nickname, botID string, verifiedAt time.Time, ok bool)
}

// LiveCatalog is the optional half of ProviderSource for providers that fetch a
// per-account catalog. A provider with a fixed model list omits it entirely, and
// the catalog layer answers from StaticModels without any network call.
type LiveCatalog interface {
	Catalog(ctx context.Context, account string, force bool) (Catalog, error)
}

// LegacyCounts reports account counts for providers the pool (not the vault)
// owns. It stays a callback so this package never imports the pool.
type LegacyCounts func() map[string]Count

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

type Row struct {
	AccountID     string     `json:"account_id"`
	ProviderID    string     `json:"provider_id"`
	Status        string     `json:"status"`
	RoutingStatus string     `json:"routing_status"`
	StreamingMode string     `json:"streaming_mode,omitempty"`
	Models        []Model    `json:"models"`
	FetchedAt     *time.Time `json:"fetched_at,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	RetryAt       *time.Time `json:"retry_at,omitempty"`
	ErrorCode     string     `json:"error_code,omitempty"`
}

type Page struct {
	Catalogs []Row `json:"catalogs"`
	Total    int   `json:"total"`
	Offset   int   `json:"offset"`
	Limit    int   `json:"limit"`
	HasMore  bool  `json:"has_more"`
}

type Options struct {
	Accounts     Accounts
	Sources      []ProviderSource
	LegacyCounts LegacyCounts
	// LegacyProviders are pool-owned providers that keep their historical
	// account counts (WorkBuddy, Trae). They never appear in the vault but must
	// still be listed.
	LegacyProviders []Provider
	Timeout         time.Duration
	Concurrency     int
}

type Accounts interface {
	List() ([]accountvault.Account, error)
}

type Service struct {
	opts  Options
	slots chan struct{}
	byID  map[string]ProviderSource
}

func New(opts Options) *Service {
	if opts.Timeout <= 0 {
		opts.Timeout = 12 * time.Second
	}
	if opts.Concurrency <= 0 || opts.Concurrency > 8 {
		opts.Concurrency = 4
	}
	byID := make(map[string]ProviderSource, len(opts.Sources))
	for _, s := range opts.Sources {
		if s != nil {
			byID[s.ID()] = s
		}
	}
	return &Service{opts: opts, slots: make(chan struct{}, opts.Concurrency), byID: byID}
}

func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// accounts lists only vault-backed provider accounts.
func (s *Service) accounts() ([]accountvault.Account, error) {
	if s.opts.Accounts == nil {
		return nil, errors.New("accounts_unavailable")
	}
	rows, err := s.opts.Accounts.List()
	if err != nil {
		return nil, errors.New("accounts_unavailable")
	}
	out := make([]accountvault.Account, 0, len(rows))
	for _, a := range rows {
		if _, ok := s.byID[a.Provider]; ok {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Service) accountViews(rows []accountvault.Account) []AccountView {
	out := make([]AccountView, 0, len(rows))
	for _, a := range rows {
		view := AccountView{ID: a.ID, Provider: a.Provider, Revision: a.Revision, UpdatedAt: a.UpdatedAt, Enabled: a.Enabled}
		if src := s.byID[a.Provider]; src != nil {
			if mobile, nickname, botID, verifiedAt, ok := src.Display(a.ID); ok {
				view.Mobile = mobile
				view.Nickname = nickname
				view.BotID = botID
				if !verifiedAt.IsZero() {
					value := verifiedAt
					view.VerifiedAt = &value
				}
			}
		}
		out = append(out, view)
	}
	return out
}

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
		out := make([]Provider, 0, len(s.opts.LegacyProviders)+len(s.opts.Sources))
		for _, p := range s.opts.LegacyProviders {
			if c, ok := counts[p.ID]; ok {
				p.Accounts = c
			}
			out = append(out, p)
		}
		for _, src := range s.opts.Sources {
			count := Count{Total: 0}
			for _, a := range accounts {
				if a.Provider == src.ID() {
					count.Total++
					if a.Enabled {
						count.Enabled++
					}
				}
			}
			out = append(out, Provider{
				ID: src.ID(), Name: src.Name(), CatalogScope: src.CatalogScope(),
				Inference: src.Inference(), Login: src.Login(), Accounts: count,
			})
		}
		if err != nil {
			// Accounts being unavailable must not hide the provider list; mark it
			// instead so the console can show a real error.
			for i := range out {
				if out[i].CatalogScope == "account" {
					out[i].ErrorCode = "accounts_unavailable"
				}
			}
		}
		write(w, 200, map[string]any{"providers": out})
		return true
	}
	if err != nil {
		write(w, 503, map[string]string{"error": "accounts_unavailable"})
		return true
	}
	if r.URL.Path == "/workbench/accounts" {
		write(w, 200, map[string]any{"accounts": s.accountViews(accounts)})
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
	if q.provider != "" {
		if s.byID[q.provider] == nil {
			write(w, 400, map[string]string{"error": "unsupported_provider"})
			return
		}
		filtered := accounts[:0]
		for _, a := range accounts {
			if a.Provider == q.provider {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
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
	provider      string
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
	out.provider = values.Get("provider")
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
	src := s.byID[a.Provider]
	row = Row{AccountID: a.ID, ProviderID: a.Provider, Status: "unavailable", RoutingStatus: "catalog_only", Models: []Model{}}
	defer func() {
		if recover() != nil {
			row.Models = []Model{}
			row.Status = "unavailable"
			row.ErrorCode = "catalog_failed"
		}
	}()
	if !a.Enabled {
		row.Status = "disabled"
		return
	}
	if src == nil {
		row.ErrorCode = "catalog_unavailable"
		return
	}
	live, hasCatalog := src.(LiveCatalog)
	if !hasCatalog {
		row.Models = cloneModels(src.StaticModels())
		if len(row.Models) == 0 {
			row.ErrorCode = "catalog_empty"
			return
		}
		callable := src.Inference()
		row.Status = "ready"
		if callable {
			row.RoutingStatus = "single_turn_text"
			row.StreamingMode = "incremental"
		}
		for i := range row.Models {
			row.Models[i].InferenceAvailable = callable
		}
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		row.ErrorCode = "catalog_timeout"
		return
	}
	snapshot, err := live.Catalog(ctx, a.ID, force)
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
		row.Models = cloneModels(snapshot.Models)
		row.RoutingStatus = "single_turn_text"
	}
	if snapshot.Status != "" {
		row.Status = snapshot.Status
	}
	if snapshot.Streaming != "" {
		row.StreamingMode = snapshot.Streaming
	}
	if err != nil || snapshot.ErrorCode != "" {
		row.ErrorCode = snapshot.ErrorCode
		if row.ErrorCode == "" {
			row.ErrorCode = "catalog_failed"
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			row.ErrorCode = "catalog_timeout"
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
	callable := src.Inference()
	if callable {
		row.StreamingMode = "incremental"
	}
	for i := range row.Models {
		row.Models[i].InferenceAvailable = callable
	}
	return
}

func cloneModels(in []Model) []Model {
	out := make([]Model, len(in))
	copy(out, in)
	return out
}
