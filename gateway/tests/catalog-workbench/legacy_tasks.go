package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

type legacyTasksTransport struct {
	mu                     sync.Mutex
	fixture                *fixtures
	reads, signins, claims int
	checked, claimed       map[string]bool
}

func (s *legacyTasksTransport) stats() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]int{"balance_reads": s.reads, "signins": s.signins, "claims": s.claims}
}
func (s *legacyTasksTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "fixture.invalid" {
		return nil, errors.New("fixture_network_blocked")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	identity := r.Header.Get("Authorization")
	var data any
	switch r.URL.Path {
	case "/v2/activity/growth/tasks":
		if s.fixture.mode.Load() == 1 {
			return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"synthetic_failure"}`)), Request: r}, nil
		}
		state := "completed"
		if s.claimed[identity] {
			state = "claimed"
		}
		data = map[string]any{"tasks": []any{map[string]any{"task_code": "fixture-growth", "title": "合成成长奖励", "accept_status": state, "reward_credit": 10}}}
	case "/v2/billing/meter/checkin-activity-status":
		data = map[string]any{"active": true, "today_checked_in": s.checked[identity]}
	case "/v2/billing/meter/daily-checkin":
		s.signins++
		s.checked[identity] = true
		data = map[string]any{}
	case "/activity/growth/tasks/fixture-growth/claim":
		s.claims++
		s.claimed[identity] = true
		data = map[string]any{}
	case "/v2/billing/meter/get-user-resource", "/billing/meter/get-user-resource":
		s.reads++
		data = map[string]any{"Response": map[string]any{"Data": map[string]any{"Accounts": []any{map[string]any{"CapacityRemain": 750, "CycleEndTime": time.Now().Add(time.Hour).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05")}}}}}
	default:
		return nil, errors.New("fixture_endpoint_blocked")
	}
	b, _ := json.Marshal(map[string]any{"code": 0, "data": data})
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
}
func seedLegacyTaskFixture(root string, p *pool.Pool, f *fixtures) (*upstream.Client, error) {
	transport := &legacyTasksTransport{fixture: f, checked: map[string]bool{}, claimed: map[string]bool{}}
	f.legacy = transport
	for _, entry := range []struct{ id, realm string }{{"1001", "cn"}, {"1002", "cn"}, {"1003", "global"}} {
		domain := "copilot.tencent.com"
		if entry.realm == "global" {
			domain = "www.workbuddy.ai"
		}
		data, _ := json.Marshal(map[string]any{"account": map[string]any{"uid": entry.id, "nickname": "Synthetic account"}, "auth": map[string]any{"accessToken": "synthetic-" + entry.id, "realm": entry.realm, "domain": domain, "expiresAt": time.Now().Add(time.Hour).Unix()}})
		account, err := auth.Parse(data)
		if err != nil {
			return nil, err
		}
		p.Add(account)
		if err = os.WriteFile(filepath.Join(root, "auths", "workbuddy-fixture-"+entry.id+".json"), data, 0600); err != nil {
			return nil, err
		}
	}
	return &upstream.Client{HTTP: &http.Client{Transport: transport}, GlobalEnabled: true, ChatBaseCN: "https://fixture.invalid", ChatBaseGlobal: "https://fixture.invalid", BillingBaseCN: "https://fixture.invalid", BillingBaseGlobal: "https://fixture.invalid"}, nil
}
