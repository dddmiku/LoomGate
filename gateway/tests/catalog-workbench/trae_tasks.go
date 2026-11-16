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
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskproviders"
	"workbuddy2api/internal/upstream/trae"
)

type traeSyntheticState struct {
	Checked                 bool
	Claims, Statuses, Reads int
}
type traeTaskFixture struct {
	mu      sync.Mutex
	fixture *fixtures
	state   traeSyntheticState
	path    string
}

func (f *traeTaskFixture) now() time.Time {
	now := time.Date(2099, 1, 3, 4, 0, 0, 0, time.UTC)
	if f.fixture.mode.Load() == 21 {
		return now.Add(61 * time.Second)
	}
	return now
}
func (f *traeTaskFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "trae-fixture.invalid" {
		return nil, errors.New("synthetic_network_blocked")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	var data any
	mode := f.fixture.mode.Load()
	switch {
	case strings.HasSuffix(r.URL.Path, "/claim"):
		f.state.Claims++
		if mode == 20 {
			data = map[string]any{"code": 9074}
		} else if mode == 23 {
			data = map[string]any{"code": 9095}
		} else {
			f.state.Checked = true
			data = map[string]any{"code": 0, "credits": 100}
		}
	case strings.HasSuffix(r.URL.Path, "/status"):
		f.state.Statuses++
		data = map[string]any{"code": 0, "enable": true, "checked_in": f.state.Checked}
	default:
		f.state.Reads++
		data = map[string]any{"code": 0, "usage_summary": map[string]any{"total_amount": 1000, "consumed_amount": 200}, "user_entitlement_pack_list": []any{map[string]any{"entitlement_base_info": map[string]any{"end_time": f.now().Add(time.Hour).Unix(), "product_extra": map[string]any{"package_extra": map[string]any{"quota": map[string]any{"credits_limit": 500}}}}, "usage": map[string]any{"credits_amount": 100}}}}
		if mode == 22 {
			data = map[string]any{"code": 0}
		}
	}
	raw, _ := json.Marshal(f.state)
	if os.WriteFile(f.path, raw, 0600) != nil {
		return nil, errors.New("synthetic_state_failed")
	}
	raw, _ = json.Marshal(data)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
}
func seedTraeTasks(root string, p *pool.Pool, f *fixtures, v *accountvault.Vault) (*taskproviders.Accounts, *traeTaskFixture, error) {
	fixture := &traeTaskFixture{fixture: f, path: filepath.Join(root, "data", "trae-upstream-synthetic.json")}
	if raw, err := os.ReadFile(fixture.path); err == nil {
		if json.Unmarshal(raw, &fixture.state) != nil {
			return nil, nil, errors.New("synthetic_state_invalid")
		}
	}
	raw, _ := json.Marshal(map[string]any{"account": map[string]any{"uid": "3001", "nickname": "Synthetic Trae"}, "auth": map[string]any{"realm": "trae", "domain": "api.trae.cn", "accessToken": "synthetic-trae-only", "expiresAt": fixture.now().Add(time.Hour).Unix()}})
	a, err := auth.Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	p.Add(a)
	if err = os.WriteFile(filepath.Join(root, "auths", "workbuddy-trae-fixture.json"), raw, 0600); err != nil {
		return nil, nil, err
	}
	store, err := taskproviders.NewTraeTaskStore(v)
	if err != nil {
		return nil, nil, err
	}
	adapter, err := taskproviders.NewAccounts(p, nil, "trae", 2*time.Hour, taskproviders.AccountsOptions{TraeStore: store, TraeClient: &trae.TaskClient{HTTP: &http.Client{Transport: fixture}, Origin: "https://trae-fixture.invalid"}, Now: fixture.now})
	return adapter, fixture, err
}
