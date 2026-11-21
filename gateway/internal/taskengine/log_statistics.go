package taskengine

import (
	"fmt"
	"sort"
	"strings"
)

// TaskLogStatistics counts accounts, not HTTP calls or successful stage rows.
// Verified means this operation was confirmed; it is not a promise that a
// larger upstream workflow or reward has completed.
type TaskLogStatistics struct {
	Accounts         int                `json:"accounts"`
	Verified         int                `json:"verified"`
	Failed           int                `json:"failed"`
	Manual           int                `json:"manual"`
	Uncertain        int                `json:"uncertain"`
	Skipped          int                `json:"skipped"`
	Canceled         int                `json:"canceled"`
	Running          int                `json:"running"`
	Waiting          int                `json:"waiting"`
	NoRecords        int                `json:"no_records"`
	Unclassified     int                `json:"unclassified"`
	ScopeKnown       bool               `json:"scope_known"`
	RejectedAccounts int                `json:"rejected_accounts"`
	UpstreamCodes    []TaskLogCodeCount `json:"upstream_codes"`
}

type TaskLogCodeCount struct {
	Code     int `json:"code"`
	Accounts int `json:"accounts"`
}

type logAccountKind int

const (
	logSkipped logAccountKind = iota
	logVerified
	logNoRecords
	logWaiting
	logRunning
	logUnclassified
	logCanceled
	logManual
	logFailed
	logUncertain
)

func logResultKind(status Status) logAccountKind {
	switch status {
	case StatusSucceeded:
		return logVerified
	case StatusFailed:
		return logFailed
	case StatusManual:
		return logManual
	case StatusUncertain:
		return logUncertain
	case StatusSkipped:
		return logSkipped
	case StatusCanceled:
		return logCanceled
	default:
		return logUnclassified
	}
}

func taskLogStatistics(r Run) TaskLogStatistics {
	return taskLogStatisticsWithPending(r, nil)
}

func taskLogStatisticsWithPending(r Run, pending map[string]logAccountKind) TaskLogStatistics {
	type accountState struct {
		kind    logAccountKind
		hasKind bool
	}
	accounts := map[string]*accountState{}
	ensure := func(id string) *accountState {
		if id == "" {
			return nil
		}
		if accounts[id] == nil {
			accounts[id] = &accountState{}
		}
		return accounts[id]
	}
	mark := func(id string, kind logAccountKind) {
		a := ensure(id)
		if a != nil && (!a.hasKind || kind > a.kind) {
			a.kind, a.hasKind = kind, true
		}
	}
	for _, id := range r.AccountIDs {
		ensure(id)
	}
	// Multiple periods/tasks may be recorded for one account. Preserve the
	// most serious unresolved result; each account contributes to one bucket.
	recorded := map[string]bool{}
	resolved := map[string]bool{}
	for _, result := range r.TaskResults {
		key := result.AccountID + "\x00" + result.TaskKey
		recorded[key] = true
		if result.Status == StatusSucceeded || result.Status == StatusSkipped {
			resolved[key] = true
		}
		mark(result.AccountID, logResultKind(result.Status))
	}
	observe := func(account, task string) {
		if account == "" || recorded[account+"\x00"+task] {
			return
		}
		kind := logUnclassified
		if r.Status == StatusRunning {
			kind = logRunning
		}
		if r.Status == StatusQueued {
			kind = logWaiting
		}
		mark(account, kind)
	}
	for _, step := range r.Steps {
		observe(step.AccountID, step.TaskKey)
	}
	for _, line := range r.Output {
		observe(line.AccountID, line.TaskKey)
	}
	for _, issue := range r.DiscoveryIssues {
		mark(issue.AccountID, logFailed)
	}
	for account, kind := range pending {
		mark(account, kind)
	}
	stats := TaskLogStatistics{ScopeKnown: len(r.AccountIDs) > 0, UpstreamCodes: []TaskLogCodeCount{}}
	for _, account := range accounts {
		if !account.hasKind {
			account.kind = logUnclassified
			if r.Status == StatusRunning || r.Status == StatusQueued {
				account.kind = logWaiting
			} else if r.TaskResultTracking {
				account.kind = logNoRecords
			}
		}
		stats.Accounts++
		switch account.kind {
		case logVerified:
			stats.Verified++
		case logFailed:
			stats.Failed++
		case logManual:
			stats.Manual++
		case logUncertain:
			stats.Uncertain++
		case logSkipped:
			stats.Skipped++
		case logCanceled:
			stats.Canceled++
		case logRunning:
			stats.Running++
		case logWaiting:
			stats.Waiting++
		case logNoRecords:
			stats.NoRecords++
		default:
			stats.Unclassified++
		}
	}
	// Refusal reasons are an additional breakdown, never an extra account in
	// the total. Ignore a previous refusal if this task was later confirmed.
	lastAction := map[string]Step{}
	for _, step := range r.Steps {
		if step.AccountID != "" && (step.Stage == StageExecute || step.Stage == StageClaim) && step.ActionOutcome != "" {
			lastAction[step.AccountID+"\x00"+step.TaskKey] = step
		}
	}
	rejected := map[string]bool{}
	codes := map[int]map[string]bool{}
	for key, step := range lastAction {
		if step.ActionOutcome != "rejected" || resolved[key] {
			continue
		}
		rejected[step.AccountID] = true
		if step.UpstreamCode != nil {
			code := *step.UpstreamCode
			if codes[code] == nil {
				codes[code] = map[string]bool{}
			}
			codes[code][step.AccountID] = true
		}
	}
	stats.RejectedAccounts = len(rejected)
	for code, ids := range codes {
		stats.UpstreamCodes = append(stats.UpstreamCodes, TaskLogCodeCount{Code: code, Accounts: len(ids)})
	}
	sort.Slice(stats.UpstreamCodes, func(i, j int) bool { return stats.UpstreamCodes[i].Code < stats.UpstreamCodes[j].Code })
	return stats
}

func (s TaskLogStatistics) lines() []string {
	scope := "账号"
	if !s.ScopeKnown {
		scope = "已记录账号"
	}
	line := fmt.Sprintf("本轮统计：%s %d｜已核验 %d｜失败 %d｜需人工 %d｜待核验 %d｜跳过 %d", scope, s.Accounts, s.Verified, s.Failed, s.Manual, s.Uncertain, s.Skipped)
	for _, extra := range []struct {
		label string
		count int
	}{{"进行中", s.Running}, {"待执行", s.Waiting}, {"取消", s.Canceled}, {"无记录", s.NoRecords}, {"记录不足", s.Unclassified}} {
		if extra.count > 0 {
			line += fmt.Sprintf("｜%s %d", extra.label, extra.count)
		}
	}
	lines := []string{line}
	if s.RejectedAccounts > 0 {
		reason := fmt.Sprintf("其中官方明确拒绝：%d 个账号", s.RejectedAccounts)
		details := []string{}
		for _, item := range s.UpstreamCodes[:min(len(s.UpstreamCodes), 6)] {
			details = append(details, fmt.Sprintf("%d × %d", item.Code, item.Accounts))
		}
		if len(s.UpstreamCodes) > 6 {
			details = append(details, "其余业务码见明细")
		}
		if len(details) > 0 {
			reason += "（" + strings.Join(details, "、") + "）"
		}
		lines = append(lines, reason)
	}
	if s.Unclassified > 0 {
		lines = append(lines, "部分记录缺少逐项最终结果，未从步骤成功或日志文字反推账号成功。")
	}
	if s.NoRecords > 0 {
		lines = append(lines, "无记录表示尚无该账号结果，不代表成功，也不能据此确认没有执行过。")
	}
	if !s.ScopeKnown {
		lines = append(lines, "当前为全部账号范围，统计只包含本轮已记录的账号；不代表整个账号池。")
	}
	return lines
}

// Read only the saved cursor. A partially verified account must not look fully
// verified between tasks or stages while it still has accepted work to finish.
func taskLogPendingAccounts(r Run, work runWork, key string) map[string]logAccountKind {
	pending := map[string]logAccountKind{}
	if r.Status != StatusRunning && r.Status != StatusQueued {
		return pending
	}
	mark := func(account string, kind logAccountKind) {
		if account != "" && kind > pending[account] {
			pending[account] = kind
		}
	}
	if len(r.TaskSequence) > 0 && key == r.TaskKey {
		for i, account := range work.SequenceAccounts {
			if work.SequenceDone[account] {
				continue
			}
			kind := logWaiting
			if i == work.SequenceAccount || work.SequenceHolds[account].Tasks != nil {
				kind = logRunning
			}
			mark(account, kind)
		}
		return pending
	}
	for i := work.Next; i < len(work.Tasks); i++ {
		task := work.Tasks[i]
		if key != r.TaskKey && !TaskKeyMatches(key, task.Key) {
			continue
		}
		kind := logWaiting
		if i == work.Next {
			kind = logRunning
		}
		mark(task.AccountID, kind)
	}
	return pending
}
