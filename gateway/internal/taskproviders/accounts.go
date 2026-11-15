// Package taskproviders adapts account-scoped business modules to taskengine.
package taskproviders

import (
	"context"
	"errors"
	"strings"
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
type ModuleSpec struct {
	ID, Key string
	Prefix  bool
	Realms  []string
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
	Pool          *pool.Pool
	Upstream      *upstream.Client
	Provider      string
	Modules       []Module
	ExpiringSoon  time.Duration
	ModuleTimeout time.Duration
}

func (a *Accounts) modules() ([]Module, error) {
	if a.Modules != nil {
		return a.Modules, nil
	}
	return DefaultModules(a)
}
func NewAccounts(p *pool.Pool, up *upstream.Client, provider string, soon time.Duration) (*Accounts, error) {
	a := &Accounts{Pool: p, Upstream: up, Provider: provider, ExpiringSoon: soon}
	modules, err := DefaultModules(a)
	if err != nil {
		return nil, err
	}
	a.Modules = modules
	return a, nil
}
func (a *Accounts) account(id string) (*auth.Auth, error) {
	if a.Pool == nil {
		return nil, errors.New("account_pool_unavailable")
	}
	status, ok := a.Pool.Status(id)
	if !ok || status.Disabled {
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
	for _, status := range a.Pool.List() {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if status.Disabled {
			continue
		}
		selected := len(scope.AccountIDs) == 0
		for _, id := range scope.AccountIDs {
			if id == status.UID {
				selected = true
			}
		}
		if !selected {
			continue
		}
		account, err := a.account(status.UID)
		if err != nil {
			continue
		}
		for _, m := range modules {
			s := m.Spec()
			if !s.realm(account.Realm()) || key != "" && key != "*" && !s.matches(key) {
				continue
			}
			bounded, cancel := context.WithTimeout(ctx, timeout)
			defs, err := discoverModule(bounded, m, account)
			cancel()
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			if err == nil {
				for _, d := range defs {
					if !s.matches(d.Key) || d.ProviderID != "" && d.ProviderID != a.Provider {
						err = errors.New("module_definition_mismatch")
						break
					}
				}
			}
			if err != nil {
				out.Issues = append(out.Issues, taskengine.DiscoveryIssue{ProviderID: a.Provider, AccountID: account.UID, ModuleID: s.ID, ErrorCode: "module_discovery_failed"})
				continue
			}
			for _, d := range defs {
				d.ProviderID = a.Provider
				out.Tasks = append(out.Tasks, taskengine.Task{Definition: d, Target: taskengine.Target{AccountID: account.UID, Realm: account.Realm()}})
			}
		}
	}
	return out, nil
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
	account, err := a.account(t.AccountID)
	if err != nil {
		return taskengine.Observation{}, err
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
	return a.dispatch(ctx, t, func(m Module, a *auth.Auth) (taskengine.Observation, error) { return m.Execute(ctx, a, t) })
}
func (a *Accounts) Claim(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return a.dispatch(ctx, t, func(m Module, a *auth.Auth) (taskengine.Observation, error) { return m.Claim(ctx, a, t) })
}
func (a *Accounts) Verify(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return a.dispatch(ctx, t, func(m Module, a *auth.Auth) (taskengine.Observation, error) { return m.Verify(ctx, a, t) })
}
