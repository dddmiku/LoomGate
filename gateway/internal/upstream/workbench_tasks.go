package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"workbuddy2api/internal/auth"
)

type taskEnvelopeKey struct{}

func strictTaskContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, taskEnvelopeKey{}, true)
}
func (c *Client) doTaskJSON(req *http.Request) (json.RawMessage, error) {
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, errors.New("task_transport_failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("task_response_invalid")
	}
	if response.StatusCode != 200 {
		return nil, errors.New("task_http_rejected")
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil {
		return nil, errors.New("task_business_code_missing")
	}
	if *envelope.Code != 0 {
		return nil, errors.New("task_business_rejected")
	}
	return envelope.Data, nil
}

// These operations reuse the existing signed transport, but reject absent
// state fields instead of interpreting a changed schema as an eligible task.
func taskDecode(raw json.RawMessage, value any, required ...string) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return errors.New("task_response_invalid")
	}
	for _, key := range required {
		if fields[key] == nil || string(fields[key]) == "null" {
			return errors.New("task_response_missing")
		}
	}
	if json.Unmarshal(raw, value) != nil {
		return errors.New("task_response_invalid")
	}
	return nil
}
func (c *Client) taskGrowthRead(ctx context.Context, a *auth.Auth, path string, value any, required ...string) error {
	if a.Realm() != "cn" {
		return errors.New("task_realm_unsupported")
	}
	raw, err := c.growthJSONContext(strictTaskContext(ctx), a, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return taskDecode(raw, value, required...)
}
func (c *Client) taskGrowthWrite(ctx context.Context, a *auth.Auth, path string, body any) (json.RawMessage, error) {
	if a.Realm() != "cn" {
		return nil, errors.New("task_realm_unsupported")
	}
	return c.growthJSONContext(strictTaskContext(ctx), a, http.MethodPost, path, body)
}
func (c *Client) TaskBuddy(ctx context.Context, a *auth.Auth) (*Buddy, error) {
	var data map[string]json.RawMessage
	if err := c.taskGrowthRead(ctx, a, buddyInfoPath, &data); err != nil {
		return nil, err
	}
	raw, ok := data["buddy"]
	if !ok {
		return nil, errors.New("buddy_state_missing")
	}
	if string(raw) == "null" {
		return nil, nil
	}
	var buddy Buddy
	if taskDecode(raw, &buddy, "id") != nil || buddy.ID <= 0 {
		return nil, errors.New("buddy_state_invalid")
	}
	return &buddy, nil
}
func (c *Client) TaskTravel(ctx context.Context, a *auth.Auth) (TravelState, error) {
	var value TravelState
	err := c.taskGrowthRead(ctx, a, travelStatusPath, &value, "state", "daily_limit_reached", "record_id")
	if err == nil && (value.State != "idle" && value.State != "traveling" && value.State != "arrived" || value.RecordID < 0 || value.State != "idle" && value.RecordID == 0) {
		err = errors.New("travel_state_invalid")
	}
	return value, err
}
func (c *Client) TaskTravelConfig(ctx context.Context, a *auth.Auth) (GrowthTravelConfig, error) {
	var value GrowthTravelConfig
	err := c.taskGrowthRead(ctx, a, growthTravelConfigPath, &value, "locations")
	if err == nil {
		seen := map[int]bool{}
		if len(value.Locations) > 128 {
			err = errors.New("travel_config_invalid")
		}
		for _, l := range value.Locations {
			if l.ID <= 0 || seen[l.ID] || len(l.Name) > 256 || l.DurationHoursMin < 0 || l.DurationHoursMax < 0 || l.DurationHoursMin > l.DurationHoursMax {
				err = errors.New("travel_config_invalid")
			}
			seen[l.ID] = true
		}
	}
	return value, err
}
func (c *Client) TaskTravelDepart(ctx context.Context, a *auth.Auth, location int) error {
	if location <= 0 {
		return errors.New("travel_location_invalid")
	}
	_, err := c.taskGrowthWrite(ctx, a, travelDepartPath, map[string]any{"location_id": location})
	return err
}
func (c *Client) TaskTravelClaim(ctx context.Context, a *auth.Auth, record int64) error {
	if record <= 0 {
		return errors.New("travel_record_invalid")
	}
	raw, err := c.taskGrowthWrite(ctx, a, travelClaimPath, map[string]any{"record_id": record})
	if err != nil {
		return err
	}
	var value struct {
		Reward int64 `json:"reward_credit"`
	}
	if taskDecode(raw, &value, "reward_credit") != nil || value.Reward < 0 {
		return errors.New("travel_reward_unconfirmed")
	}
	return nil
}

type TaskStreak struct {
	GrowthStreakFull
	Redemption map[string]json.RawMessage `json:"redemption_status"`
}

func (c *Client) TaskStreak(ctx context.Context, a *auth.Auth) (TaskStreak, error) {
	var value TaskStreak
	var fields map[string]json.RawMessage
	if err := c.taskGrowthRead(ctx, a, streakPath, &fields, "streak"); err != nil {
		return value, err
	}
	var nested struct {
		Days int `json:"days"`
	}
	if taskDecode(fields["streak"], &nested, "days") != nil || nested.Days < 0 {
		return value, errors.New("streak_state_invalid")
	}
	raw, _ := json.Marshal(fields)
	err := taskDecode(raw, &value, "streak")
	return value, err
}
func (c *Client) TaskHeatmap(ctx context.Context, a *auth.Auth) (GrowthHeatmap, error) {
	var value GrowthHeatmap
	var fields map[string]json.RawMessage
	if err := c.taskGrowthRead(ctx, a, growthHeatmapPath, &fields, "today", "cells"); err != nil {
		return value, err
	}
	var today struct {
		Date string `json:"date"`
	}
	if taskDecode(fields["today"], &today, "date") != nil {
		return value, errors.New("calendar_today_missing")
	}
	var cells []json.RawMessage
	if json.Unmarshal(fields["cells"], &cells) != nil || cells == nil || len(cells) > 400 {
		return value, errors.New("calendar_cells_invalid")
	}
	for _, cell := range cells {
		var item GrowthHeatmapCell
		if taskDecode(cell, &item, "date", "score") != nil || item.Score < 0 {
			return value, errors.New("calendar_cell_invalid")
		}
	}
	raw, _ := json.Marshal(fields)
	err := taskDecode(raw, &value, "today", "cells")
	return value, err
}
func (c *Client) TaskRedeem(ctx context.Context, a *auth.Auth, tier, operation string) error {
	if tier == "" || len(tier) > 64 || strings.ContainsAny(tier, "/\\\x00") || operation == "" {
		return errors.New("redemption_identity_invalid")
	}
	_, err := c.taskGrowthWrite(ctx, a, redeemPath, map[string]any{"tier": tier, "client_token": operation})
	return err
}
func (c *Client) TaskMakeup(ctx context.Context, a *auth.Auth, date string) error {
	if len(date) != 10 {
		return errors.New("makeup_date_invalid")
	}
	_, err := c.taskGrowthWrite(ctx, a, growthMakeupUsePath, map[string]any{"target_date": date})
	return err
}

type TaskResources struct {
	Energy, Chances int
	Quota           GrowthBuddyQuota
}

func (c *Client) TaskResources(ctx context.Context, a *auth.Auth) (TaskResources, error) {
	var result TaskResources
	var energy, chances struct {
		Balance int `json:"balance"`
	}
	if err := c.taskGrowthRead(ctx, a, growthEnergyPath, &energy, "balance"); err != nil {
		return result, err
	}
	if err := c.taskGrowthRead(ctx, a, lotteryChancesPath, &chances, "balance"); err != nil {
		return result, err
	}
	if err := c.taskGrowthRead(ctx, a, growthBuddyQuotaPath, &result.Quota, "affordable", "max_open_count"); err != nil {
		return result, err
	}
	if energy.Balance < 0 || chances.Balance < 0 || result.Quota.Affordable < 0 || result.Quota.MaxOpenCount < 0 {
		return result, errors.New("resource_state_invalid")
	}
	result.Energy, result.Chances = energy.Balance, chances.Balance
	return result, nil
}

type TaskCompensation struct {
	Active  bool   `json:"active"`
	Claimed bool   `json:"claimed"`
	Credit  int64  `json:"credit_num"`
	End     string `json:"end_time"`
}

func (c *Client) TaskCompensation(ctx context.Context, a *auth.Auth) (TaskCompensation, error) {
	var result TaskCompensation
	if a.Realm() != "cn" {
		return result, errors.New("task_realm_unsupported")
	}
	raw, err := c.billingJSONContext(strictTaskContext(ctx), a, http.MethodPost, "/billing/meter/compensation-status", map[string]any{})
	if err != nil {
		return result, err
	}
	if err = taskDecode(raw, &result, "active", "claimed"); err != nil {
		return result, err
	}
	if len(result.End) > 128 || result.Active && (strings.TrimSpace(result.End) == "" || result.Credit <= 0) {
		return result, errors.New("compensation_state_invalid")
	}
	return result, nil
}
func (c *Client) TaskCompensationClaim(ctx context.Context, a *auth.Auth) error {
	if a.Realm() != "cn" {
		return errors.New("task_realm_unsupported")
	}
	_, err := c.billingJSONContext(strictTaskContext(ctx), a, http.MethodPost, "/billing/meter/claim-compensation", map[string]any{})
	return err
}
