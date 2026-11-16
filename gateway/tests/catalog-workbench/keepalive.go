package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

type keepaliveFixture struct {
	mu    sync.Mutex
	path  string
	calls map[string]int
}

func (f *keepaliveFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "refresh-fixture.invalid" {
		return nil, errors.New("synthetic_network_blocked")
	}
	if r.Body != nil {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	kind := "workbuddy"
	if strings.HasSuffix(r.URL.Path, "ExchangeToken") {
		kind = "trae"
	}
	f.calls[kind]++
	raw, _ := json.Marshal(f.calls)
	if os.WriteFile(f.path, raw, 0600) != nil {
		return nil, errors.New("synthetic_state_failed")
	}
	var data any
	if kind == "trae" {
		data = map[string]any{"Result": map[string]any{"Token": fmt.Sprintf("synthetic-rotated-%d", f.calls[kind]), "RefreshToken": "synthetic-next", "TokenExpireAt": fmt.Sprint(time.Now().Add(2 * time.Hour).Unix())}}
	} else {
		data = map[string]any{"code": 0, "data": map[string]any{"accessToken": fmt.Sprintf("synthetic-rotated-%d", f.calls[kind]), "refreshToken": "synthetic-next", "expiresIn": 7200}}
	}
	raw, _ = json.Marshal(data)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
}
func (f *keepaliveFixture) stats(p *pool.Pool) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := []map[string]any{}
	for _, st := range p.List() {
		a := p.AuthByUID(st.UID)
		snapshot := a.Snapshot()
		disk, err := a.PersistedRevision()
		rows = append(rows, map[string]any{"id": st.UID, "disabled": st.Disabled, "saved": err == nil && disk == a.CredentialRevision(), "rotated": strings.HasPrefix(snapshot.AccessToken, "synthetic-rotated-"), "realm": snapshot.RealmStored()})
	}
	return map[string]any{"workbuddy_refreshes": f.calls["workbuddy"], "trae_refreshes": f.calls["trae"], "accounts": rows}
}
func seedKeepalive(root string, p *pool.Pool, f *fixtures, v *accountvault.Vault) (map[string]*taskproviders.Accounts, *keepaliveFixture, error) {
	transport := &keepaliveFixture{path: filepath.Join(root, "data", "synthetic-refresh-counts.json"), calls: map[string]int{}}
	if raw, err := os.ReadFile(transport.path); err == nil {
		if json.Unmarshal(raw, &transport.calls) != nil {
			return nil, nil, errors.New("synthetic_state_invalid")
		}
	}
	for _, row := range []struct{ id, realm, reason string }{{"4101", "cn", ""}, {"4102", "global", ""}, {"4103", "cn", pool.SessionDeadReason()}, {"4104", "cn", "manual"}, {"4105", "cn", pool.ReviewFailReason()}, {"4201", "trae", ""}} {
		path := filepath.Join(root, "auths", "workbuddy-refresh-"+row.id+".json")
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			domain := "www.codebuddy.cn"
			if row.realm == "global" {
				domain = "www.workbuddy.ai"
			}
			if row.realm == "trae" {
				domain = "api.trae.cn"
			}
			raw, _ = json.Marshal(map[string]any{"uid": row.id, "realm": row.realm, "domain": domain, "accessToken": "synthetic-original", "refreshToken": "synthetic-refresh", "expiresAt": time.Now().Add(time.Hour).Unix()})
		} else if err != nil {
			return nil, nil, err
		}
		a, err := auth.Parse(raw)
		if err != nil {
			return nil, nil, err
		}
		a.FilePath = path
		if err = a.SaveAtomic(); err != nil {
			return nil, nil, err
		}
		p.Add(a)
		if row.reason != "" && !strings.HasPrefix(a.Snapshot().AccessToken, "synthetic-rotated-") {
			p.Disable(row.id, row.reason)
		}
	}
	up := &upstream.Client{HTTP: &http.Client{Transport: transport}, GlobalEnabled: true, ChatBaseCN: "https://refresh-fixture.invalid", ChatBaseGlobal: "https://refresh-fixture.invalid"}
	client := &trae.TaskClient{HTTP: &http.Client{Transport: transport}, Origin: "https://refresh-fixture.invalid"}
	adapters := map[string]*taskproviders.Accounts{}
	for _, provider := range []string{"workbuddy", "trae"} {
		a, err := taskproviders.NewAccounts(p, up, provider, time.Hour, taskproviders.AccountsOptions{RefreshVault: v, Refresh: func(ctx context.Context, a *auth.Auth) (auth.TokenUpdate, error) {
			if a.Realm() == "trae" {
				return client.Refresh(ctx, a)
			}
			return up.TaskRefresh(ctx, a)
		}})
		if err != nil {
			return nil, nil, err
		}
		// This acceptance fixture isolates the production keepalive family, avoiding
		// unrelated synthetic activity endpoints. It retains the actual module.
		modules := a.Modules[:0]
		for _, m := range a.Modules {
			if m.Spec().ID == "keepalive" {
				modules = append(modules, m)
			}
		}
		a.Modules = modules
		adapters[provider] = a
	}
	return adapters, transport, nil
}
