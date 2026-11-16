package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"workbuddy2api/internal/auth"
)

// TaskClient uses the same official endpoints/header contract, with independent
// transport injection and strict task eligibility/credit response validation.
type TaskClient struct {
	HTTP   *http.Client
	Origin string
}

func (c *TaskClient) post(ctx context.Context, a *auth.Auth, path string, body any) (map[string]any, error) {
	if a == nil || a.Realm() != Realm {
		return nil, errors.New("trae_task_account_invalid")
	}
	if body == nil {
		body = map[string]any{}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("trae_task_input_invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	origin := c.Origin
	if origin == "" {
		origin = TraeAPIHost
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(origin, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("trae_task_request_invalid")
	}
	req.Header = checkinHeaders(a, 0)
	client := http.Client{Transport: chatTransport()}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("trae_task_transport_failed")
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("trae_task_response_invalid")
	}
	if resp.StatusCode != 200 {
		return nil, errors.New("trae_task_http_rejected")
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil || data == nil {
		return nil, errors.New("trae_task_schema_invalid")
	}
	code, ok := asFloat(data["code"])
	if !ok || math.IsNaN(code) || math.IsInf(code, 0) {
		return nil, errors.New("trae_task_code_missing")
	}
	switch code {
	case 0:
		return data, nil
	case RateLimitCode:
		return nil, &RateLimitError{}
	case DeviceAlreadyCheckedCode:
		return nil, &DeviceCheckedError{}
	case AuthFailedCode:
		return nil, &AuthError{}
	}
	return nil, errors.New("trae_task_business_rejected")
}
func (c *TaskClient) Status(ctx context.Context, a *auth.Auth) (CheckinStatus, error) {
	data, err := c.post(ctx, a, checkinStatusPath, nil)
	if err != nil {
		return CheckinStatus{}, err
	}
	enabled, ok := data["enable"].(bool)
	if !ok {
		return CheckinStatus{}, errors.New("trae_task_enabled_missing")
	}
	checked, ok := data["checked_in"].(bool)
	if !ok {
		return CheckinStatus{}, errors.New("trae_task_checked_missing")
	}
	s := CheckinStatus{Enabled: enabled, CheckedIn: checked}
	if n, ok := asFloat(data["credits"]); ok && n >= 0 && !math.IsInf(n, 0) && !math.IsNaN(n) {
		s.Credits = n
	}
	return s, nil
}
func (c *TaskClient) Claim(ctx context.Context, a *auth.Auth) (CheckinStatus, error) {
	data, err := c.post(ctx, a, checkinClaimPath, nil)
	if err != nil {
		return CheckinStatus{}, err
	}
	s := CheckinStatus{CheckedIn: true}
	if n, ok := asFloat(data["credits"]); ok && n >= 0 && !math.IsInf(n, 0) && !math.IsNaN(n) {
		s.Credits = n
	}
	return s, nil
}
func (c *TaskClient) Credits(ctx context.Context, a *auth.Auth) (Credits, error) {
	data, err := c.post(ctx, a, entUsagePath, map[string]any{"require_usage": true, "req_source": 1})
	if err != nil {
		return Credits{}, err
	}
	summary, ok := data["usage_summary"].(map[string]any)
	if !ok {
		return Credits{}, errors.New("trae_credit_summary_missing")
	}
	total, totalOK := asFloat(summary["total_amount"])
	used, usedOK := asFloat(summary["consumed_amount"])
	if !totalOK || !usedOK || !validTaskCredits(total) || !validTaskCredits(used) {
		return Credits{}, errors.New("trae_credit_summary_invalid")
	}
	packs, ok := data["user_entitlement_pack_list"].([]any)
	if !ok || len(packs) > 4096 {
		return Credits{}, errors.New("trae_credit_packs_missing")
	}
	result := Credits{Total: total, Consumed: used, Remaining: math.Max(0, total-used)}
	for _, raw := range packs {
		pack, ok := raw.(map[string]any)
		if !ok {
			return Credits{}, errors.New("trae_credit_pack_invalid")
		}
		parsed, err := strictCreditPack(pack)
		if err != nil {
			return Credits{}, err
		}
		result.Packs = append(result.Packs, parsed)
	}
	return result, nil
}

func validTaskCredits(n float64) bool {
	return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n < float64(math.MaxInt64)
}

// These raw fields are the observed entitlement contract, rather than the
// legacy display parser's defaults. An explicit empty usage object means zero
// consumed; absent, null or renamed usage is not evidence of an unused pack.
func strictCreditPack(pack map[string]any) (CreditPack, error) {
	invalid := errors.New("trae_credit_pack_invalid")
	base, ok := pack["entitlement_base_info"].(map[string]any)
	if !ok {
		return CreditPack{}, invalid
	}
	extra, ok := base["product_extra"].(map[string]any)
	if !ok {
		return CreditPack{}, invalid
	}
	pe, ok := extra["package_extra"].(map[string]any)
	if !ok {
		return CreditPack{}, invalid
	}
	quota, ok := pe["quota"].(map[string]any)
	if !ok {
		return CreditPack{}, invalid
	}
	limit, ok := asFloat(quota["credits_limit"])
	if !ok || !validTaskCredits(limit) {
		return CreditPack{}, invalid
	}
	usage, ok := pack["usage"].(map[string]any)
	if !ok {
		return CreditPack{}, invalid
	}
	used := float64(0)
	if len(usage) != 0 {
		used, ok = asFloat(usage["credits_amount"])
		if !ok || !validTaskCredits(used) {
			return CreditPack{}, invalid
		}
	}
	// end_time=0 plus top-level expire_time is another observed shape. Explicit
	// zero with no fallback means no expiry; omitted or malformed time is unknown.
	end, hasEnd := base["end_time"]
	fallback, hasFallback := pack["expire_time"]
	validTime := func(raw any) (int64, bool) {
		n, ok := asFloat(raw)
		if !ok || n < 0 || n > 253402300799 || math.Trunc(n) != n {
			return 0, false
		}
		return int64(n), true
	}
	var stamp int64
	if hasEnd {
		stamp, ok = validTime(end)
		if !ok {
			return CreditPack{}, invalid
		}
	}
	if hasFallback {
		f, good := validTime(fallback)
		if !good {
			return CreditPack{}, invalid
		}
		if stamp == 0 {
			stamp = f
		}
	}
	if !hasEnd && !hasFallback {
		return CreditPack{}, invalid
	}
	parsed := parseCreditPack(pack)
	parsed.Limit = limit
	parsed.Remaining = math.Max(0, limit-used)
	parsed.EndTime = stamp
	return parsed, nil
}

// TaskExpiry validates a complete snapshot before the pool adopts any part of
// it. Sum in the bounded remaining domain so large packs cannot overflow int64.
func TaskExpiry(credits Credits, window time.Duration, now time.Time) (int64, error) {
	if !validTaskCredits(credits.Total) || !validTaskCredits(credits.Consumed) || !validTaskCredits(credits.Remaining) {
		return 0, errors.New("trae_credit_snapshot_invalid")
	}
	remaining := int64(credits.Remaining)
	var bucket int64
	for _, p := range credits.Packs {
		if !validTaskCredits(p.Limit) || !validTaskCredits(p.Remaining) || p.Remaining > p.Limit || p.EndTime < 0 || p.EndTime > 253402300799 {
			return 0, errors.New("trae_credit_pack_invalid")
		}
		if window > 0 && p.EndTime > now.Unix() && !time.Unix(p.EndTime, 0).After(now.Add(window)) {
			bucket += min(int64(p.Remaining), remaining-bucket)
		}
	}
	return bucket, nil
}
