package taskengine

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func logResult(account, key string, status Status) TaskResult {
	return TaskResult{AccountID: account, TaskKey: key, PeriodKey: "synthetic-day", Status: status}
}

func TestTaskLogStatisticsDeduplicateAndPrioritizeAccountOutcomes(t *testing.T) {
	r := Run{TaskResultTracking: true, Status: StatusSucceeded, AccountIDs: []string{"account-a", "account-b", "account-c"}, TaskResults: []TaskResult{
		logResult("account-a", "checkin", StatusSucceeded), logResult("account-a", "balance", StatusSkipped),
		logResult("account-b", "checkin", StatusManual), logResult("account-b", "balance", StatusFailed),
		logResult("account-c", "first", StatusSucceeded), logResult("account-c", "second", StatusUncertain),
	}}
	s := taskLogStatistics(r)
	if s.Accounts != 3 || s.Verified != 1 || s.Failed != 1 || s.Uncertain != 1 || s.Manual != 0 || s.Skipped != 0 {
		t.Fatalf("task/stage rows inflated account totals: %+v", s)
	}
}

func TestTaskLogStatisticsHistoricalStageSuccessDoesNotInventVerifiedAccounts(t *testing.T) {
	code := 9074
	r := Run{Status: StatusSucceeded, AccountIDs: []string{"account-a", "account-b"}, Steps: []Step{
		{AccountID: "account-a", TaskKey: "checkin", Stage: StageInspect, Status: StatusSucceeded},
		{AccountID: "account-a", TaskKey: "checkin", Stage: StageExecute, Status: StatusSucceeded, ActionOutcome: "accepted"},
		{AccountID: "account-a", TaskKey: "checkin", Stage: StageVerify, Status: StatusSucceeded},
		{AccountID: "account-b", TaskKey: "checkin", Stage: StageExecute, Status: StatusFailed, ActionOutcome: "rejected", UpstreamCode: &code},
	}}
	s := taskLogStatistics(r)
	if s.Verified != 0 || s.Unclassified != 2 || s.RejectedAccounts != 1 || len(s.UpstreamCodes) != 1 || s.UpstreamCodes[0].Code != 9074 {
		t.Fatalf("historical step success was mistaken for account verification: %+v", s)
	}
	if !strings.Contains(strings.Join(s.lines(), "\n"), "未从步骤成功") {
		t.Fatal("missing explanation for incomplete historical evidence")
	}
}

func TestTaskLogStatisticsRunningWaitingAndNotExecutedAreDistinct(t *testing.T) {
	r := Run{TaskResultTracking: true, Status: StatusRunning, AccountIDs: []string{"account-a", "account-b", "account-c"}, Output: []OutputLine{{AccountID: "account-a", TaskKey: "checkin"}}}
	s := taskLogStatistics(r)
	if s.Accounts != 3 || s.Running != 1 || s.Waiting != 2 || s.Verified != 0 {
		t.Fatalf("running and waiting collapsed: %+v", s)
	}
	r.Status = StatusCanceled
	s = taskLogStatistics(r)
	if s.Unclassified != 1 || s.NoRecords != 2 || s.Verified != 0 {
		t.Fatalf("untouched accounts were called successful: %+v", s)
	}
	r.AccountIDs = nil
	s = taskLogStatistics(r)
	if s.Accounts != 1 || s.ScopeKnown || !strings.Contains(strings.Join(s.lines(), "\n"), "不代表整个账号池") {
		t.Fatal("all-account scope pretended to know the pool total")
	}
}

func TestTaskLogStatisticsCountRejectionsOnceAndIgnoreSupersededRejection(t *testing.T) {
	code := 9074
	rejected := Step{AccountID: "account-a", TaskKey: "checkin", Stage: StageExecute, ActionOutcome: "rejected", UpstreamCode: &code}
	r := Run{Steps: []Step{rejected, rejected}}
	s := taskLogStatistics(r)
	if s.RejectedAccounts != 1 || s.UpstreamCodes[0].Accounts != 1 {
		t.Fatal("repeated stages inflated the refusal count")
	}
	r.TaskResults = []TaskResult{logResult("account-a", "checkin", StatusSucceeded)}
	if s = taskLogStatistics(r); s.RejectedAccounts != 0 || s.Verified != 1 {
		t.Fatal("old refusal survived authoritative reconciliation")
	}
}

func TestTaskLogStatisticsProjectionExcludesSiblingSequenceFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Now().UTC()
	state := newState()
	state.Runs = []Run{{ID: "synthetic-run", ProviderID: "fake", TaskKey: "daily-sequence", TaskSequence: []string{"checkin", "balance"}, TaskResultTracking: true, Status: StatusFailed, StartedAt: now, FinishedAt: &now, AccountIDs: []string{"account-a", "account-b"},
		TaskResults:     []TaskResult{logResult("account-a", "checkin", StatusFailed), logResult("account-a", "balance", StatusSucceeded)},
		DiscoveryIssues: []DiscoveryIssue{{ProviderID: "fake", AccountID: "account-b", ModuleID: "checkin", ErrorCode: "upstream_unavailable"}},
	}}
	if err := writeState(path, state); err != nil {
		t.Fatal(err)
	}
	e, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	log, err := e.TaskLogFor("fake", "balance")
	if err != nil {
		t.Fatal(err)
	}
	if log.Statistics == nil || log.Statistics.Verified != 1 || log.Statistics.Failed != 0 || log.Statistics.NoRecords != 1 {
		t.Fatalf("sibling task result leaked into statistics: %+v", log.Statistics)
	}
	response := httptest.NewRecorder()
	e.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/workbench/task/log?provider=fake&key=balance", nil))
	var body struct {
		Statistics TaskLogStatistics
		Lines      []string
	}
	if err = json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || body.Statistics.Verified != 1 || body.Statistics.Failed != 0 {
		t.Fatal("statistics missing or unscoped in log API")
	}
}

func TestTaskLogStatisticsFooterSurvivesVerboseLogTruncation(t *testing.T) {
	now := time.Now().UTC()
	r := Run{TaskResultTracking: true, Status: StatusFailed, StartedAt: now, FinishedAt: &now, AccountIDs: []string{"account-a"}, TaskResults: []TaskResult{logResult("account-a", "checkin", StatusFailed)}}
	for range 2100 {
		r.Steps = append(r.Steps, Step{AccountID: "account-a", TaskKey: "checkin", Status: StatusSucceeded, Stage: StageInspect, Summary: "合成过程信息"})
	}
	lines := taskLogLines(r)
	if len(lines) > 2000 || !strings.Contains(lines[len(lines)-1], "本轮统计：账号 1｜已核验 0｜失败 1") || !strings.Contains(strings.Join(lines, "\n"), "另有") {
		t.Fatal("long log dropped its final statistics")
	}
}

func TestTaskLogStatisticsPartialAccountIsNotVerifiedBeforeRemainingWork(t *testing.T) {
	r := Run{Status: StatusRunning, TaskKey: "daily-sequence", TaskSequence: []string{"checkin", "balance"}, TaskResultTracking: true, AccountIDs: []string{"account-a"}, TaskResults: []TaskResult{logResult("account-a", "checkin", StatusSucceeded)}}
	work := runWork{SequenceAccounts: []string{"account-a"}, SequenceAccount: 0, SequenceIndex: 1}
	s := taskLogStatisticsWithPending(r, taskLogPendingAccounts(r, work, r.TaskKey))
	if s.Verified != 0 || s.Running != 1 {
		t.Fatalf("first stage confirmed the whole account sequence: %+v", s)
	}
	// A view scoped to the already verified single operation may still show
	// that operation as verified; its unfinished sibling does not leak in.
	s = taskLogStatisticsWithPending(r, taskLogPendingAccounts(r, work, "checkin"))
	if s.Verified != 1 {
		t.Fatal("completed operation lost its scoped confirmation")
	}
	r.TaskSequence = nil
	r.TaskKey = "growth/"
	r.TaskResults = []TaskResult{logResult("account-a", "growth/one", StatusSucceeded)}
	work = runWork{Next: 1, Tasks: []Task{{Definition: Definition{Key: "growth/one"}, Target: Target{AccountID: "account-a"}}, {Definition: Definition{Key: "growth/two"}, Target: Target{AccountID: "account-a"}}}}
	s = taskLogStatisticsWithPending(r, taskLogPendingAccounts(r, work, r.TaskKey))
	if s.Verified != 0 || s.Running != 1 {
		t.Fatal("first item confirmed an account with more captured tasks")
	}
}
