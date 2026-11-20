package autoclaw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/providercatalog"
)

const (
	creditCacheTTL       = 60 * time.Second
	creditFailureBackoff = 12 * time.Second
)

type creditCacheEntry struct {
	value   providercatalog.CreditView
	at      time.Time
	retryAt time.Time
}

type creditFlight struct {
	done  chan struct{}
	value providercatalog.CreditView
	err   error
}

var autoClawCreditCache = struct {
	sync.Mutex
	rows     map[string]creditCacheEntry
	inflight map[string]*creditFlight
}{rows: map[string]creditCacheEntry{}, inflight: map[string]*creditFlight{}}

const maxCreditCacheEntries = 500
const maxConcurrentCreditRefreshes = 4

// Both manual and background refreshes share a process-wide upstream limit.
var creditRefreshSlots = make(chan struct{}, maxConcurrentCreditRefreshes)

// Credits reads the AutoClaw wallet and expiring-points endpoints. It is a
// short-lived read-through cache so loading the provider/account pages does not
// fan out to the upstream on every repaint. force bypasses the cache.
func (s Source) Credits(ctx context.Context, account string, force bool) (providercatalog.CreditView, error) {
	cacheKey := account
	if s.Vault != nil {
		cacheKey = s.Vault.OpaqueID("credit-cache", account)
	}
	if force {
		return s.refreshCreditSync(ctx, cacheKey, account)
	}
	return s.creditSnapshot(cacheKey, account), nil
}

func pendingCredit() providercatalog.CreditView {
	return providercatalog.CreditView{Supported: true, Pending: true, ErrorCode: "credit_pending"}
}

func (s Source) creditSnapshot(cacheKey, account string) providercatalog.CreditView {
	autoClawCreditCache.Lock()
	defer autoClawCreditCache.Unlock()
	entry, cached := autoClawCreditCache.rows[cacheKey]
	if cached && time.Now().Before(entry.retryAt) {
		return entry.value
	}
	if autoClawCreditCache.inflight[cacheKey] == nil {
		select {
		case creditRefreshSlots <- struct{}{}:
			flight := startCreditRefreshLocked(cacheKey)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				s.runCreditRefresh(ctx, cacheKey, account, flight, false)
			}()
		default:
			// A full background queue is dropped. Another snapshot can retry.
		}
	}
	if cached && entry.value.Remain != nil {
		return entry.value
	}
	return pendingCredit()
}

func (s Source) refreshCreditSync(ctx context.Context, cacheKey, account string) (providercatalog.CreditView, error) {
	if err := ctx.Err(); err != nil {
		return pendingCredit(), err
	}
	flight, started := beginCreditRefresh(cacheKey)
	if !started {
		select {
		case <-flight.done:
			return flight.value, flight.err
		case <-ctx.Done():
			return pendingCredit(), ctx.Err()
		}
	}
	select {
	case creditRefreshSlots <- struct{}{}:
		return s.runCreditRefresh(ctx, cacheKey, account, flight, true)
	case <-ctx.Done():
		return finishCreditRefresh(cacheKey, flight, pendingCredit(), ctx.Err())
	}
}

func (s Source) runCreditRefresh(ctx context.Context, cacheKey, account string, flight *creditFlight, manual bool) (providercatalog.CreditView, error) {
	defer func() { <-creditRefreshSlots }()
	if err := ctx.Err(); err != nil && manual {
		return finishCreditRefresh(cacheKey, flight, pendingCredit(), err)
	}
	value, err := s.fetchCredits(ctx, account)
	// A disconnected request or canceled slot wait says nothing about the upstream.
	if err != nil && manual && ctx.Err() != nil {
		return finishCreditRefresh(cacheKey, flight, pendingCredit(), ctx.Err())
	}
	return finishCreditRefresh(cacheKey, flight, value, err)
}

func (s Source) fetchCredits(ctx context.Context, account string) (providercatalog.CreditView, error) {
	if s.Vault == nil {
		return providercatalog.CreditView{Supported: true, ErrorCode: "autoclaw_vault_unavailable"}, errors.New("autoclaw_vault_unavailable")
	}
	p := &Provider{Vault: s.Vault, Client: s.Client}
	_, session, err := p.loadSession(account)
	if err != nil {
		return providercatalog.CreditView{Supported: true, ErrorCode: "autoclaw_session_unavailable"}, err
	}
	client := p.httpClient()
	value, status, err := fetchCreditWith(ctx, session.AccessToken, client, s.BaseURL)
	if status == http.StatusUnauthorized && value.Remain == nil && session.RefreshToken != "" {
		// RefreshToken is single-use; reuse the provider's durable intent path.
		if renewed, refreshErr := p.refreshAccount(ctx, account, session.RefreshToken); refreshErr == nil {
			value, status, err = fetchCreditWith(ctx, renewed.AccessToken, client, s.BaseURL)
		} else {
			err = refreshErr
		}
	}
	if value.Remain != nil {
		now := time.Now().UTC()
		value.FetchedAt = &now
		if err != nil {
			value.ErrorCode = "autoclaw_expiring_unavailable"
		}
		return value, err
	}
	if err != nil || status < 200 || status >= 300 {
		if err == nil {
			err = errors.New("autoclaw_credit_http")
		}
		value.Supported = true
		if value.ErrorCode == "" {
			value.ErrorCode = "autoclaw_credit_unavailable"
		}
		return value, err
	}
	return providercatalog.CreditView{Supported: true, ErrorCode: "autoclaw_credit_unavailable"}, errors.New("autoclaw_credit_response_invalid")
}

func fetchCreditWith(ctx context.Context, token string, client *http.Client, baseURL string) (providercatalog.CreditView, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	view := providercatalog.CreditView{Supported: true}
	total, status, err := fetchCreditEndpointWith(ctx, token, walletPath, client, baseURL)
	if status == http.StatusUnauthorized {
		return view, status, err
	}
	if err != nil {
		return view, status, err
	}
	var wallet struct {
		Code *int `json:"code"`
		Data struct {
			TotalBalance *int64 `json:"total_balance"`
		} `json:"data"`
	}
	if json.Unmarshal(total, &wallet) != nil || wallet.Code == nil || *wallet.Code != 0 || wallet.Data.TotalBalance == nil {
		return view, status, errors.New("autoclaw_credit_response_invalid")
	}
	view.Remain = wallet.Data.TotalBalance
	expiring, status, err := fetchCreditEndpointWith(ctx, token, expiringPath, client, baseURL)
	if err != nil {
		return view, status, err
	}
	var exp struct {
		Code *int `json:"code"`
		Data struct {
			Points *int64 `json:"expiring_points"`
			Text   string `json:"expiring_points_text"`
		} `json:"data"`
	}
	if json.Unmarshal(expiring, &exp) != nil || exp.Code == nil || *exp.Code != 0 {
		return view, status, errors.New("autoclaw_expiring_response_invalid")
	}
	view.Expiring = exp.Data.Points
	view.ExpiringText = exp.Data.Text
	if view.Expiring == nil {
		return view, status, errors.New("autoclaw_expiring_response_missing")
	}
	return view, status, nil
}

const (
	walletPath   = "/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw"
	expiringPath = "/agent-assetmgr/api/v1/points/expiring?biz_app_id=autoclaw"
)

func fetchCreditEndpointWith(ctx context.Context, token, path string, client *http.Client, baseURL string) ([]byte, int, error) {
	if baseURL == "" {
		baseURL = "https://" + apiBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return nil, 0, err
	}
	setHeaders(req, token, "")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, resp.StatusCode, errors.New("autoclaw_credit_http")
	}
	return raw, resp.StatusCode, nil
}

var _ providercatalog.CreditSource = Source{}

func beginCreditRefresh(key string) (*creditFlight, bool) {
	autoClawCreditCache.Lock()
	defer autoClawCreditCache.Unlock()
	if flight := autoClawCreditCache.inflight[key]; flight != nil {
		return flight, false
	}
	return startCreditRefreshLocked(key), true
}

func startCreditRefreshLocked(key string) *creditFlight {
	flight := &creditFlight{done: make(chan struct{})}
	autoClawCreditCache.inflight[key] = flight
	return flight
}

func finishCreditRefresh(key string, flight *creditFlight, value providercatalog.CreditView, err error) (providercatalog.CreditView, error) {
	autoClawCreditCache.Lock()
	defer autoClawCreditCache.Unlock()
	if !value.Pending {
		var ttl = creditCacheTTL
		if err != nil {
			ttl = creditFailureBackoff
			if value.ErrorCode == "" {
				value.ErrorCode = "autoclaw_credit_unavailable"
			}
			if old, exists := autoClawCreditCache.rows[key]; exists && value.Remain == nil && old.value.Remain != nil {
				code := value.ErrorCode
				value = old.value
				value.ErrorCode = code
			}
		}
		now := time.Now()
		storeCreditLocked(key, creditCacheEntry{value: value, at: now, retryAt: now.Add(ttl)})
	}
	flight.value, flight.err = value, err
	delete(autoClawCreditCache.inflight, key)
	close(flight.done)
	return value, err
}

func storeCreditLocked(key string, entry creditCacheEntry) {
	if _, exists := autoClawCreditCache.rows[key]; !exists && len(autoClawCreditCache.rows) >= maxCreditCacheEntries {
		var oldestKey string
		var oldest time.Time
		for candidate, cached := range autoClawCreditCache.rows {
			if oldest.IsZero() || cached.at.Before(oldest) {
				oldestKey, oldest = candidate, cached.at
			}
		}
		delete(autoClawCreditCache.rows, oldestKey)
	}
	autoClawCreditCache.rows[key] = entry
}
