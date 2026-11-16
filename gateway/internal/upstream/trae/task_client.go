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
	if !totalOK || !usedOK || total < 0 || used < 0 || math.IsNaN(total) || math.IsNaN(used) || math.IsInf(total, 0) || math.IsInf(used, 0) {
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
		usage, known := pack["usage"].(map[string]any)
		if !known {
			return Credits{}, errors.New("trae_credit_pack_usage_missing")
		}
		if rawUsed, exists := usage["credits_amount"]; exists {
			n, valid := asFloat(rawUsed)
			if !valid || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
				return Credits{}, errors.New("trae_credit_pack_usage_invalid")
			}
		}
		parsed := parseCreditPack(pack)
		if math.IsNaN(parsed.Limit) || math.IsInf(parsed.Limit, 0) || math.IsNaN(parsed.Remaining) || math.IsInf(parsed.Remaining, 0) || parsed.Limit < 0 || parsed.Remaining < 0 {
			return Credits{}, errors.New("trae_credit_pack_invalid")
		}
		result.Packs = append(result.Packs, parsed)
	}
	return result, nil
}
