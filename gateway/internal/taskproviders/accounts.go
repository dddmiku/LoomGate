// Package taskproviders adapts account-scoped business modules to taskengine.
package taskproviders

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
)

// Module owns discovery and operations for one stable key/prefix. New modules
// are registered in the composition root; core scheduling contains no task table.
type Module interface {
	Spec() ModuleSpec
	Discover(context.Context, *auth.Auth) ([]taskengine.Definition, error)
	Inspect(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error)
	Execute(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error)
	Claim(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error)
	Verify(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error)
}

func taskProgress(ctx context.Context, line string) {
	// Progress is diagnostic only; losing a log line cannot change a provider
	// receipt or make an accepted write eligible for replay.
	_ = taskengine.Output(ctx, line)
}

func (a *Accounts) acquireLegacyAccountPacing(ctx context.Context, accountID string) (func(), error) {
	now := time.Now
	if a.pacingNow != nil {
		now = a.pacingNow
	}
	waitFor := a.pacingWait
	if waitFor == nil {
		waitFor = waitAccountPacing
	}
	const interval = 800 * time.Millisecond
	a.pacingOnce.Do(func() {
		a.pacingSlot = make(chan struct{}, 1)
		a.pacingSlot <- struct{}{}
	})
	select {
	case <-a.pacingSlot:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	free := func() { a.pacingSlot <- struct{}{} }
	if wait := a.lastPacedAt.Sub(now()); a.lastPacedAccount != accountID && wait > 0 {
		if err := waitFor(ctx, wait); err != nil {
			free()
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		free()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			a.lastPacedAt = now().Add(interval)
			a.lastPacedAccount = accountID
			free()
		})
	}, nil
}

func waitAccountPacing(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Optional pacing runs outside the per-task request timeout and before any
// write marker. It cannot submit work; the usual dispatch rechecks the account.
type GatedModule interface {
	AcquireTask(context.Context, *auth.Auth, taskengine.Task) (func(), error)
}

func (a *Accounts) AcquireTask(ctx context.Context, t taskengine.Task) (func(), error) {
	modules, err := a.modules()
	if err != nil {
		return nil, err
	}
	for _, m := range modules {
		if m.Spec().matches(t.Key) {
			account, err := a.accountForTask(t.AccountID, m.Spec().RecoverSession)
			if err != nil {
				return nil, err
			}
			if !m.Spec().realm(account.Realm()) {
				continue
			}
			if gate, ok := m.(GatedModule); ok {
				return gate.AcquireTask(ctx, account, t)
			}
			return func() {}, nil
		}
	}
	return nil, errors.New("task_module_unavailable")
}

func (a *Accounts) SequenceAccounts(ctx context.Context, requested, selectors []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.Pool == nil {
		return nil, errors.New("account_pool_unavailable")
	}
	modules, err := a.modules()
	if err != nil {
		return nil, err
	}
	if len(selectors) == 0 {
		return nil, errors.New("task_sequence_empty")
	}
	var allowedRealms []string
	for _, module := range modules {
		spec := module.Spec()
		if spec.matches(selectors[0]) {
			allowedRealms = spec.Realms
			break
		}
	}
	if len(allowedRealms) == 0 {
		return nil, errors.New("task_sequence_route_missing")
	}
	selected := make(map[string]bool, len(requested))
	for _, id := range requested {
		selected[id] = true
	}
	eligible := func(realm string) bool {
		for _, allowed := range allowedRealms {
			if allowed == realm {
				return true
			}
		}
		return false
	}
	if len(requested) > 0 {
		accounts := make([]string, 0, len(requested))
		for _, id := range requested {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			account, accountErr := a.accountForTask(id, true)
			if accountErr != nil || !eligible(account.Realm()) {
				return nil, errors.New("task_sequence_scope_invalid")
			}
			accounts = append(accounts, id)
		}
		return accounts, nil
	}
	accounts := make([]string, 0)
	for _, status := range a.Pool.List() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if status.Disabled && status.DisabledReason != pool.SessionDeadReason() {
			continue
		}
		account, accountErr := a.accountForTask(status.UID, true)
		if accountErr == nil && eligible(account.Realm()) {
			accounts = append(accounts, status.UID)
		}
	}
	return accounts, nil
}

// Stateful modules discover definitions and their matching read snapshot in
// one pass. This does not call Execute/Claim or refresh credentials.
type ModuleTask struct {
	Definition taskengine.Definition
	State      taskengine.Observation
}
type CatalogModule interface {
	DiscoverState(context.Context, *auth.Auth) ([]ModuleTask, error)
}
type ModuleSpec struct {
	ID, Key        string
	Prefix         bool
	InternalOnly   bool
	Realms         []string
	RecoverSession bool
}

func (s ModuleSpec) matches(key string) bool {
	if s.Prefix {
		return strings.HasPrefix(key, s.Key)
	}
	return key == s.Key
}
func (s ModuleSpec) realm(realm string) bool {
	for _, r := range s.Realms {
		if r == realm {
			return true
		}
	}
	return false
}

type Accounts struct {
	Pool                 *pool.Pool
	Upstream             *upstream.Client
	Provider             string
	Modules              []Module
	ExpiringSoon         time.Duration
	ModuleTimeout        time.Duration
	DiscoveryConcurrency int
	discoverySlots       chan struct{}
	discoveryOnce        sync.Once
	pacingOnce           sync.Once
	pacingSlot           chan struct{}
	lastPacedAt          time.Time
	lastPacedAccount     string
	pacingNow            func() time.Time
	pacingWait           func(context.Context, time.Duration) error
	options              AccountsOptions
}

func (a *Accounts) modules() ([]Module, error) {
	if a.Modules != nil {
		return a.Modules, nil
	}
	return DefaultModules(a)
}
func NewAccounts(p *pool.Pool, up *upstream.Client, provider string, soon time.Duration, options ...AccountsOptions) (*Accounts, error) {
	a := &Accounts{Pool: p, Upstream: up, Provider: provider, ExpiringSoon: soon}
	if len(options) > 1 {
		return nil, errors.New("task_options_invalid")
	}
	if len(options) == 1 {
		a.options = options[0]
	}
	modules, err := DefaultModules(a)
	if err != nil {
		return nil, err
	}
	a.Modules = modules
	return a, nil
}
func (a *Accounts) account(id string) (*auth.Auth, error) {
	return a.accountForTask(id, false)
}
func (a *Accounts) accountForTask(id string, recoverSession bool) (*auth.Auth, error) {
	if a.Pool == nil {
		return nil, errors.New("account_pool_unavailable")
	}
	status, ok := a.Pool.Status(id)
	if !ok || status.Disabled && !(recoverSession && status.DisabledReason == pool.SessionDeadReason()) {
		return nil, errors.New("account_unavailable")
	}
	account := a.Pool.AuthByUID(id)
	if account == nil {
		return nil, errors.New("account_unavailable")
	}
	account = account.Snapshot()
	if a.Provider == "trae" && account.Realm() != "trae" || a.Provider == "workbuddy" && !account.IsWorkBuddy() {
		return nil, errors.New("provider_mismatch")
	}
	return account, nil
}
func validateModules(modules []Module) error {
	for i, m := range modules {
		if m == nil {
			return errors.New("module_missing")
		}
		s := m.Spec()
		if !taskengine.IDPattern.MatchString(s.ID) || s.Key == "" || len(s.Key) > 128 || len(s.Realms) == 0 {
			return errors.New("module_invalid")
		}
		for _, other := range modules[:i] {
			o := other.Spec()
			if o.ID == s.ID || o.matches(s.Key) || s.matches(o.Key) {
				return errors.New("module_route_conflict")
			}
		}
	}
	return nil
}
func (a *Accounts) Discover(ctx context.Context, scope taskengine.Scope) ([]taskengine.Task, error) {
	out, err := a.DiscoverCatalog(ctx, scope, "")
	if err == nil && len(out.Issues) > 0 {
		return nil, errors.New("partial_discovery_failed")
	}
	return out.Tasks, err
}
func (a *Accounts) DiscoverCatalog(ctx context.Context, scope taskengine.Scope, key string) (taskengine.Catalog, error) {
	out := taskengine.Catalog{Tasks: []taskengine.Task{}, Issues: []taskengine.DiscoveryIssue{}}
	modules, err := a.modules()
	if err != nil {
		return out, err
	}
	if err = validateModules(modules); err != nil {
		return out, err
	}
	if a.Pool == nil {
		return out, errors.New("account_pool_unavailable")
	}
	timeout := a.ModuleTimeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	limit := a.discoveryConcurrency()
	jobs := make(chan discoveryJob, 1)
	results := make(chan discoveryResult, limit)
	var producers sync.WaitGroup
	producers.Add(1)
	go func() {
		defer producers.Done()
		defer close(jobs)
		selected := make(map[string]bool, len(scope.AccountIDs))
		for _, id := range scope.AccountIDs {
			selected[id] = true
		}
		for _, status := range a.Pool.List() {
			if ctx.Err() != nil {
				return
			}
			if status.Disabled && status.DisabledReason != pool.SessionDeadReason() {
				continue
			}
			if len(scope.AccountIDs) > 0 && !selected[status.UID] {
				continue
			}
			account, err := a.accountForTask(status.UID, true)
			if err != nil {
				continue
			}
			for _, m := range modules {
				s := m.Spec()
				if (s.InternalOnly && (key == "" || key == "*")) || status.Disabled && !s.RecoverSession || !s.realm(account.Realm()) || key != "" && key != "*" && !s.matches(key) {
					continue
				}
				select {
				case jobs <- discoveryJob{account: account, module: m, spec: s}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	var workers sync.WaitGroup
	workers.Add(limit)
	for i := 0; i < limit; i++ {
		go func() {
			defer workers.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				result := a.discoverOne(ctx, timeout, job, key)
				select {
				case results <- result:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() { producers.Wait(); workers.Wait(); close(results) }()
	for result := range results {
		out.Tasks = append(out.Tasks, result.tasks...)
		if result.issue != nil {
			out.Issues = append(out.Issues, *result.issue)
		}
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	sort.SliceStable(out.Tasks, func(i, j int) bool { return out.Tasks[i].AccountID < out.Tasks[j].AccountID })
	sort.SliceStable(out.Issues, func(i, j int) bool {
		if out.Issues[i].AccountID != out.Issues[j].AccountID {
			return out.Issues[i].AccountID < out.Issues[j].AccountID
		}
		return out.Issues[i].ModuleID < out.Issues[j].ModuleID
	})
	return out, nil
}

type discoveryJob struct {
	account *auth.Auth
	module  Module
	spec    ModuleSpec
}

// discoveryReason maps an internal discovery failure to a small, stable panel
// code. Before this, every failure collapsed into module_discovery_failed, so
// the console repeated the same sentence fourteen times and an operator could
// not tell an upstream 5xx from a missing capability.
//
// Only enumerated reasons cross the boundary: raw upstream text, links and
// account identifiers never reach the panel.
func discoveryReason(err error) string {
	var upstreamFailure *upstream.Error
	if errors.As(err, &upstreamFailure) {
		switch upstreamFailure.Kind {
		case upstream.ErrServer, upstream.ErrUpstreamWAF, upstream.ErrUpstreamGateway:
			return "upstream_unavailable"
		case upstream.ErrSessionDead:
			return "authentication_required"
		case upstream.ErrSoftRate:
			return "upstream_rate_limited"
		case upstream.ErrHardCredit, upstream.ErrAccountFault:
			return "account_unavailable"
		case upstream.ErrNotFound:
			return "module_not_offered"
		}
	}
	switch strings.TrimSpace(err.Error()) {
	case "buddy_state_missing", "buddy_state_invalid", "travel_state_invalid",
		"travel_config_invalid", "makeup_state_missing", "redemption_directory_missing",
		"redemption_directory_invalid", "redemption_directory_duplicate", "redemption_state_unknown":
		return "state_unrecognized"
	}
	return "module_discovery_failed"
}

type discoveryResult struct {
	tasks []taskengine.Task
	issue *taskengine.DiscoveryIssue
}

func (a *Accounts) discoveryConcurrency() int {
	a.discoveryOnce.Do(func() {
		limit := a.DiscoveryConcurrency
		if limit <= 0 {
			limit = 8
		}
		if limit > 32 {
			limit = 32
		}
		a.discoverySlots = make(chan struct{}, limit)
	})
	return cap(a.discoverySlots)
}

func (a *Accounts) discoverOne(ctx context.Context, timeout time.Duration, job discoveryJob, key string) discoveryResult {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defs, states, err := discoverModuleState(bounded, job.module, job.account)
	if err == nil {
		for _, d := range defs {
			if !job.spec.matches(d.Key) || d.ProviderID != "" && d.ProviderID != a.Provider {
				err = errors.New("module_definition_mismatch")
				break
			}
		}
	}
	if err != nil {
		return discoveryResult{issue: &taskengine.DiscoveryIssue{ProviderID: a.Provider, AccountID: job.account.UID, ModuleID: job.spec.ID, ErrorCode: discoveryReason(err)}}
	}
	result := discoveryResult{tasks: make([]taskengine.Task, 0, len(defs))}
	for _, d := range defs {
		// 计划可以按模块前缀表达「该模块当前的所有任务」（如 "growth/"）：
		// 旧 config 只有模块级排程，逐字比较会把发现出来的 "growth/<任务码>"
		// 全部丢掉，计划按点触发却什么都不做。
		if !taskengine.TaskKeyMatches(key, d.Key) {
			continue
		}
		d.ProviderID = a.Provider
		task := taskengine.Task{Definition: d, Target: taskengine.Target{AccountID: job.account.UID, Realm: job.account.Realm()}}
		if state, ok := states[d.Key]; ok {
			copy := state
			task.State = &copy
		}
		result.tasks = append(result.tasks, task)
	}
	return result
}

func discoverModuleState(ctx context.Context, m Module, a *auth.Auth) (defs []taskengine.Definition, states map[string]taskengine.Observation, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("module_discovery_failed")
		}
	}()
	if catalog, ok := m.(CatalogModule); ok {
		var rows []ModuleTask
		rows, err = catalog.DiscoverState(ctx, a)
		states = map[string]taskengine.Observation{}
		if len(rows) > 256 {
			return nil, nil, errors.New("module_catalog_limit")
		}
		for _, row := range rows {
			if _, duplicate := states[row.Definition.Key]; duplicate {
				return nil, nil, errors.New("module_duplicate_task")
			}
			defs = append(defs, row.Definition)
			states[row.Definition.Key] = row.State
		}
	} else {
		defs, err = discoverModule(ctx, m, a)
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return
}
func discoverModule(ctx context.Context, m Module, a *auth.Auth) (defs []taskengine.Definition, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("module_discovery_failed")
		}
	}()
	defs, err = m.Discover(ctx, a)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return
}
func (a *Accounts) dispatch(ctx context.Context, t taskengine.Task, fn func(Module, *auth.Auth) (taskengine.Observation, error)) (taskengine.Observation, error) {
	if t.ProviderID != a.Provider {
		return taskengine.Observation{}, errors.New("provider_mismatch")
	}
	modules, err := a.modules()
	if err != nil {
		return taskengine.Observation{}, err
	}
	if err = validateModules(modules); err != nil {
		return taskengine.Observation{}, err
	}
	for _, m := range modules {
		s := m.Spec()
		if !s.matches(t.Key) {
			continue
		}
		account, err := a.accountForTask(t.AccountID, s.RecoverSession)
		if err != nil {
			return taskengine.Observation{}, err
		}
		if s.matches(t.Key) && s.realm(account.Realm()) {
			if ctx.Err() != nil {
				return taskengine.Observation{}, ctx.Err()
			}
			return fn(m, account)
		}
	}
	return taskengine.Observation{}, errors.New("task_module_unavailable")
}
func (a *Accounts) Inspect(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return a.dispatch(ctx, t, func(m Module, a *auth.Auth) (taskengine.Observation, error) { return m.Inspect(ctx, a, t) })
}
func (a *Accounts) Execute(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return a.dispatchAction(ctx, t, taskengine.StageExecute, func(m Module, a *auth.Auth) (taskengine.Observation, error) { return m.Execute(ctx, a, t) })
}
func (a *Accounts) Claim(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return a.dispatchAction(ctx, t, taskengine.StageClaim, func(m Module, a *auth.Auth) (taskengine.Observation, error) { return m.Claim(ctx, a, t) })
}
func (a *Accounts) dispatchAction(ctx context.Context, t taskengine.Task, stage taskengine.Stage, fn func(Module, *auth.Auth) (taskengine.Observation, error)) (taskengine.Observation, error) {
	invoked := false
	o, err := a.dispatch(ctx, t, func(m Module, account *auth.Auth) (taskengine.Observation, error) {
		invoked = true
		return fn(m, account)
	})
	if !invoked {
		o = taskengine.NotSubmitted(t, stage, o)
	}
	return o, err
}
func (a *Accounts) Verify(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return a.dispatch(ctx, t, func(m Module, a *auth.Auth) (taskengine.Observation, error) { return m.Verify(ctx, a, t) })
}
