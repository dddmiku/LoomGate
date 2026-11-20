package taskengine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/taskengine"
)

// The fixture keeps official read-back state separate from the action receipt.
// Unknown stays pending until the test explicitly supplies authoritative proof.
type orderedFixture struct {
	mu                           sync.Mutex
	order                        []string
	failKey, failMode            string
	completed                    map[string]bool
	accounts                     []string
	entered, cancelSeen, release chan struct{}
	dynamic                      bool
	failAccount                  string
	readOnlyResources            bool
	reads                        []string
}

func orderedTaskIdentity(t taskengine.Task) string { return t.AccountID + ":" + t.Key }
func newOrderedFixture() *orderedFixture {
	return &orderedFixture{completed: map[string]bool{}, accounts: []string{"account-a"}}
}
func (f *orderedFixture) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.order...)
}
func (f *orderedFixture) Discover(_ context.Context, scope taskengine.Scope) ([]taskengine.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var tasks []taskengine.Task
	for _, account := range f.accounts {
		if len(scope.AccountIDs) > 0 {
			selected := false
			for _, id := range scope.AccountIDs {
				selected = selected || account == id
			}
			if !selected {
				continue
			}
		}
		for _, key := range []string{"travel/arrive", "growth/accept/live", "growth/live", "makeup", "redeem/monthly", "lottery", "buddy_box", "resources"} {
			if f.dynamic && strings.HasPrefix(key, "growth/") && !f.completed[account+":travel/arrive"] {
				continue
			}
			task := syntheticTask("sequence", account, key)
			task.Name = "合成动态任务 " + key
			if key == "resources" && f.readOnlyResources {
				task.SideEffect = false
				task.Idempotent = true
			}
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}
func (f *orderedFixture) Inspect(_ context.Context, t taskengine.Task) (taskengine.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, orderedTaskIdentity(t))
	if f.completed[orderedTaskIdentity(t)] || t.Key == "resources" && f.readOnlyResources {
		return taskengine.Observation{Phase: "done"}, nil
	}
	if t.Key == "growth/live" {
		return taskengine.Observation{Phase: "claimable"}, nil
	}
	return taskengine.Observation{Phase: "ready"}, nil
}
func (f *orderedFixture) Execute(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return f.act(ctx, t, taskengine.StageExecute)
}
func (f *orderedFixture) Claim(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	return f.act(ctx, t, taskengine.StageClaim)
}
func (f *orderedFixture) act(ctx context.Context, t taskengine.Task, stage taskengine.Stage) (taskengine.Observation, error) {
	f.mu.Lock()
	f.order = append(f.order, orderedTaskIdentity(t)+":"+string(stage))
	mode, match := f.failMode, t.Key == f.failKey && (f.failAccount == "" || f.failAccount == t.AccountID)
	f.mu.Unlock()
	_ = taskengine.Output(ctx, "合成官方请求已提交，等待核验。")
	if match && mode == "unknown" {
		return taskengine.UnknownAction(t, stage, taskengine.Observation{Phase: "pending"}), nil
	}
	if match && mode == "block" {
		return taskengine.NotSubmitted(t, stage, taskengine.Observation{Phase: "blocked"}), nil
	}
	if match && mode == "pause" {
		close(f.entered)
		select {
		case <-f.release:
		case <-ctx.Done():
			close(f.cancelSeen)
			// A late response is observed by the synthetic upstream, even though
			// the engine must stop dispatching new actions while closing.
			select {
			case <-f.release:
			case <-time.After(time.Second):
			}
		}
	}
	f.mu.Lock()
	f.completed[orderedTaskIdentity(t)] = true
	f.mu.Unlock()
	return taskengine.Accepted(t, stage, taskengine.Observation{Phase: "pending"}), nil
}
func (f *orderedFixture) Verify(ctx context.Context, t taskengine.Task) (taskengine.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completed[orderedTaskIdentity(t)] {
		return taskengine.Observation{Phase: "done"}, nil
	}
	return taskengine.Observation{Phase: "pending"}, ctx.Err()
}
func openOrdered(t *testing.T, path string, f *orderedFixture) *taskengine.Engine {
	t.Helper()
	e, err := taskengine.Open(taskengine.Options{Path: path, Timeout: 120 * time.Millisecond, PollMin: time.Millisecond, PollMax: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err = e.Register("sequence", f); err != nil {
		t.Fatal(err)
	}
	return e
}
func orderedRequest(selectors []string, accounts ...string) taskengine.Request {
	return taskengine.Request{ProviderID: "sequence", TaskKey: "growth-sequence", TaskSequence: selectors, AccountIDs: accounts}
}
func submitOrdered(t *testing.T, e *taskengine.Engine, selectors []string) taskengine.Run {
	t.Helper()
	r, err := e.Submit(orderedRequest(selectors, "account-a"))
	if err != nil {
		t.Fatal(err)
	}
	return awaitBehaviorRun(t, e, r.ID)
}

func TestOrderedTaskSequenceRunsDiscoveredFamiliesInDeclaredOrder(t *testing.T) {
	f := newOrderedFixture()
	f.dynamic = true
	path := filepath.Join(t.TempDir(), "sequence.json")
	e := openOrdered(t, path, f)
	sequence := []string{"travel/", "growth/", "makeup", "redeem/", "lottery", "buddy_box", "resources"}
	run := submitOrdered(t, e, sequence)
	if run.Status != taskengine.StatusSucceeded {
		t.Fatalf("ordered pipeline ended %s: %s", run.Status, run.ErrorCode)
	}
	want := []string{"account-a:travel/arrive:execute", "account-a:growth/accept/live:execute", "account-a:growth/live:claim", "account-a:makeup:execute", "account-a:redeem/monthly:execute", "account-a:lottery:execute", "account-a:buddy_box:execute", "account-a:resources:execute"}
	if got := f.events(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pipeline order %v want %v", got, want)
	}
	if !reflect.DeepEqual(run.TaskSequence, sequence) {
		t.Fatal("run lost sequence contract")
	}
	_ = e.Close()
	e = openOrdered(t, path, f)
	logs, err := e.TaskLogFor("sequence", "growth-sequence")
	if err != nil || !strings.Contains(strings.Join(logs.Lines, "\n"), "合成官方请求") {
		t.Fatal("sequence output did not survive restart")
	}
}

func TestOrderedTaskSequenceUnknownStopsAndReadbackResumesWithoutReplay(t *testing.T) {
	f := newOrderedFixture()
	f.failKey, f.failMode = "growth/live", "unknown"
	path := filepath.Join(t.TempDir(), "sequence.json")
	e := openOrdered(t, path, f)
	sequence := []string{"travel/", "growth/", "makeup", "redeem/"}
	run := submitOrdered(t, e, sequence)
	wantFirst := []string{"account-a:travel/arrive:execute", "account-a:growth/accept/live:execute", "account-a:growth/live:claim"}
	if got := f.events(); run.Status != taskengine.StatusUncertain || !reflect.DeepEqual(got, wantFirst) {
		t.Fatalf("unknown did not hold dependency chain: %s %v", run.Status, got)
	}
	_ = e.Close()
	e = openOrdered(t, path, f)
	// Reopening alone must not restart the terminal unknown run.
	for _, row := range e.Runs() {
		if row.ID == run.ID && row.FinishedAt == nil {
			t.Fatal("unknown was automatically redispatched")
		}
	}
	resumed, err := e.Submit(orderedRequest(sequence, "account-a"))
	if err != nil || resumed.ID != run.ID {
		t.Fatalf("pending cursor not resumed: %v", err)
	}
	if current := awaitBehaviorRun(t, e, resumed.ID); current.Status != taskengine.StatusUncertain {
		t.Fatalf("unproven action became %s", current.Status)
	}
	if !reflect.DeepEqual(f.events(), wantFirst) {
		t.Fatal("unknown was replayed before proof")
	}
	_ = e.Close()
	f.mu.Lock()
	f.completed["account-a:growth/live"] = true
	f.mu.Unlock()
	e = openOrdered(t, path, f)
	resumed, err = e.Submit(orderedRequest(sequence, "account-a"))
	if err != nil || resumed.ID != run.ID {
		t.Fatalf("proven cursor not resumed: %v", err)
	}
	final := awaitBehaviorRun(t, e, resumed.ID)
	want := append(append([]string{}, wantFirst...), "account-a:makeup:execute", "account-a:redeem/monthly:execute")
	if got := f.events(); final.Status != taskengine.StatusSucceeded || !reflect.DeepEqual(got, want) {
		t.Fatalf("proof did not resume exactly once: %s %v", final.Status, got)
	}
}

func TestOrderedTaskSequenceRestartResumesAfterSavedStageCursor(t *testing.T) {
	f := newOrderedFixture()
	f.failKey, f.failMode = "makeup", "pause"
	f.entered, f.cancelSeen, f.release = make(chan struct{}), make(chan struct{}), make(chan struct{})
	path := filepath.Join(t.TempDir(), "sequence.json")
	e := openOrdered(t, path, f)
	r, err := e.Submit(orderedRequest([]string{"travel/", "growth/", "makeup", "redeem/"}, "account-a"))
	if err != nil {
		t.Fatal(err)
	}
	awaitBehaviorSignal(t, f.entered)
	closed := make(chan struct{})
	go func() { _ = e.Close(); close(closed) }()
	awaitBehaviorSignal(t, f.cancelSeen)
	close(f.release)
	awaitBehaviorSignal(t, closed)
	f.mu.Lock()
	f.failMode = ""
	f.mu.Unlock()
	e = openOrdered(t, path, f)
	final := awaitBehaviorRun(t, e, r.ID)
	if final.Status != taskengine.StatusSucceeded {
		t.Fatalf("restarted pipeline ended %s: %s", final.Status, final.ErrorCode)
	}
	want := []string{"account-a:travel/arrive:execute", "account-a:growth/accept/live:execute", "account-a:growth/live:claim", "account-a:makeup:execute", "account-a:redeem/monthly:execute"}
	if got := f.events(); !reflect.DeepEqual(got, want) {
		t.Fatalf("completed stage replayed across restart: %v", got)
	}
}

func TestOrderedTaskSequenceUnknownIsAccountLocalAndReadonlyTailStillRuns(t *testing.T) {
	f := newOrderedFixture()
	f.accounts = []string{"account-a", "account-b"}
	f.failKey, f.failMode, f.failAccount = "growth/live", "unknown", "account-a"
	f.readOnlyResources = true
	path := filepath.Join(t.TempDir(), "sequence.json")
	e := openOrdered(t, path, f)
	sequence := []string{"travel/", "growth/", "makeup", "resources"}
	req := orderedRequest(sequence, "account-a", "account-b")
	r, err := e.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	first := awaitBehaviorRun(t, e, r.ID)
	want := []string{"account-a:travel/arrive:execute", "account-a:growth/accept/live:execute", "account-a:growth/live:claim",
		"account-b:travel/arrive:execute", "account-b:growth/accept/live:execute", "account-b:growth/live:claim", "account-b:makeup:execute"}
	if first.Status != taskengine.StatusUncertain || !reflect.DeepEqual(f.events(), want) {
		t.Fatalf("unknown blocked another account or ran dependent writes: status=%s actions=%v", first.Status, f.events())
	}
	f.mu.Lock()
	hasRead := map[string]bool{}
	for _, key := range f.reads {
		hasRead[key] = true
	}
	f.mu.Unlock()
	if !hasRead["account-a:resources"] || !hasRead["account-b:resources"] {
		t.Fatal("read-only resource tail was lost after unknown")
	}
	_ = e.Close()
	e = openOrdered(t, path, f)
	resumed, err := e.Submit(req)
	if err != nil || resumed.ID != r.ID {
		t.Fatalf("account-local unknown cursor not restored: %v", err)
	}
	if final := awaitBehaviorRun(t, e, resumed.ID); final.Status != taskengine.StatusUncertain || !reflect.DeepEqual(f.events(), want) {
		t.Fatalf("unproven retry replayed a completed account: %s %v", final.Status, f.events())
	}
	_ = e.Close()
	f.mu.Lock()
	f.completed["account-a:growth/live"] = true
	f.mu.Unlock()
	e = openOrdered(t, path, f)
	resumed, err = e.Submit(req)
	if err != nil || resumed.ID != r.ID {
		t.Fatalf("proven cursor not restored: %v", err)
	}
	want = append(want, "account-a:makeup:execute")
	if final := awaitBehaviorRun(t, e, resumed.ID); final.Status != taskengine.StatusSucceeded || !reflect.DeepEqual(f.events(), want) {
		t.Fatalf("only unresolved account must continue: %s %v", final.Status, f.events())
	}
}

func TestOrderedTaskSequenceBlockedQualificationStopsDependentStage(t *testing.T) {
	f := newOrderedFixture()
	f.failKey, f.failMode = "travel/arrive", "block"
	e := openOrdered(t, filepath.Join(t.TempDir(), "sequence.json"), f)
	run := submitOrdered(t, e, []string{"travel/", "growth/"})
	if run.Status != taskengine.StatusFailed || len(f.events()) != 1 {
		t.Fatalf("blocked prerequisite did not stop: %s %v", run.Status, f.events())
	}
}

func TestOrderedTaskSequenceCompletesOneAccountBeforeStartingAnother(t *testing.T) {
	f := newOrderedFixture()
	f.accounts = []string{"account-a", "account-b"}
	e := openOrdered(t, filepath.Join(t.TempDir(), "sequence.json"), f)
	r, err := e.Submit(orderedRequest([]string{"travel/", "makeup"}, "account-b", "account-a"))
	if err != nil {
		t.Fatal(err)
	}
	if final := awaitBehaviorRun(t, e, r.ID); final.Status != taskengine.StatusSucceeded {
		t.Fatal(final.Status)
	}
	want := []string{"account-b:travel/arrive:execute", "account-b:makeup:execute", "account-a:travel/arrive:execute", "account-a:makeup:execute"}
	if got := f.events(); !reflect.DeepEqual(got, want) {
		t.Fatalf("account-local sequence mixed: %v", got)
	}
}

func TestOrderedTaskSequenceDefaultScopeResumesUsingItsFrozenExplicitScope(t *testing.T) {
	f := newOrderedFixture()
	f.failKey, f.failMode = "growth/live", "unknown"
	path := filepath.Join(t.TempDir(), "sequence.json")
	e := openOrdered(t, path, f)
	sequence := []string{"travel/", "growth/"}
	r, err := e.Submit(orderedRequest(sequence))
	if err != nil {
		t.Fatal(err)
	}
	if first := awaitBehaviorRun(t, e, r.ID); first.Status != taskengine.StatusUncertain {
		t.Fatal(first.Status)
	}
	_ = e.Close()
	e = openOrdered(t, path, f)
	resumed, err := e.Submit(orderedRequest(sequence, "account-a"))
	if err != nil || resumed.ID != r.ID {
		t.Fatalf("manual explicit scope lost saved default cursor: same_run=%t err=%v", resumed.ID == r.ID, err)
	}
	if final := awaitBehaviorRun(t, e, resumed.ID); final.Status != taskengine.StatusUncertain {
		t.Fatal(final.Status)
	}
	if len(f.events()) != 3 {
		t.Fatal("scope reconciliation replayed writes")
	}
}

func TestOrderedTaskSequenceBusyScheduleDoesNotStopScheduler(t *testing.T) {
	f := newOrderedFixture()
	f.failKey, f.failMode = "travel/arrive", "pause"
	f.entered, f.cancelSeen, f.release = make(chan struct{}), make(chan struct{}), make(chan struct{})
	e := openOrdered(t, filepath.Join(t.TempDir(), "sequence.json"), f)
	sequence := []string{"travel/", "growth/"}
	r, err := e.Submit(orderedRequest(sequence, "account-a"))
	if err != nil {
		t.Fatal(err)
	}
	awaitBehaviorSignal(t, f.entered)
	defer close(f.release)
	plan, err := e.SaveSchedule(taskengine.Schedule{ID: "morning-sequence", ProviderID: "sequence", TaskKey: "growth-sequence", TaskSequence: sequence,
		AccountIDs: []string{"account-a"}, Enabled: true, Hours: []int{0}, Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Tick(*plan.NextRun); err != nil {
		t.Fatalf("busy sequence stopped the scheduler: %v", err)
	}
	if len(e.Runs()) != 1 || e.Runs()[0].ID != r.ID {
		t.Fatal("busy tick created duplicate sequence work")
	}
	saved := e.Schedules()[0]
	if saved.NextRun == nil || !saved.NextRun.After(*plan.NextRun) || saved.LastDispatched == nil {
		t.Fatal("busy tick did not persist the next dispatch time")
	}
}

func TestOrderedTaskSequenceCanceledCrashPreservesUnknownCheckpoint(t *testing.T) {
	f := newOrderedFixture()
	f.failKey, f.failMode = "growth/live", "unknown"
	path := filepath.Join(t.TempDir(), "sequence.json")
	e := openOrdered(t, path, f)
	sequence := []string{"travel/", "growth/", "makeup"}
	r := submitOrdered(t, e, sequence)
	_ = e.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	for _, item := range state["runs"].([]any) {
		run := item.(map[string]any)
		if run["id"] == r.ID {
			run["status"] = "running"
			delete(run, "finished_at")
		}
	}
	state["work"].(map[string]any)[r.ID].(map[string]any)["cancel_requested"] = true
	raw, _ = json.Marshal(state)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	e = openOrdered(t, path, f)
	rows := e.Runs()
	if len(rows) != 1 || rows[0].Status != taskengine.StatusUncertain || rows[0].FinishedAt == nil {
		t.Fatal("cancel/restart discarded the unresolved checkpoint")
	}
	resumed, err := e.Submit(orderedRequest(sequence, "account-a"))
	if err != nil || resumed.ID != r.ID {
		t.Fatalf("cancel lost resumable unknown: %v", err)
	}
	if final := awaitBehaviorRun(t, e, resumed.ID); final.Status != taskengine.StatusUncertain {
		t.Fatal(final.Status)
	}
	if len(f.events()) != 3 {
		t.Fatal("cancel/restart replayed a mutation")
	}
}

func TestOrderedTaskSequenceRejectsTamperedAccountCheckpoints(t *testing.T) {
	changes := map[string]func(map[string]any){
		"cursor":              func(hold map[string]any) { hold["index"] = 9 },
		"next":                func(hold map[string]any) { hold["next"] = -1 },
		"foreign-account":     func(hold map[string]any) { hold["tasks"].([]any)[0].(map[string]any)["account_id"] = "outside-account" },
		"foreign-family":      func(hold map[string]any) { hold["tasks"].([]any)[0].(map[string]any)["key"] = "outside-task" },
		"forged-confirmation": func(hold map[string]any) { hold["confirmed"] = map[string]bool{"outside-proof": true} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := newOrderedFixture()
			f.failKey, f.failMode = "growth/live", "unknown"
			path := filepath.Join(t.TempDir(), "sequence.json")
			e := openOrdered(t, path, f)
			r := submitOrdered(t, e, []string{"growth/", "makeup"})
			_ = e.Close()
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]any
			if err = json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			work := state["work"].(map[string]any)[r.ID].(map[string]any)
			hold := work["sequence_holds"].(map[string]any)["account-a"].(map[string]any)
			change(hold)
			raw, _ = json.Marshal(state)
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if reopened, err := taskengine.Open(taskengine.Options{Path: path}); err == nil {
				_ = reopened.Close()
				t.Fatal("tampered sequence was accepted")
			}
			if _, err = os.Stat(path + ".corrupt"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("semantic checkpoint failure was silently reset")
			}
		})
	}
}

func TestTaskSequenceValidationRejectsAmbiguousOrUnsafeSelectors(t *testing.T) {
	e := openOrdered(t, filepath.Join(t.TempDir(), "sequence.json"), newOrderedFixture())
	valid := []string{"travel/", "growth/", "redeem/"}
	_, err := e.SaveSchedule(taskengine.Schedule{ID: "valid", ProviderID: "sequence", TaskKey: "growth-sequence", TaskSequence: valid, Hours: []int{1}, Timezone: "UTC"})
	if err != nil {
		t.Fatalf("valid selectors rejected: %v", err)
	}
	bad := [][]string{{"*", "growth/"}, {"../outside"}, {"growth//task"}, {"growth/", "growth/"}, {"growth/", "growth/claim"}, {strings.Repeat("x", 513)}}
	long := make([]string, 17)
	for i := range long {
		long[i] = "task" + string(rune('a'+i))
	}
	bad = append(bad, long)
	for _, sequence := range bad {
		if _, err := e.Submit(orderedRequest(sequence, "account-a")); !errors.Is(err, taskengine.ErrInvalid) {
			t.Fatalf("unsafe sequence accepted: %v", sequence)
		}
		if _, err := e.SaveSchedule(taskengine.Schedule{ID: "bad", ProviderID: "sequence", TaskKey: "growth-sequence", TaskSequence: sequence, Hours: []int{1}, Timezone: "UTC"}); !errors.Is(err, taskengine.ErrInvalid) {
			t.Fatalf("unsafe schedule accepted: %v", sequence)
		}
	}
}

func TestOrderedTaskSequencePreservesOrdinaryWildcardAndPrefixRequests(t *testing.T) {
	for _, selector := range []string{"", "*", "growth/", "travel/arrive"} {
		t.Run(selector, func(t *testing.T) {
			e := openOrdered(t, filepath.Join(t.TempDir(), "sequence.json"), newOrderedFixture())
			r, err := e.Submit(taskengine.Request{ProviderID: "sequence", TaskKey: selector, AccountIDs: []string{"account-a"}})
			if err != nil {
				t.Fatalf("ordinary selector no longer accepted: %v", err)
			}
			if final := awaitBehaviorRun(t, e, r.ID); final.Status != taskengine.StatusSucceeded {
				t.Fatal(final.Status)
			}
		})
	}
}
