// Package taskengine owns provider independent task definitions, schedules,
// execution records and leases. It never stores credentials or raw upstream errors.
package taskengine

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

type Stage string

const (
	StageDiscover Stage = "discover"
	StageInspect  Stage = "inspect"
	StageExecute  Stage = "execute"
	StageClaim    Stage = "claim"
	StageVerify   Stage = "verify"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
	StatusUncertain Status = "uncertain"
	StatusSkipped   Status = "skipped"
	StatusManual    Status = "manual_action"
)

var (
	ErrNotFound  = errors.New("task item not found")
	ErrBusy      = errors.New("task already has an active lease")
	ErrInvalid   = errors.New("invalid task configuration")
	ErrLeaseLost = errors.New("task lease lost")
	ErrUncertain = errors.New("previous non-idempotent execution needs verification")
	// ErrLegacyScheduleImportBlocked means the one-time legacy schedule import
	// was skipped because user-owned data (existing plans or in-flight
	// mutations) must not be overwritten. The import marker stays unset so the
	// operator can retry after resolving the state.
	ErrLegacyScheduleImportBlocked = errors.New("legacy schedule import blocked by existing state")
)
var IDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// TaskKeyMatches 判断一个「请求/计划的任务键」是否覆盖某个已发现任务。
//
// 语义：
//   - "" 与 "*" 覆盖全部任务（与既有行为一致）；
//   - 完全相等覆盖该任务；
//   - 以 "/" 结尾时按前缀覆盖（例如 "growth/" 覆盖 "growth/accept/<码>" 与
//     "growth/<码>"）。
//
// 为什么需要前缀：旧 config 的 schedule 段只有**模块级**排程（growth_hours
// 表示成长中心整条流水线），而发现出来的任务键是 "growth/<任务码>"。导入时
// 若用裸模块名 "growth"，发现阶段能给出任务（模块 spec 本身按前缀匹配），但
// 运行阶段的逐字过滤会把它们全丢掉——计划按点触发、发现 0 个任务、以
// status=skipped 结束，看起来启用、实际什么都不做。用 "growth/" 表达「这个
// 模块下的所有当前任务」，既不丢任务，也不会像 "*" 那样把签到、余额刷新等
// 无关侧写任务一起带进来。
func TaskKeyMatches(pattern, key string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if pattern == key {
		return true
	}
	return strings.HasSuffix(pattern, "/") && strings.HasPrefix(key, pattern)
}

type Definition struct {
	Key            string  `json:"key"`
	ProviderID     string  `json:"provider_id"`
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	Stages         []Stage `json:"stages"`
	ManualOnly     bool    `json:"manual_only"`
	OncePerDay     bool    `json:"once_per_day"`
	Once           bool    `json:"once"`
	SideEffect     bool    `json:"side_effect"`
	Idempotent     bool    `json:"idempotent"`
	MaxAttempts    int     `json:"max_attempts"`
	PeriodKey      string  `json:"period_key,omitempty"`
	Reward         string  `json:"reward,omitempty"`
	ActionURL      string  `json:"action_url,omitempty"`
	TimeoutSeconds int     `json:"timeout_seconds,omitempty"`
}

type Scope struct {
	AccountIDs []string `json:"account_ids"`
}
type Target struct {
	AccountID string `json:"account_id"`
	Realm     string `json:"realm,omitempty"`
}
type Task struct {
	Definition
	Target
	State               *Observation `json:"state,omitempty"`
	PendingVerification bool         `json:"pending_verification,omitempty"`
	// Set only by the engine after a successful adapter action result is durably
	// recorded. Missing on an interrupted/unknown response; never inferred from
	// an empty directory or HTTP200 alone.
	AcceptedStage Stage `json:"accepted_stage,omitempty"`
	actionAttempt string
	actionStage   Stage
}

// Observation is an adapter's factual result. Success at Execute/Claim alone
// does not prove a completed task: a declared Verify stage must confirm it.
// Summary must be safe operator text; raw response bodies and credentials are forbidden.
type Observation struct {
	// Action evidence is private transport metadata, not an upstream phase.
	action    *actionEvidence
	Status    Status `json:"status"`
	Summary   string `json:"summary,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	// Phase is authoritative upstream state, never inferred from HTTP status.
	Phase   string `json:"phase"` // ready, pending, claimable, done, manual, blocked
	Current int64  `json:"current,omitempty"`
	Total   int64  `json:"total,omitempty"`
}
type Adapter interface {
	Discover(context.Context, Scope) ([]Task, error)
	Inspect(context.Context, Task) (Observation, error)
	Execute(context.Context, Task) (Observation, error)
	Claim(context.Context, Task) (Observation, error)
	Verify(context.Context, Task) (Observation, error)
}

// TaskGate owns provider pacing before the action timeout and write marker.
// The release always runs, including storage failure, cancellation and panic.
// Waiting must not submit requests. Inspect/Execute recheck eligibility after it.
type TaskGate interface {
	AcquireTask(context.Context, Task) (func(), error)
}

type DiscoveryIssue struct {
	ProviderID string `json:"provider_id"`
	AccountID  string `json:"account_id"`
	ModuleID   string `json:"module_id"`
	ErrorCode  string `json:"error_code"`
}
type Catalog struct {
	Tasks  []Task           `json:"tasks"`
	Issues []DiscoveryIssue `json:"issues"`
}

// CatalogAdapter can isolate account/module failures and discover only the
// requested task family. The scheduler still knows no provider task names.
type CatalogAdapter interface {
	DiscoverCatalog(context.Context, Scope, string) (Catalog, error)
}

type Schedule struct {
	ID             string     `json:"id"`
	TaskKey        string     `json:"task_key"`
	ProviderID     string     `json:"provider_id"`
	Enabled        bool       `json:"enabled"`
	Hours          []int      `json:"hours"`
	Minute         int        `json:"minute"`
	Timezone       string     `json:"timezone"`
	AccountIDs     []string   `json:"account_ids"`
	OncePerDay     bool       `json:"once_per_day"`
	LegacyEnabled  *bool      `json:"legacy_enabled,omitempty"`
	// UserEdited marks a plan saved through the operator API. Default-import
	// repair must never flip a plan the operator deliberately changed.
	UserEdited     bool       `json:"user_edited,omitempty"`
	NextRun        *time.Time `json:"next_run,omitempty"`
	LastRunID      string     `json:"last_run_id,omitempty"`
	LastDispatched *time.Time `json:"last_dispatched,omitempty"`
}
type Step struct {
	ActionOutcome string     `json:"action_outcome,omitempty"`
	TaskKey       string     `json:"task_key,omitempty"`
	AccountID     string     `json:"account_id,omitempty"`
	Stage         Stage      `json:"stage"`
	Status        Status     `json:"status"`
	Attempt       int        `json:"attempt"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Summary       string     `json:"summary,omitempty"`
	ErrorCode     string     `json:"error_code,omitempty"`
	UpstreamCode  *int       `json:"upstream_code,omitempty"`
}
type Run struct {
	DiscoveryIssues []DiscoveryIssue `json:"discovery_issues,omitempty"`
	ID              string           `json:"id"`
	TaskKey         string           `json:"task_key"`
	ProviderID      string           `json:"provider_id"`
	ScheduleID      string           `json:"schedule_id,omitempty"`
	Trigger         string           `json:"trigger"`
	Status          Status           `json:"status"`
	Stage           Stage            `json:"stage,omitempty"`
	StartedAt       time.Time        `json:"started_at"`
	FinishedAt      *time.Time       `json:"finished_at,omitempty"`
	AccountIDs      []string         `json:"account_ids"`
	Summary         string           `json:"summary,omitempty"`
	ErrorCode       string           `json:"error_code,omitempty"`
	Steps           []Step           `json:"steps"`
	SideEffect      bool             `json:"side_effect"`
	Idempotent      bool             `json:"idempotent"`
}
type Query struct {
	TaskKey, ProviderID string
	Status              Status
	Limit, Offset       int
}
type Page struct {
	Runs   []Run `json:"runs"`
	Total  int   `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}
type Request struct {
	TaskKey     string     `json:"task_key"`
	ProviderID  string     `json:"provider_id"`
	ScheduleID  string     `json:"schedule_id,omitempty"`
	Trigger     string     `json:"trigger,omitempty"`
	AccountIDs  []string   `json:"account_ids"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
}

// NextFire uses calendar arithmetic in the configured zone, including DST.
// A repeated local hour is dispatched once; nonexistent local hours are skipped.
func NextFire(s Schedule, after time.Time) time.Time {
	if !s.Enabled || len(s.Hours) == 0 {
		return time.Time{}
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.Time{}
	}
	now := after.In(loc)
	var earliest time.Time
	for day := 0; day < 3; day++ {
		d := now.AddDate(0, 0, day)
		for _, hour := range s.Hours {
			candidate := time.Date(d.Year(), d.Month(), d.Day(), hour, s.Minute, 0, 0, loc)
			if candidate.Hour() != hour || candidate.Minute() != s.Minute || candidate.Day() != d.Day() || !candidate.After(after) {
				continue
			}
			if earliest.IsZero() || candidate.Before(earliest) {
				earliest = candidate
			}
		}
		if !earliest.IsZero() {
			break
		}
	}
	return earliest
}
