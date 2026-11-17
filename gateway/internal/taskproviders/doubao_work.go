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
	if j.Period != t.PeriodKey || j.Stage != taskengine.StageExecute || j.Conversation == "" || j.State != "uncertain" && j.State != "attempting" {
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
	w.mu.Lock()
	_, err = w.save(t, j, rev)
	w.mu.Unlock()
	return err
}

type workInput struct {
	Account     string `json:"account"`
	Task        string `json:"task"`
	Model       string `json:"model"`
	Instruction string `json:"instruction"`
	Enabled     bool   `json:"enabled"`
}
type workJournal struct {
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
	if readErr == nil && old.Period == t.PeriodKey {
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
	reference := taskengine.ActionReference(t, taskengine.StageExecute)
	if reference == "" {
		w.mu.Unlock()
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{}), taskengine.ErrInvalid
	}
	journal := workJournal{Account: t.AccountID, Task: t.Key, Period: t.PeriodKey, Attempt: reference, Stage: taskengine.StageExecute, AccountRevision: account.Revision, Instance: up.ID, Started: time.Now(), State: "attempting"}
	before, err := a.Client.RewardRecords(ctx, t.AccountID)
	if err != nil {
		w.mu.Unlock()
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{}), err
	}
	for _, record := range before.Records {
		journal.Before = append(journal.Before, record.ID)
	}
	rev, err = w.save(t, journal, rev)
	w.mu.Unlock()
	if err != nil {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, taskengine.Observation{}), err
	}
	session, err := (doubao.VaultSessions{Vault: a.Vault}).Session(ctx, t.AccountID)
	if err != nil || session.Revision != strconv.FormatUint(account.Revision, 10) {
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending"}), errors.New("account_identity_changed")
	}
	result, err := w.Provider.CompleteTask(ctx, doubao.TaskWorkRequest{AccountID: t.AccountID, Revision: session.Revision, Model: input.Model, Instruction: input.Instruction, ActionKey: reference, Task: up})
	journal.State = "uncertain"
	journal.Conversation = result.ConversationRef
	if err == nil && result.Complete && result.Acknowledged {
		journal.State = "generated"
	}
	w.mu.Lock()
	_, saveErr := w.save(t, journal, rev)
	w.mu.Unlock()
	if saveErr != nil {
		return taskengine.UnknownAction(t, taskengine.StageExecute, taskengine.Observation{Phase: "pending"}), saveErr
	}
	observation := taskengine.Observation{Phase: "pending", Summary: "真实工作已提交，继续核验官方任务条件与奖励；生成结果不代表任务达标。"}
	if journal.State == "generated" {
		return taskengine.Accepted(t, taskengine.StageExecute, observation), nil
	}
	return taskengine.UnknownAction(t, taskengine.StageExecute, observation), err
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
	journal := workJournal{Account: t.AccountID, Task: t.Key, Period: t.PeriodKey, Attempt: reference, Stage: stage, AccountRevision: account.Revision, Instance: up.ID, Started: time.Now(), State: "attempting", LegacyWork: old.LegacyWork, Conversation: old.Conversation}
	for _, r := range before.Records {
		journal.Before = append(journal.Before, r.ID)
	}
	rev, err = w.save(t, journal, rev)
	w.mu.Unlock()
	if err != nil {
		return taskengine.NotSubmitted(t, stage, taskengine.Observation{}), err
	}
	operation := "task_claim"
	if stage == taskengine.StageExecute {
		operation = "task_checkin"
	}
	result, err := a.Client.TaskActionRevision(ctx, t.AccountID, strconv.FormatUint(account.Revision, 10), operation, up)
	journal.State = "uncertain"
	if err == nil && result.OperationStatus == "submitted" {
		journal.State = "accepted"
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
