package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
	"workbuddy2api/internal/config"
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
	taskVault  *accountvault.Vault
	doubao     *doubao.Client
	inference  map[string]inference.Provider
	connection *doubao.DesktopConnection
	managed    *doubao.ManagedCommand
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
		if w.managed != nil {
			return map[string]authflow.Driver{"doubao": &doubao.ManagedConnection{Binding: w.connection, Client: w.managed}}
		}
		w.connection.Fallback = driver
		return map[string]authflow.Driver{"doubao": w.connection}
	}
	return map[string]authflow.Driver{"doubao": driver}
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

// doubaoManagedRuntimeSupported reports whether this gateway may own the
// official Doubao client's lifecycle. tools/doubao-runtime/managed.py starts a
// Windows desktop client and rejects every other operating system with
// desktop_management_platform_unsupported. Constructing that runtime elsewhere
// would advertise QR/SMS/desktop methods that can never succeed, so the
// managed branch is Windows-only. The explicit loopback-port bridge stays
// available on every OS because it does not launch the client itself.
func doubaoManagedRuntimeSupported(goos string) bool {
	return goos == "windows"
}

// findDoubaoRuntimeHelper locates the packaged bridge helper. Both the desktop
// bundle and the Linux runtime place it in <root>/doubao-runtime/bridge.py.
func findDoubaoRuntimeHelper(dirs ...string) string {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, "doubao-runtime", "bridge.py")
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

func openWorkbench(stateDir string, p *pool.Pool, up *upstream.Client, expiring ...time.Duration) (*workbenchRuntime, http.Handler, error) {
	return openWorkbenchFor(stateDir, p, up, runtime.GOOS, expiring...)
}

// openWorkbenchFor is openWorkbench with an explicit host platform so the
// Doubao wiring contract can be regression-tested for a target OS on any host.
func openWorkbenchFor(stateDir string, p *pool.Pool, up *upstream.Client, goos string, expiring ...time.Duration) (*workbenchRuntime, http.Handler, error) {
	var desktop doubao.CatalogRuntime
	var managed *doubao.ManagedCommand
	python, helper, port := os.Getenv("LOOMGATE_DOUBAO_DESKTOP_PYTHON"), os.Getenv("LOOMGATE_DOUBAO_DESKTOP_HELPER"), os.Getenv("LOOMGATE_DOUBAO_DESKTOP_PORT")
	if python == "" && helper == "" {
		binary, _ := os.Executable()
		helper = findDoubaoRuntimeHelper(os.Getenv("WB2API_RUNTIME_DIR"), filepath.Dir(binary))
	}
	if helper != "" && python == "" {
		python = resolveManagedPython(os.Getenv("WB2A_PYTHON"))
		if python == "" {
			helper = ""
		}
	}
	if python != "" || helper != "" || port != "" {
		if port == "" {
			if !doubaoManagedRuntimeSupported(goos) {
				// No owned official client can be launched here. Leave the
				// runtime nil so /connections/providers reports the Doubao
				// methods as unavailable with an accurate reason instead of
				// accepting a login that managed.py will always reject.
				log.Printf("[doubao] 当前平台 %s 无法启动官方客户端；如需已有会话，请显式配置 LOOMGATE_DOUBAO_DESKTOP_PORT 回环调试端口，或改用桌面版网关。", goos)
			} else {
				root := filepath.Join(stateDir, "official-client")
				var err error
				managed, err = doubao.NewManagedCommand(python, filepath.Join(filepath.Dir(helper), "managed.py"), root, os.Getenv("LOOMGATE_DOUBAO_EXECUTABLE"))
				if err != nil {
					return nil, nil, err
				}
				bridge, err := doubao.NewManagedDesktopRuntime(python, helper, managed)
				if err != nil {
					return nil, nil, err
				}
				desktop = bridge
			}
		} else {
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
	}
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
	client := doubao.NewWithRuntime(doubao.VaultSessions{Vault: vault}, desktop)
	doubaoTasks := &taskproviders.Doubao{Vault: vault, Client: client}
	if err = e.Register("doubao", doubaoTasks); err != nil {
		e.Close()
		vault.Close()
		return nil, nil, err
	}
	workbench := &workbenchRuntime{engine: e, vault: vault, taskVault: taskVault, doubao: client, managed: managed}
	workbench.connection = &doubao.DesktopConnection{Vault: vault}
	if runtime, ok := desktop.(*doubao.CommandRuntime); ok {
		workbench.connection.Runtime = runtime
		workbench.inference = map[string]inference.Provider{"doubao": &doubao.TextProvider{Vault: vault, Client: client, Runtime: runtime, Dialogues: &doubao.DialogueStore{Vault: taskVault}}}
		doubaoTasks.Work = &taskproviders.DoubaoWork{Vault: taskVault, Provider: workbench.inference["doubao"].(*doubao.TextProvider)}
	}
	base := e.Handler()
	catalogs := providercatalog.New(providercatalog.Options{Accounts: vault, Doubao: client, DoubaoInference: len(workbench.inference) > 0, DoubaoMultiTurn: len(workbench.inference) > 0, DoubaoBinding: workbench.connection.DisplayBinding, LegacyCounts: func() map[string]providercatalog.Count {
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
	keepTaskVault = true
	return workbench, handler, nil
}

// Import before Run starts. This version repairs already-imported workspaces
// once, without changing their plans or inspecting/executing upstream tasks.
func openWorkbenchWithLegacySchedules(stateDir string, p *pool.Pool, up *upstream.Client, schedule config.Schedule, expiring ...time.Duration) (*workbenchRuntime, http.Handler, error) {
	workbench, handler, err := openWorkbench(stateDir, p, up, expiring...)
	if err != nil {
		return nil, nil, err
	}
	candidates := legacySchedulesFromConfig(schedule)
	candidates = inheritGrowthSchedule(candidates, workbench.engine.Schedules())
	// The original five import IDs may be missing because the user deleted
	// them. A v1 marker alone cannot distinguish that from a missing old plan.
	count, importErr := workbench.engine.ImportDefaultSchedulesOnce("legacy-defaults-v2", candidates)
	if importErr != nil {
		log.Printf("[task-engine] 默认计划补齐失败（下次启动会重试）：%v", importErr)
	} else if count > 0 {
		log.Printf("[task-engine] 已补齐 %d 条默认计划，已有计划保留，新计划从未来时刻开始", count)
	}
	return workbench, handler, nil
}

func legacyTaskKey(module string) string {
	switch module {
	case "growth", "redeem", "travel":
		return module + "/"
	}
	// makeup carries the date in PeriodKey; its actual key has no slash.
	return module
}

// Only the read-only balance refresh may stay enabled. Every default plan that
// can write upstream is imported disabled and records the old switch in
// LegacyEnabled, so the console can distinguish "migrated off" from "was off";
// turning a write plan on is an explicit operator action after upgrade.
//
// The legacy growth job was a dependency-aware pipeline. Its stages surface as
// independent plans at the same hours, staggered so one account is not hit by
// six writes at the same instant, and disabled until the operator enables them.
func legacySchedulesFromConfig(schedule config.Schedule) []taskengine.Schedule {
	var out []taskengine.Schedule
	for _, item := range []struct {
		id, provider, key string
		enabled           bool
		hours             []int
		minute            int
	}{
		{"legacy-checkin", "workbuddy", "checkin", schedule.CheckinEnabled, schedule.CheckinHours, 0},
		{"legacy-trae-check", "trae", "checkin", schedule.TraeCheckEnabled, schedule.TraeCheckHours, 5},
		{"legacy-balance", "workbuddy", "balance", schedule.BalanceEnabled, schedule.BalanceHours, 0},
		{"legacy-travel", "workbuddy", "travel", schedule.TravelEnabled, schedule.TravelHours, 0},
		{"legacy-keepalive", "workbuddy", "keepalive", schedule.KeepaliveEnabled, schedule.KeepaliveHours, 0},
		{"legacy-trae-keepalive", "trae", "keepalive", schedule.KeepaliveEnabled, schedule.KeepaliveHours, 0},
		{"legacy-growth", "workbuddy", "growth", schedule.GrowthEnabled, schedule.GrowthHours, 0},
		// The old growth job was one dependency-aware pipeline. Its stages are
		// surfaced as independent plans at the same hours, staggered by five
		// minutes so one account is not hit by six writes at the same instant.
		{"legacy-makeup", "workbuddy", "makeup", schedule.GrowthEnabled, schedule.GrowthHours, 5},
		{"legacy-redeem", "workbuddy", "redeem", schedule.GrowthEnabled, schedule.GrowthHours, 10},
		{"legacy-lottery", "workbuddy", "lottery", schedule.GrowthEnabled, schedule.GrowthHours, 15},
		{"legacy-buddy-box", "workbuddy", "buddy_box", schedule.GrowthEnabled, schedule.GrowthHours, 20},
		{"legacy-resources", "workbuddy", "resources", schedule.GrowthEnabled, schedule.GrowthHours, 25},
		// Compensation had no scheduled entry in the legacy runtime and was a
		// one-off manual claim. Keep it visible but disabled; only an explicit
		// user edit may automate it.
		{"legacy-compensation", "workbuddy", "compensation", false, schedule.GrowthHours, 30},
	} {
		if len(item.hours) == 0 {
			continue
		}
		source := item.enabled
		plan := taskengine.Schedule{ID: item.id, ProviderID: item.provider, TaskKey: legacyTaskKey(item.key),
			Enabled: item.id == "legacy-balance" && item.enabled,
			Hours:   append([]int{}, item.hours...), Minute: item.minute,
			Timezone: "Asia/Shanghai", AccountIDs: []string{}, LegacyEnabled: &source}
		if item.key == "compensation" {
			plan.LegacyEnabled = nil
		}
		out = append(out, plan)
	}
	return out
}

// Returns defaults for the current task catalog. Growth children inherit an
// existing growth plan's clock/scope/switch so an upgraded workspace keeps the
// operator's edited time; otherwise the legacy growth hours and staggered
// minutes are used. These are independent plans, not a dependency executor.
func inheritGrowthSchedule(candidates, existing []taskengine.Schedule) []taskengine.Schedule {
	var parents []taskengine.Schedule
	for _, p := range existing {
		if p.ID == "legacy-growth" || (p.ProviderID == "workbuddy" && (p.TaskKey == "growth" || p.TaskKey == "growth/")) {
			parents = append(parents, p)
		}
	}
	if len(parents) != 1 {
		return candidates
	}
	p := parents[0]
	for i := range candidates {
		s := &candidates[i]
		switch s.ID {
		case "legacy-makeup", "legacy-redeem", "legacy-lottery", "legacy-buddy-box", "legacy-resources":
		default:
			continue
		}
		// Clock and scope inherit; the switch never does: these plans can
		// write upstream and an upgrade must not arm them.
		s.Timezone, s.OncePerDay = p.Timezone, p.OncePerDay
		s.AccountIDs = append([]string{}, p.AccountIDs...)
		s.Hours = append([]int{}, p.Hours...)
		offset := s.Minute
		s.Minute = (p.Minute + offset) % 60
		carry := (p.Minute + offset) / 60
		if carry > 0 {
			for j := range s.Hours {
				s.Hours[j] = (s.Hours[j] + carry) % 24
			}
		}
	}
	return candidates
}

func runWorkbench(ctx context.Context, w *workbenchRuntime) error {
	if w.taskVault != nil {
		defer w.taskVault.Close()
	}
	defer w.vault.Close()
	defer w.engine.Close()
	if w.managed != nil {
		recoveryCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			rows, err := w.vault.List()
			if err != nil {
				return
			}
			for _, account := range rows {
				if account.Provider != "doubao" || !account.Enabled {
					continue
				}
				// Starting an owned profile restores the official client's own
				// session. It never creates a login flow or sends a code/QR.
				bounded, stop := context.WithTimeout(recoveryCtx, 25*time.Second)
				_, err := w.managed.Endpoint(bounded, true)
				stop()
				if err != nil && recoveryCtx.Err() == nil {
					log.Print("[doubao] official client recovery unconfirmed; inspect connection status")
				}
				return
			}
		}()
		defer func() { cancel(); <-done }()
	}
	return w.engine.Run(ctx)
}
