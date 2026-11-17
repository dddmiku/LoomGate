package taskproviders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
	"workbuddy2api/internal/taskengine"
)

type DoubaoWork struct {
	Vault    *accountvault.Vault
	Provider doubao.TaskWorkExecutor
	mu       sync.Mutex
}
type officialWorkReader interface {
	TaskWorkComplete(context.Context, string, string) (bool, error)
}

func (a *Doubao) reconcileWork(ctx context.Context, t taskengine.Task) error {
	if a.Work == nil {
		return nil
	}
	w := a.Work
	j, rev, err := w.journal(t)
	if err != nil {
		return nil
	}
	if j.Period != t.PeriodKey || j.Stage != taskengine.StageExecute || j.Conversation == "" || !workGenerationPending(j.State) {
		return nil
	}
	if !w.referenceMatches(t, j) {
		return nil
	}
	reader, ok := w.Provider.(officialWorkReader)
	if !ok {
		return nil
	}
	read, err := reader.TaskWorkComplete(ctx, t.AccountID, j.Conversation)
	if err != nil {
		return err
	}
	if !read {
		return nil
	}
	j.State = "generated"
	if j.Generate != nil && j.Generate.State == "unknown" {
		j.Generate.State = "accepted"
		j.Generate.Updated = time.Now().UTC()
	}
	w.mu.Lock()
	_, err = w.save(t, j, rev)
	w.mu.Unlock()
	return err
}

func workGenerationPending(state string) bool {
	switch state {
	case "uncertain", "attempting", "generate_unknown", "generated", "fact_attempting", "fact_unknown":
		return true
	}
	return false
}

type workInput struct {
	Account     string `json:"account"`
	Task        string `json:"task"`
	Model       string `json:"model"`
	Instruction string `json:"instruction"`
	Enabled     bool   `json:"enabled"`
}

// workStep records one non-idempotent sub-operation of a work task. The
// attempt is the engine-issued ActionReference for the enclosing stage; the
// state is written before the request and only authoritative provider
// evidence may advance it.
type workStep struct {
	Attempt      string    `json:"attempt,omitempty"`
	State        string    `json:"state"`
	Instance     string    `json:"instance,omitempty"`
	Message      string    `json:"message,omitempty"`
	Conversation string    `json:"conversation,omitempty"`
	Updated      time.Time `json:"updated,omitempty"`
}

type workJournal struct {
	Version         int              `json:"version,omitempty"`
	Account         string           `json:"account"`
	Task            string           `json:"task"`
	Period          string           `json:"period"`
	Attempt         string           `json:"attempt"`
	Stage           taskengine.Stage `json:"stage"`
	AccountRevision uint64           `json:"account_revision"`
	Instance        string           `json:"instance"`
	Started         time.Time        `json:"started"`
	State           string           `json:"state"`
	Before          []string         `json:"before,omitempty"`
	Confirmed       bool             `json:"confirmed"`
	RewardAmount    int64            `json:"reward_amount,omitempty"`
	LegacyWork      json.RawMessage  `json:"work,omitempty"`
	Conversation    string           `json:"conversation,omitempty"`
	Message         string           `json:"message,omitempty"`
	Start           *workStep        `json:"start,omitempty"`
	Generate        *workStep        `json:"generate,omitempty"`
	Fact            *workStep        `json:"fact,omitempty"`
	Claim           *workStep        `json:"claim,omitempty"`
}

func workKey(prefix, account, key string) string {
	sum := sha256.Sum256([]byte(account + "\x00" + key))
	return prefix + hex.EncodeToString(sum[:])
}
func (w *DoubaoWork) input(account, key string) (workInput, uint64, error) {
	var value workInput
	a, b, err := w.Vault.Get(workKey("dw-input-", account, key))
	if err != nil {
		return value, 0, err
	}
	defer func() {
		for i := range b {
			b[i] = 0
		}
	}()
	if json.Unmarshal(b, &value) != nil || value.Account != account || value.Task != key {
		return value, 0, errors.New("work_input_invalid")
	}
	return value, a.Revision, nil
}
func (w *DoubaoWork) journal(t taskengine.Task) (workJournal, uint64, error) {
	var value workJournal
	a, b, err := w.Vault.Get(workKey("dw-action-", t.AccountID, t.Key))
	if err != nil {
		return value, 0, err
	}
	defer func() {
		for i := range b {
			b[i] = 0
		}
	}()
	if json.Unmarshal(b, &value) != nil || value.Account != t.AccountID || value.Task != t.Key {
		return value, 0, errors.New("work_journal_invalid")
	}
	return value, a.Revision, nil
}
func (w *DoubaoWork) save(t taskengine.Task, v workJournal, rev uint64) (uint64, error) {
	v.Version = 2
	b, _ := json.Marshal(v)
	a, err := w.Vault.Put(accountvault.Account{ID: workKey("dw-action-", t.AccountID, t.Key), Provider: "doubao-work", Enabled: true}, b, rev)
	return a.Revision, err
}
func (w *DoubaoWork) referenceMatches(t taskengine.Task, j workJournal) bool {
	return j.Period == t.PeriodKey && j.Attempt != "" && j.Attempt == taskengine.ActionReference(t, j.Stage)
}
func (a *Doubao) workReady(t taskengine.Task, up doubao.Task) bool {
	if a.Work == nil || a.Work.Provider == nil || !supportedWork(up.Type) || up.Work == nil || !up.Work.Available {
		return false
	}
	input, _, err := a.Work.input(t.AccountID, t.Key)
	return err == nil && input.Enabled && input.Instruction != ""
}
func supportedWork(kind string) bool {
	switch kind {
	case "WorkTaskMode", "GenWord", "GenExcel", "GenPPT", "ScheduleSkill":
		return true
	}
	return false
}

func findActivityTask(activity doubao.Activity, code string) (doubao.Task, bool) {
	for _, candidate := range append(append([]doubao.Task{}, activity.Tasks...), activity.Milestones...) {
		if candidate.Code == code {
			return candidate, true
		}
	}
	return doubao.Task{}, false
}

func (a *Doubao) executeWork(ctx context.Context, t taskengine.Task, up doubao.Task) (taskengine.Observation, error) {
	w := a.Work
	if w == nil || w.Provider == nil || !supportedWork(up.Type) || up.Work == nil || !up.Work.Available {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{Phase: "manual"}), nil
	}
	w.mu.Lock()
	input, _, err := w.input(t.AccountID, t.Key)
	if err != nil || !input.Enabled || input.Instruction == "" {
		w.mu.Unlock()
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{Phase: "manual", Summary: "请先填写此任务的真实工作内容。"}), nil
	}
	old, rev, readErr := w.journal(t)
	if readErr == nil && old.Period == t.PeriodKey && old.State != "not_submitted" {
		w.mu.Unlock()
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "已有工作尝试，只读取结果，不重新生成。"}), taskengine.ErrUncertain
	}
	if readErr != nil && !errors.Is(readErr, accountvault.ErrNotFound) {
		w.mu.Unlock()
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{}), readErr
	}
	account, _, err := a.Vault.Get(t.AccountID)
	if err != nil || !account.Enabled {
		w.mu.Unlock()
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{}), errors.New("account_disabled")
	}
	session, err := (doubao.VaultSessions{Vault: a.Vault}).Session(ctx, t.AccountID)
	if err != nil || session.Revision != strconv.FormatUint(account.Revision, 10) {
		w.mu.Unlock()
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{Phase: "blocked"}), errors.New("account_identity_changed")
	}
	reference := taskengine.ActionReference(t, taskengine.StageExecute)
	if reference == "" {
		w.mu.Unlock()
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{}), taskengine.ErrInvalid
	}
	now := time.Now().UTC()
	journal := workJournal{
		Account: t.AccountID, Task: t.Key, Period: t.PeriodKey, Attempt: reference,
		Stage: taskengine.StageExecute, AccountRevision: account.Revision, Instance: up.ID,
		Started: now, State: "attempting",
		Start: &workStep{Attempt: reference, State: "attempting", Instance: up.ID, Updated: now},
	}
	before, err := a.Client.RewardRecords(ctx, t.AccountID)
	if err != nil {
		w.mu.Unlock()
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{}), err
	}
	for _, record := range before.Records {
		journal.Before = append(journal.Before, record.ID)
	}
	if rev, err = w.save(t, journal, rev); err != nil {
		w.mu.Unlock()
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{}), err
	}
	w.mu.Unlock()
	// task_start is the first upstream mutation. The journal above is durable
	// before it leaves the process, so every later failure is unknown, never
	// NotSubmitted.
	started, startErr := a.Client.TaskActionRevision(ctx, t.AccountID, session.Revision, "task_start", up)
	if startErr != nil {
		w.mu.Lock()
		journal.State = "start_unknown"
		if journal.Start != nil {
			journal.Start.State = "unknown"
			journal.Start.Updated = time.Now().UTC()
		}
		_, _ = w.save(t, journal, rev)
		w.mu.Unlock()
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "任务开始请求结果未知，保留待核验，不重新开始或生成。"}), startErr
	}
	candidate, ok := findActivityTask(started, up.Code)
	if !ok || candidate.Status != 4 || candidate.ID == "" {
		w.mu.Lock()
		journal.State = "start_unknown"
		if journal.Start != nil {
			journal.Start.State = "unknown"
			journal.Start.Updated = time.Now().UTC()
		}
		_, _ = w.save(t, journal, rev)
		w.mu.Unlock()
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "任务开始回执未显示进行中实例，保留待核验。"}), errors.New("work_start_unconfirmed")
	}
	journal.Instance = candidate.ID
	up.ID = candidate.ID
	journal.State = "started"
	journal.Start.State = "accepted"
	journal.Start.Instance = candidate.ID
	journal.Start.Updated = time.Now().UTC()
	w.mu.Lock()
	rev, err = w.save(t, journal, rev)
	w.mu.Unlock()
	if err != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "任务已开始但实例未能安全保存，保留待核验。"}), err
	}
	result, genErr := w.Provider.CompleteTask(ctx, doubao.TaskWorkRequest{AccountID: t.AccountID, Revision: session.Revision, Model: input.Model, Instruction: input.Instruction, ActionKey: reference, Task: up})
	now = time.Now().UTC()
	journal.Conversation = result.ConversationRef
	journal.Message = result.MessageID
	journal.Generate = &workStep{Attempt: reference, State: "unknown", Instance: candidate.ID, Message: result.MessageID, Conversation: result.ConversationRef, Updated: now}
	journal.State = "generate_unknown"
	if genErr == nil && result.Attempted && result.Acknowledged && result.Complete {
		journal.Generate.State = "accepted"
		journal.State = "generated"
	}
	w.mu.Lock()
	var saveErr error
	rev, saveErr = w.save(t, journal, rev)
	w.mu.Unlock()
	if saveErr != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "生成结果未能安全保存，保留待核验。"}), saveErr
	}
	observation := taskengine.Observation{Phase: "pending", Summary: "真实工作已提交，继续核验官方任务条件与奖励；生成结果不代表任务达标。"}
	if journal.State != "generated" {
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation), genErr
	}
	if journal.Message == "" {
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "生成已完成但缺少可核验的消息标识，保留待核验，不重发。"}), errors.New("work_message_id_missing")
	}
	// A fact report is its own non-idempotent operation. Persist it before the
	// request so a lost response can never cause a second report.
	now = time.Now().UTC()
	journal.Fact = &workStep{Attempt: reference, State: "attempting", Instance: journal.Instance, Message: journal.Message, Conversation: journal.Conversation, Updated: now}
	journal.State = "fact_attempting"
	w.mu.Lock()
	rev, saveErr = w.save(t, journal, rev)
	w.mu.Unlock()
	if saveErr != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "事实上报尝试未能持久化，未发送上报，保留待核验。"}), saveErr
	}
	fact, factErr := a.Client.TaskFactRevision(ctx, t.AccountID, session.Revision, doubao.Task{ID: journal.Instance, Group: up.Group, Code: up.Code}, journal.Message)
	if factErr != nil {
		w.mu.Lock()
		journal.State = "fact_unknown"
		journal.Fact.State = "unknown"
		journal.Fact.Updated = time.Now().UTC()
		_, _ = w.save(t, journal, rev)
		w.mu.Unlock()
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "任务事实上报结果未知，保留待核验，不重复上报。"}), factErr
	}
	confirmed, ok := findActivityTask(fact, up.Code)
	if !ok || (confirmed.ID != "" && confirmed.ID != journal.Instance) || (confirmed.Status != 2 && confirmed.Status != 3) {
		w.mu.Lock()
		journal.State = "fact_unknown"
		journal.Fact.State = "unknown"
		journal.Fact.Updated = time.Now().UTC()
		_, _ = w.save(t, journal, rev)
		w.mu.Unlock()
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "事实上报未获权威完成状态，保留待核验，不重复上报。"}), errors.New("work_fact_unconfirmed")
	}
	journal.State = "fact_accepted"
	journal.Fact.State = "accepted"
	journal.Fact.Updated = time.Now().UTC()
	w.mu.Lock()
	_, saveErr = w.save(t, journal, rev)
	w.mu.Unlock()
	if saveErr != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "官方已确认任务事实，但本地状态未能保存，保留待核验。"}), saveErr
	}
	return taskengine.Accepted(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending", Summary: "官方已确认任务事实，奖励进入核验。"}), nil
}

// resolveFact accepts an unconfirmed fact attempt only when the official task
// state itself proves completion for the same instance. No event is invented
// or re-sent.
func (a *Doubao) resolveFact(t taskengine.Task, current doubao.Task) error {
	w := a.Work
	if w == nil {
		return nil
	}
	j, rev, err := w.journal(t)
	if err != nil || j.Period != t.PeriodKey || j.Fact == nil || j.Fact.State == "accepted" || j.Start == nil || j.Start.State != "accepted" {
		return nil
	}
	if current.Status != 2 && current.Status != 3 {
		return nil
	}
	if current.ID != "" && j.Instance != "" && current.ID != j.Instance {
		return nil
	}
	j.State = "fact_accepted"
	j.Fact.State = "accepted"
	j.Fact.Instance = current.ID
	j.Fact.Updated = time.Now().UTC()
	w.mu.Lock()
	_, err = w.save(t, j, rev)
	w.mu.Unlock()
	return err
}

func (a *Doubao) rewardAction(ctx context.Context, t taskengine.Task, up doubao.Task, stage taskengine.Stage) (taskengine.Observation, error) {
	w := a.Work
	if w == nil {
		return taskengine.UnknownAction(t, stage, taskengine.Observation{Phase: "pending"}), errors.New("reward_verifier_unavailable")
	}
	before, err := a.Client.RewardRecords(ctx, t.AccountID)
	if err != nil {
		return taskengine.NotSubmitted(t, stage, taskengine.Observation{}), err
	}
	account, _, err := a.Vault.Get(t.AccountID)
	if err != nil || !account.Enabled {
		return taskengine.NotSubmitted(t, stage, taskengine.Observation{}), errors.New("account_disabled")
	}
	reference := taskengine.ActionReference(t, stage)
	if reference == "" {
		return taskengine.NotSubmitted(t, stage, taskengine.Observation{}), taskengine.ErrInvalid
	}
	w.mu.Lock()
	old, rev, readErr := w.journal(t)
	if readErr == nil && old.Period == t.PeriodKey && (old.Stage == stage || old.State == "attempting" || old.State == "uncertain") {
		w.mu.Unlock()
		return taskengine.UnknownAction(t, stage, taskengine.Observation{Phase: "pending"}), taskengine.ErrUncertain
	}
	if readErr != nil && !errors.Is(readErr, accountvault.ErrNotFound) {
		w.mu.Unlock()
		return taskengine.NotSubmitted(t, stage, taskengine.Observation{}), readErr
	}
	now := time.Now().UTC()
	journal := workJournal{
		Account: t.AccountID, Task: t.Key, Period: t.PeriodKey, Attempt: reference,
		Stage: stage, AccountRevision: account.Revision, Instance: up.ID, Started: now,
		State: "attempting", LegacyWork: old.LegacyWork, Conversation: old.Conversation,
		Start: old.Start, Generate: old.Generate, Fact: old.Fact,
	}
	if stage == taskengine.StageClaim {
		journal.Claim = &workStep{Attempt: reference, State: "attempting", Instance: up.ID, Updated: now}
	}
	for _, r := range before.Records {
		journal.Before = append(journal.Before, r.ID)
	}
	if rev, err = w.save(t, journal, rev); err != nil {
		w.mu.Unlock()
		return taskengine.NotSubmitted(t, stage, taskengine.Observation{}), err
	}
	w.mu.Unlock()
	operation := "task_claim"
	if stage == taskengine.StageExecute {
		operation = "task_checkin"
	}
	result, err := a.Client.TaskActionRevision(ctx, t.AccountID, strconv.FormatUint(account.Revision, 10), operation, up)
	now = time.Now().UTC()
	journal.State = "uncertain"
	if err == nil && result.OperationStatus == "submitted" {
		journal.State = "accepted"
	}
	step := journal.Claim
	if stage == taskengine.StageExecute {
		if journal.Start == nil {
			journal.Start = &workStep{Attempt: reference, Instance: up.ID, Updated: now}
		}
		step = journal.Start
	}
	if step != nil {
		step.Updated = now
		if journal.State == "accepted" {
			step.State = "accepted"
		} else {
			step.State = "unknown"
		}
	}
	w.mu.Lock()
	_, saveErr := w.save(t, journal, rev)
	w.mu.Unlock()
	if saveErr != nil {
		return taskengine.UnknownAction(t, stage, taskengine.Observation{Phase: "pending"}), saveErr
	}
	observation := taskengine.Observation{Phase: "pending", Summary: "提交已记录，等待同账号、任务及本次操作对应的奖励流水。"}
	if journal.State == "accepted" {
		return taskengine.Accepted(t, stage, observation), nil
	}
	return taskengine.UnknownAction(t, stage, observation), err
}
func (a *Doubao) verifyReward(ctx context.Context, t taskengine.Task, up doubao.Task, state taskengine.Observation) (taskengine.Observation, error) {
	if a.Work == nil {
		return state, nil
	}
	w := a.Work
	journal, rev, err := w.journal(t)
	if err != nil || !w.referenceMatches(t, journal) {
		if t.PendingVerification && state.Phase == "done" {
			return taskengine.Observation{Phase: "pending", Summary: "历史操作尚缺少可绑定到本次周期的奖励证据，保留待复核。"}, nil
		}
		return state, nil
	}
	if journal.State == "generated" && state.Phase != "done" {
		return state, nil
	}
	account, _, err := a.Vault.Get(t.AccountID)
	if err != nil || !account.Enabled || account.Revision != journal.AccountRevision {
		return taskengine.Observation{Phase: "pending"}, errors.New("account_identity_changed")
	}
	if journal.Instance != "" && up.ID != journal.Instance {
		return taskengine.Observation{Phase: "pending", Summary: "任务实例已改变，保留原操作待核验。"}, nil
	}
	if journal.Confirmed {
		state.Phase = "done"
		state.Summary = "已核验本次操作的任务状态及新增奖励流水。"
		return state, nil
	}
	if state.Phase != "done" {
		return state, nil
	}
	records, err := a.Client.RewardRecords(ctx, t.AccountID)
	if err != nil {
		return taskengine.Observation{Phase: "pending"}, err
	}
	seen := map[string]bool{}
	for _, id := range journal.Before {
		seen[id] = true
	}
	expected := int64(0)
	for _, reward := range up.Rewards {
		if reward.Type == "quota_grant" {
			expected += reward.Amount
		}
	}
	if up.Daily != nil {
		expected = up.Daily.Reward.Amount
	}
	found := int64(0)
	unit := ""
	for _, record := range records.Records {
		if record.ID == "" || seen[record.ID] || record.TaskCode != up.Code || record.CreatedMS < journal.Started.UnixMilli()-2000 || record.CreatedMS > time.Now().Add(2*time.Second).UnixMilli() {
			continue
		}
		seen[record.ID] = true
		if record.Unit == "" || unit != "" && unit != record.Unit {
			return taskengine.Observation{Phase: "pending", Summary: "奖励单位不一致，保留待核验。"}, nil
		}
		unit = record.Unit
		amount, e := strconv.ParseInt(record.Amount, 10, 64)
		if e == nil && amount > 0 && amount <= 1<<40 {
			found += amount
		}
	}
	if found <= 0 || expected > 0 && found < expected {
		return taskengine.Observation{Phase: "pending", Summary: "任务状态已完成，但本次奖励流水尚未核实；不会重复提交。"}, nil
	}
	journal.Confirmed = true
	journal.RewardAmount = found
	w.mu.Lock()
	_, err = w.save(t, journal, rev)
	w.mu.Unlock()
	if err != nil {
		return taskengine.Observation{Phase: "pending"}, err
	}
	state.Summary = "已核验本次操作的任务状态及新增奖励流水。"
	return state, nil
}
