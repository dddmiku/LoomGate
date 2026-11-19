package main

import (
	"context"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/providercatalog"
	"workbuddy2api/internal/providers/autoclaw"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/taskproviders"
	"workbuddy2api/internal/upstream"
)

type workbenchRuntime struct {
	engine    *taskengine.Engine
	vault     *accountvault.Vault
	taskVault *accountvault.Vault
	inference map[string]inference.Provider
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
	// No provider currently ships an in-gateway login driver: WorkBuddy and
	// Trae authenticate through the legacy pool, and AutoClaw accounts are
	// imported from an existing AutoClaw credential blob. Returning an empty
	// map keeps the panel's login buttons honest instead of advertising a flow
	// that cannot succeed.
	return map[string]authflow.Driver{}
}

// The isolated reconstruction runtime mounts this service while the legacy
// scheduler is stopped. Production remains on the released legacy runtime.
// resolveManagedPython returns the absolute interpreter path required by the
// managed bridge. A launcher may inherit a bare "python"/"python3"/"py" name
// from its own environment; that must be resolved here instead of failing
// desktop startup with desktop_configuration_invalid.
func resolveManagedPython(configured string) string {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		if filepath.IsAbs(configured) {
			return configured
		}
		if resolved, err := exec.LookPath(configured); err == nil {
			if absolute, err := filepath.Abs(resolved); err == nil {
				return absolute
			}
		}
		return ""
	}
	python, _ := exec.LookPath("python.exe")
	if python == "" {
		python, _ = exec.LookPath("python3")
	}
	if python == "" {
		python, _ = exec.LookPath("python")
	}
	if absolute, err := filepath.Abs(python); err == nil {
		return absolute
	}
	return ""
}

func openWorkbench(stateDir string, p *pool.Pool, up *upstream.Client, expiring ...time.Duration) (*workbenchRuntime, http.Handler, error) {
	return openWorkbenchFor(stateDir, p, up, runtime.GOOS, expiring...)
}

// openWorkbenchFor keeps an explicit host platform parameter so callers that
// used to test the Doubao desktop wiring contract still compile; the current
// provider set is transport-independent and ignores it.
func openWorkbenchFor(stateDir string, p *pool.Pool, up *upstream.Client, goos string, expiring ...time.Duration) (*workbenchRuntime, http.Handler, error) {
	_ = goos
	e, err := taskengine.Open(taskengine.Options{Path: filepath.Join(stateDir, "task-engine.json"), AllowTaskTimeout: true})
	if err != nil {
		return nil, nil, err
	}
	taskVault, err := accountvault.Open(filepath.Join(stateDir, "task-provider-state"))
	if err != nil {
		e.Close()
		return nil, nil, err
	}
	keepTaskVault := false
	defer func() {
		if !keepTaskVault {
			taskVault.Close()
		}
	}()
	traeState, err := taskproviders.NewTraeTaskStore(taskVault)
	if err != nil {
		e.Close()
		return nil, nil, err
	}
	for _, id := range []string{"workbuddy", "trae"} {
		var soon time.Duration
		if len(expiring) > 0 {
			soon = expiring[0]
		}
		adapter, adapterErr := taskproviders.NewAccounts(p, up, id, soon, taskproviders.AccountsOptions{TraeStore: traeState, RefreshVault: taskVault, SpendVault: taskVault})
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
	autoclawProvider := &autoclaw.Provider{Vault: vault}
	workbench := &workbenchRuntime{
		engine:    e,
		vault:     vault,
		taskVault: taskVault,
		inference: map[string]inference.Provider{"autoclaw": autoclawProvider},
	}
	base := e.Handler()
	catalogs := providercatalog.New(providercatalog.Options{
		Accounts: vault,
		Sources:  []providercatalog.ProviderSource{autoclaw.Source{Vault: vault}},
		LegacyProviders: []providercatalog.Provider{
			{ID: "workbuddy", Name: "WorkBuddy", CatalogScope: "provider", Inference: true, Login: true},
			{ID: "trae", Name: "Trae", CatalogScope: "provider", Inference: true, Login: true},
		},
		LegacyCounts: func() map[string]providercatalog.Count {
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
		},
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if catalogs.Route(w, r) {
			return
		}
		base.ServeHTTP(w, r)
	})
	keepTaskVault = true
	return workbench, handler, nil
}

// Import before Run starts. The plan set ships with the build, inside the task
// files themselves (see taskproviders.BuiltinSchedules); this server never
// derives plans from an old config file. Every release carries the same fixed
// list, so an upgrade adds what is missing and never rewrites what the operator
// edited. The config.Schedule section is now only read by the legacy scheduler.
func openWorkbenchWithBuiltinSchedules(stateDir string, p *pool.Pool, up *upstream.Client, expiring ...time.Duration) (*workbenchRuntime, http.Handler, error) {
	workbench, handler, err := openWorkbench(stateDir, p, up, expiring...)
	if err != nil {
		return nil, nil, err
	}
	count, importErr := workbench.engine.ImportDefaultSchedulesOnce("loomgate-builtin-v1", taskproviders.BuiltinSchedules())
	if importErr != nil {
		log.Printf("[task-engine] 内置计划补齐失败（下次启动会重试）：%v", importErr)
	} else if count > 0 {
		log.Printf("[task-engine] 已补齐 %d 条内置计划，已有计划保留，新计划从未来时刻开始", count)
	}
	return workbench, handler, nil
}

func runWorkbench(ctx context.Context, w *workbenchRuntime) error {
	if w.taskVault != nil {
		defer w.taskVault.Close()
	}
	defer w.vault.Close()
	defer w.engine.Close()
	return w.engine.Run(ctx)
}
