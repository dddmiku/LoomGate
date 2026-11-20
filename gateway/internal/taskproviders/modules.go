// Package taskproviders adapts account-scoped business modules to taskengine.
//
// Layout contract: one task = one file. A task file owns its implementation, its
// taskengine.Definition and its DefaultPlan (the schedule the product ships
// with). Nothing else lists task names, hours or stages. The catalogs in
// workbuddy_modules.go / trae_modules.go only name the file's constructor, so
// removing a task is "delete its file, delete its catalog line" and removing a
// schedule is deleting the DefaultPlan in that same file.
package taskproviders

import (
	"context"
	"errors"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
)

// moduleBase is the shared skeleton for tasks whose discovery is a single static
// definition. Task-specific behaviour lives in its own file.
type moduleBase struct {
	spec       ModuleSpec
	definition taskengine.Definition
}

func (m moduleBase) Spec() ModuleSpec { return m.spec }
func (m moduleBase) Discover(ctx context.Context, a *auth.Auth) ([]taskengine.Definition, error) {
	d := m.definition
	if d.OncePerDay {
		location, _ := time.LoadLocation("Asia/Shanghai")
		d.PeriodKey = time.Now().In(location).Format("2006-01-02")
	}
	return []taskengine.Definition{d}, ctx.Err()
}
func (moduleBase) Execute(_ context.Context, _ *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{Phase: "manual"}), nil
}
func (moduleBase) Claim(_ context.Context, _ *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return taskengine.NotSubmitted(t, taskengine.StageClaim, taskengine.Observation{}), errors.New("claim_unsupported")
}

// sequenceMarkerModule reserves a stable plan anchor. It is intentionally
// omitted from the dynamic catalog and cannot itself submit a provider action.
type sequenceMarkerModule struct{ moduleBase }

func (*sequenceMarkerModule) Inspect(context.Context, *auth.Auth, taskengine.Task) (taskengine.Observation, error) {
	return taskengine.Observation{Phase: "manual", Summary: "该计划仅通过引擎保存的 task_sequence 启动。"}, nil
}
func (m *sequenceMarkerModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}

// AccountsOptions carries the optional stores a task may need. Each task that
// requires one checks for it and simply stays unregistered when it is absent,
// so a deployment without the vault never advertises a task it cannot prove.
type AccountsOptions struct {
	TraeStore    *TraeTaskStore
	TraeClient   TraeTaskAPI
	Now          func() time.Time
	RefreshVault *accountvault.Vault
	// SpendVault stores durable lottery/Buddy attempt journals. It is a
	// separate option so reward modules do not implicitly enable keepalive.
	SpendVault *accountvault.Vault
	Refresh    func(context.Context, *auth.Auth) (auth.TokenUpdate, error)
}

// DefaultPlan is the schedule a task ships with. Hours/minute are Asia/Shanghai.
// A sequence stays in one taskengine run and retains ordered per-stage evidence.
// StartEnabled is true only for read-only balance refresh. Every plan that
// writes upstream is imported paused so an upgrade never arms a write.
type DefaultPlan struct {
	ID            string
	TaskKey       string
	TaskSequence  []string
	Hours         []int
	Minute        int
	OncePerDay    bool
	StartEnabled  bool
	LegacyEnabled *bool
}

// moduleCatalogEntry is one task in the shipped catalog. Keeping the schedule
// beside the constructor is what lets a single file own a whole task.
type moduleCatalogEntry struct {
	provider string
	plan     *DefaultPlan
	build    func(*Accounts) (Module, error)
}

func boolValue(v bool) *bool { return &v }

// DefaultModules builds the catalog for one provider. A build function returning
// (nil, nil) means "this task is not available in this configuration"; it is
// skipped rather than aborting the whole provider.
func DefaultModules(a *Accounts) ([]Module, error) {
	var catalog []moduleCatalogEntry
	switch a.Provider {
	case "workbuddy":
		catalog = workbuddyCatalog
	case "trae":
		catalog = traeCatalog
	default:
		return nil, errors.New("provider_unsupported")
	}
	out := make([]Module, 0, len(catalog))
	for _, entry := range catalog {
		module, err := entry.build(a)
		if err != nil {
			return nil, err
		}
		if module == nil {
			continue
		}
		out = append(out, module)
	}
	if len(out) == 0 {
		return nil, errors.New("provider_unsupported")
	}
	return out, nil
}

// BuiltinSchedules returns the task list the product ships with, straight from
// the task files that own them. The server imports this once into the engine;
// it never derives plans from an old config file.
func BuiltinSchedules() []taskengine.Schedule {
	out := []taskengine.Schedule{}
	for _, catalog := range [][]moduleCatalogEntry{workbuddyCatalog, traeCatalog} {
		for _, entry := range catalog {
			if entry.plan == nil {
				continue
			}
			out = append(out, taskengine.Schedule{
				ID: entry.plan.ID, ProviderID: entry.provider, TaskKey: entry.plan.TaskKey,
				TaskSequence: append([]string{}, entry.plan.TaskSequence...),
				OncePerDay:   entry.plan.OncePerDay,
				Enabled:      entry.plan.StartEnabled, Hours: append([]int{}, entry.plan.Hours...),
				Minute: entry.plan.Minute, Timezone: "Asia/Shanghai", AccountIDs: []string{},
				LegacyEnabled: func() *bool {
					if entry.plan.LegacyEnabled == nil {
						return nil
					}
					source := *entry.plan.LegacyEnabled
					return &source
				}(),
			})
		}
	}
	return out
}
