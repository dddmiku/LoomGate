package main

import (
	"context"
	"net/http"
	"path/filepath"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/providercatalog"
	"workbuddy2api/internal/providers/doubao"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/taskproviders"
	"workbuddy2api/internal/upstream"
)

type workbenchRuntime struct {
	engine *taskengine.Engine
	vault  *accountvault.Vault
	doubao *doubao.Client
}

// The isolated reconstruction runtime mounts this service while the legacy
// scheduler is stopped. Production remains on the released legacy runtime.
func openWorkbench(stateDir string, p *pool.Pool, up *upstream.Client) (*workbenchRuntime, http.Handler, error) {
	e, err := taskengine.Open(taskengine.Options{Path: filepath.Join(stateDir, "task-engine.json")})
	if err != nil {
		return nil, nil, err
	}
	for _, id := range []string{"workbuddy", "trae"} {
		if err = e.Register(id, &taskproviders.Accounts{Pool: p, Upstream: up, Provider: id}); err != nil {
			e.Close()
			return nil, nil, err
		}
	}
	vault, err := accountvault.Open(filepath.Join(stateDir, "provider-accounts"))
	if err != nil {
		e.Close()
		return nil, nil, err
	}
	client := doubao.New(doubao.VaultSessions{Vault: vault}, nil)
	if err = e.Register("doubao", &taskproviders.Doubao{Vault: vault, Client: client}); err != nil {
		e.Close()
		vault.Close()
		return nil, nil, err
	}
	workbench := &workbenchRuntime{engine: e, vault: vault, doubao: client}
	base := e.Handler()
	catalogs := providercatalog.New(providercatalog.Options{Accounts: vault, Doubao: client, LegacyCounts: func() map[string]providercatalog.Count {
		counts := map[string]providercatalog.Count{}
		for _, a := range p.List() {
			id := "workbuddy"
			if a.Realm == "trae" {
				id = "trae"
			} else if a.Realm != "cn" && a.Realm != "global" {
				continue
			}
			count := counts[id]
			count.Total++
			if !a.Disabled {
				count.Enabled++
			}
			counts[id] = count
		}
		return counts
	}})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if catalogs.Route(w, r) {
			return
		}
		base.ServeHTTP(w, r)
	})
	return workbench, handler, nil
}
func runWorkbench(ctx context.Context, w *workbenchRuntime) error {
	defer w.vault.Close()
	defer w.engine.Close()
	return w.engine.Run(ctx)
}
