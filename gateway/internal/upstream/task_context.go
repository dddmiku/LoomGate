package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"workbuddy2api/internal/auth"
)

func (c *Client) TaskCheckinStatus(ctx context.Context, a *auth.Auth) (*CheckinStatus, error) {
	data, err := c.taskBilling(ctx, a, c.checkinStatusPaths(a))
	if err != nil {
		return nil, err
	}
	var s CheckinStatus
	err = json.Unmarshal(data, &s)
	return &s, err
}
func (c *Client) TaskCheckin(ctx context.Context, a *auth.Auth) error {
	_, err := c.taskBilling(ctx, a, c.checkinMeterPaths(a))
	return err
}
func (c *Client) taskBilling(ctx context.Context, a *auth.Auth, paths []string) (json.RawMessage, error) {
	// The configured realm chooses the endpoint; no cross-provider fallback.
	return c.billingJSONContext(ctx, a, http.MethodPost, paths[0], map[string]any{})
}
func (c *Client) TaskCatalog(ctx context.Context, a *auth.Auth) ([]GrowthTask, error) {
	data, err := c.growthJSONContext(ctx, a, http.MethodGet, growthTasksPath, nil)
	if err != nil {
		return nil, err
	}
	var s struct {
		Tasks []GrowthTask `json:"tasks"`
	}
	err = json.Unmarshal(data, &s)
	return s.Tasks, err
}
func (c *Client) TaskClaim(ctx context.Context, a *auth.Auth, key string) error {
	_, err := c.growthJSONContext(ctx, a, http.MethodPost, growthTaskClaimPrefix+url.PathEscape(key)+"/claim", map[string]any{})
	return err
}
