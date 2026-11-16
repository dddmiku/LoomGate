package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
)

type lifecycleAccount struct {
	Travel      string
	Record      int64
	Daily       bool
	Redeemed    bool
	Made        []string
	Cards       int
	Compensated bool
}
type lifecycleFixture struct {
	path     string
	fixture  *fixtures
	accounts map[string]*lifecycleAccount
	counts   map[string]int
}

func openLifecycleFixture(root string, f *fixtures) *lifecycleFixture {
	l := &lifecycleFixture{path: filepath.Join(root, "data", "lifecycle-synthetic.json"), fixture: f, accounts: map[string]*lifecycleAccount{}, counts: map[string]int{}}
	if raw, err := os.ReadFile(l.path); err == nil {
		var saved struct {
			Accounts map[string]*lifecycleAccount
			Counts   map[string]int
		}
		if json.Unmarshal(raw, &saved) != nil {
			panic("fixture_lifecycle_invalid")
		}
		l.accounts, l.counts = saved.Accounts, saved.Counts
	}
	return l
}
func (l *lifecycleFixture) respond(r *http.Request, id string) (any, bool, error) {
	a := l.accounts[id]
	if a == nil {
		a = &lifecycleAccount{Travel: "idle", Made: []string{}, Cards: 3}
		l.accounts[id] = a
	}
	mode := l.fixture.mode.Load()
	day := "2099-01-03"
	end := "2099-02-01 00:00:00"
	if mode == 12 {
		day = "2099-02-03"
		end = "2099-03-01 00:00:00"
	}
	var data any
	action := ""
	switch r.URL.Path {
	case "/activity/growth/buddy/info":
		data = map[string]any{"buddy": map[string]any{"id": 17, "name": "合成 Buddy"}}
	case "/activity/growth/buddy/travel/status":
		if mode == 10 && a.Travel == "traveling" {
			a.Travel = "arrived"
		}
		data = map[string]any{"state": a.Travel, "record_id": a.Record, "daily_limit_reached": a.Daily, "reward_credit": 7}
	case "/activity/growth/buddy/travel/config":
		data = map[string]any{"locations": []any{map[string]any{"id": 73, "name": "合成目的地", "duration_hours_min": 1, "duration_hours_max": 2}}}
	case "/activity/growth/buddy/travel/depart":
		var body struct {
			Location int `json:"location_id"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Location != 73 {
			return nil, true, errors.New("synthetic_location_mismatch")
		}
		if mode != 11 {
			a.Travel = "traveling"
			a.Record = 412
			a.Daily = true
		}
		action = "travel_departures"
		data = map[string]any{}
	case "/activity/growth/buddy/travel/claim":
		var body struct {
			Record int64 `json:"record_id"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Record != 412 {
			return nil, true, errors.New("synthetic_record_mismatch")
		}
		if mode != 11 {
			a.Travel = "idle"
			a.Record = 0
		}
		action = "travel_claims"
		data = map[string]any{"reward_credit": 7}
	case "/activity/growth/streak":
		status := "available"
		if a.Redeemed {
			status = "claimed"
		}
		data = map[string]any{"launch_date": "2099-01-01", "streak": map[string]any{"days": 12, "makeup_dates": a.Made}, "makeup_cards": map[string]any{"balance": a.Cards}, "redemption_status": map[string]any{"tiers": []any{map[string]any{"tier": "fixture-future", "days": 9, "credit": 23}}, "tier_fixture-future_status": status}}
	case "/activity/growth/heatmap":
		data = map[string]any{"today": map[string]any{"date": day}, "cells": []any{map[string]any{"date": "2099-01-01", "score": 0}, map[string]any{"date": "2099-01-02", "score": 0}, map[string]any{"date": day, "score": 1}}}
	case "/activity/growth/redeem":
		var body struct {
			Tier  string `json:"tier"`
			Token string `json:"client_token"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Tier != "fixture-future" || body.Token == "" {
			return nil, true, errors.New("synthetic_redemption_mismatch")
		}
		if mode != 11 {
			a.Redeemed = true
		}
		action = "redemptions"
		data = map[string]any{}
	case "/activity/growth/makeup-cards/use":
		var body struct {
			Date string `json:"target_date"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Date != "2099-01-02" {
			return nil, true, errors.New("synthetic_makeup_mismatch")
		}
		if mode != 11 {
			a.Cards--
			a.Made = append(a.Made, body.Date)
		}
		action = "makeups"
		data = map[string]any{"makeup_cards": a.Cards}
	case "/activity/growth/energy":
		data = map[string]any{"balance": 11}
	case "/activity/growth/lottery/chances":
		data = map[string]any{"balance": 4}
	case "/activity/growth/buddy/quota":
		data = map[string]any{"affordable": 2, "max_open_count": 1}
	case "/billing/meter/compensation-status":
		data = map[string]any{"active": true, "claimed": a.Compensated, "credit_num": 31, "end_time": end}
	case "/billing/meter/claim-compensation":
		if mode != 11 {
			a.Compensated = true
		}
		action = "compensation_claims"
		data = map[string]any{}
	default:
		return nil, false, nil
	}
	if action != "" {
		l.counts[action]++
	}
	raw, _ := json.Marshal(struct {
		Accounts map[string]*lifecycleAccount
		Counts   map[string]int
	}{l.accounts, l.counts})
	if os.WriteFile(l.path, raw, 0600) != nil {
		return nil, true, errors.New("synthetic_state_write_failed")
	}
	return data, true, nil
}
