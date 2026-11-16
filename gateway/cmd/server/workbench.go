package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/providercatalog"
	"workbuddy2api/internal/providers/doubao"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/taskproviders"
	"workbuddy2api/internal/upstream"
)

type workbenchRuntime struct {
	engine     *taskengine.Engine
	vault      *accountvault.Vault
	doubao     *doubao.Client
	inference  map[string]inference.Provider
	connection *doubao.DesktopConnection
}

func workbenchInference(w *workbenchRuntime) map[string]inference.Provider {
	if w == nil {
		return nil
	}
	return w.inference
}

func workbenchAuthDrivers(w *workbenchRuntime) map[string]authflow.Driver {
	if w == nil {
		return nil
	}
	driver := &doubao.Passport{Save: func(ctx context.Context, id string, p doubao.StoredProfile) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rows, err := w.vault.List()
		if err != nil {
			return err
		}
		var revision uint64
		for _, a := range rows {
			if a.ID == id {
				if a.Provider != "doubao" {
					return errors.New("account_provider_conflict")
				}
				revision = a.Revision
			}
		}
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		defer func() {
			for i := range b {
				b[i] = 0
			}
		}()
		_, err = w.vault.Put(accountvault.Account{ID: id, Provider: "doubao", Enabled: true}, b, revision)
		return err
	}}
	if path := os.Getenv("LOOMGATE_DOUBAO_AUTH_PROFILE"); path != "" {
		driver.Bootstrap = func(ctx context.Context) (doubao.StoredProfile, error) {
			var p doubao.StoredProfile
			if ctx.Err() != nil {
				return p, ctx.Err()
			}
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
				return p, errors.New("bootstrap_unavailable")
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return p, errors.New("bootstrap_unavailable")
			}
			defer func() {
				for i := range b {
					b[i] = 0
				}
			}()
			if json.Unmarshal(b, &p) != nil {
				return p, errors.New("bootstrap_invalid")
			}
			return p, nil
		}
	}
	if w.connection != nil {
		w.connection.Fallback = driver
		return map[string]authflow.Driver{"doubao": w.connection}
	}
	return map[string]authflow.Driver{"doubao": driver}
}

// The isolated reconstruction runtime mounts this service while the legacy
// scheduler is stopped. Production remains on the released legacy runtime.
func openWorkbench(stateDir string, p *pool.Pool, up *upstream.Client, expiring ...time.Duration) (*workbenchRuntime, http.Handler, error) {
	var desktop doubao.CatalogRuntime
	if python, helper, port := os.Getenv("LOOMGATE_DOUBAO_DESKTOP_PYTHON"), os.Getenv("LOOMGATE_DOUBAO_DESKTOP_HELPER"), os.Getenv("LOOMGATE_DOUBAO_DESKTOP_PORT"); python != "" || helper != "" || port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil {
			return nil, nil, errors.New("desktop_configuration_invalid")
		}
		bridge, err := doubao.NewDesktopRuntime(python, helper, portNumber)
		if err != nil {
			return nil, nil, err
		}
		desktop = bridge
	}
	e, err := taskengine.Open(taskengine.Options{Path: filepath.Join(stateDir, "task-engine.json")})
	if err != nil {
		return nil, nil, err
	}
	for _, id := range []string{"workbuddy", "trae"} {
		var soon time.Duration
		if len(expiring) > 0 {
			soon = expiring[0]
		}
		adapter, adapterErr := taskproviders.NewAccounts(p, up, id, soon)
		if adapterErr != nil {
			e.Close()
			return nil, nil, adapterErr
		}
		if err = e.Register(id, adapter); err != nil {
			e.Close()
			return nil, nil, err
		}
	}
	vault, err := accountvault.Open(filepath.Join(stateDir, "provider-accounts"))
	if err != nil {
		e.Close()
		return nil, nil, err
	}
	client := doubao.NewWithRuntime(doubao.VaultSessions{Vault: vault}, desktop)
	if err = e.Register("doubao", &taskproviders.Doubao{Vault: vault, Client: client}); err != nil {
		e.Close()
		vault.Close()
		return nil, nil, err
	}
	workbench := &workbenchRuntime{engine: e, vault: vault, doubao: client}
	workbench.connection = &doubao.DesktopConnection{Vault: vault}
	if runtime, ok := desktop.(*doubao.CommandRuntime); ok {
		workbench.connection.Runtime = runtime
		workbench.inference = map[string]inference.Provider{"doubao": &doubao.TextProvider{Vault: vault, Client: client, Runtime: runtime}}
	}
	base := e.Handler()
	catalogs := providercatalog.New(providercatalog.Options{Accounts: vault, Doubao: client, DoubaoInference: len(workbench.inference) > 0, LegacyCounts: func() map[string]providercatalog.Count {
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
		if workbench.connection.Route(w, r) {
			return
		}
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
