package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"

	"workbuddy2api/internal/auth"
)

var taskIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func (c *Client) TaskCheckinStatus(ctx context.Context, a *auth.Auth) (*CheckinStatus, error) {
	data, err := c.taskBilling(ctx, a, c.checkinStatusPaths(a))
	if err != nil {
		return nil, err
	}
	var s CheckinStatus
	err = json.Unmarshal(data, &s)
	var fields map[string]json.RawMessage
	if err == nil {
		err = json.Unmarshal(data, &fields)
		var checked *bool
		if err == nil {
			err = json.Unmarshal(fields["today_checked_in"], &checked)
			if err == nil && checked == nil {
				err = errors.New("checkin_state_missing")
			}
		}
	}
	return &s, err
}
func (c *Client) TaskCheckin(ctx context.Context, a *auth.Auth) error {
	_, err := c.taskBilling(ctx, a, c.checkinMeterPaths(a))
	return err
}
func (c *Client) taskBilling(ctx context.Context, a *auth.Auth, paths []string) (json.RawMessage, error) {
	// The configured realm chooses the endpoint; no cross-provider fallback.
	return c.billingJSONContext(strictTaskContext(ctx), a, http.MethodPost, paths[0], map[string]any{})
}
func (c *Client) TaskCatalog(ctx context.Context, a *auth.Auth) ([]GrowthTask, error) {
	data, err := c.growthJSONContext(strictTaskContext(ctx), a, http.MethodGet, growthTasksPath, nil)
	if err != nil {
		return nil, err
	}
	var s struct {
		Tasks []GrowthTask `json:"tasks"`
	}
	err = json.Unmarshal(data, &s)
	if err == nil && s.Tasks == nil {
		err = errors.New("task_catalog_missing")
	}
	return s.Tasks, err
}
func (c *Client) TaskClaim(ctx context.Context, a *auth.Auth, key string) error {
	_, err := c.growthJSONContext(strictTaskContext(ctx), a, http.MethodPost, growthTaskClaimPrefix+url.PathEscape(key)+"/claim", map[string]any{})
	return err
}

// TaskGrowthAccept starts official progress tracking for one or more
// discovered growth tasks. The response is authoritative per code; an absent
// results array is not proof of acceptance.
func (c *Client) TaskGrowthAccept(ctx context.Context, a *auth.Auth, codes []string) (map[string]string, error) {
	if len(codes) == 0 || len(codes) > 32 {
		return nil, errors.New("task_accept_batch_invalid")
	}
	for _, code := range codes {
		if !taskIdentifier.MatchString(code) {
			return nil, errors.New("task_accept_batch_invalid")
		}
	}
	raw, err := c.growthJSONContext(strictTaskContext(ctx), a, http.MethodPost, growthTasksAcceptPath, map[string]any{"task_codes": codes})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			TaskCode string `json:"task_code"`
			Status   string `json:"status"`
			Message  string `json:"message"`
		} `json:"results"`
	}
	if !jsonObject(raw) || json.Unmarshal(raw, &resp) != nil || len(resp.Results) != len(codes) {
		return nil, errors.New("task_accept_receipt_invalid")
	}
	out := map[string]string{}
	for _, row := range resp.Results {
		if !taskIdentifier.MatchString(row.TaskCode) || row.Status == "" {
			return nil, errors.New("task_accept_receipt_invalid")
		}
		if _, duplicate := out[row.TaskCode]; duplicate {
			return nil, errors.New("task_accept_receipt_invalid")
		}
		out[row.TaskCode] = row.Status
	}
	return out, nil
}
