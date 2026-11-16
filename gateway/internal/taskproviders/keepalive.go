package taskproviders

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

type keepaliveModule struct {
	moduleBase
	owner   *Accounts
	vault   *accountvault.Vault
	refresh func(context.Context, *auth.Auth) (auth.TokenUpdate, error)
}
type refreshRecord struct {
	Provider string           `json:"provider"`
	Account  string           `json:"account"`
	Before   string           `json:"before"`
	After    string           `json:"after"`
	Update   auth.TokenUpdate `json:"update"`
	Verified bool             `json:"verified"`
}

func newKeepaliveModule(a *Accounts, d taskengine.Definition) *keepaliveModule {
	m := &keepaliveModule{moduleBase: moduleBase{spec: ModuleSpec{ID: "keepalive", Key: "keepalive", Realms: []string{"cn", "global", "trae"}, RecoverSession: true}, definition: d}, owner: a, vault: a.options.RefreshVault, refresh: a.options.Refresh}
	if m.refresh == nil {
		m.refresh = func(ctx context.Context, account *auth.Auth) (auth.TokenUpdate, error) {
			if account.Realm() == "trae" {
				return (&trae.TaskClient{}).Refresh(ctx, account)
			}
			return a.Upstream.TaskRefresh(ctx, account)
		}
	}
	return m
}
func refreshRecordID(provider, id string) string {
	return fmt.Sprintf("refresh-%x", sha256.Sum256([]byte(provider+"\x00"+id)))
}
func (m *keepaliveModule) load(a *auth.Auth) (refreshRecord, uint64, error) {
	var record refreshRecord
	if m.vault == nil {
		return record, 0, errors.New("refresh_store_unavailable")
	}
	rows, err := m.vault.List()
	if err != nil {
		return record, 0, errors.New("refresh_store_unavailable")
	}
	if len(rows) > 1000 {
		return record, 0, errors.New("refresh_store_limit")
	}
	id := refreshRecordID(m.owner.Provider, a.UID)
	for _, row := range rows {
		if row.ID != id {
			continue
		}
		_, raw, err := m.vault.Get(id)
		if err != nil {
			return record, 0, errors.New("refresh_store_unavailable")
		}
		defer func() {
			for i := range raw {
				raw[i] = 0
			}
		}()
		if json.Unmarshal(raw, &record) != nil || record.Provider != m.owner.Provider || record.Account != a.UID || len(record.Before) != 64 || len(record.After) != 64 || strings.TrimSpace(record.Update.AccessToken) == "" || record.Update.ExpiresAt <= 0 {
			return refreshRecord{}, 0, errors.New("refresh_store_invalid")
		}
		return record, row.Revision, nil
	}
	return record, 0, nil
}
func (m *keepaliveModule) save(record refreshRecord, revision uint64) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return errors.New("refresh_store_invalid")
	}
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	_, err = m.vault.Put(accountvault.Account{ID: refreshRecordID(record.Provider, record.Account), Provider: record.Provider, Enabled: true}, raw, revision)
	if err != nil {
		return errors.New("refresh_store_failed")
	}
	return nil
}
func (m *keepaliveModule) DiscoverState(ctx context.Context, a *auth.Auth) ([]ModuleTask, error) {
	d := m.definition
	d.PeriodKey = a.CredentialRevision()
	o, err := m.inspect(ctx, a, taskengine.Task{Definition: d}, false)
	return []ModuleTask{{Definition: d, State: o}}, err
}
func (m *keepaliveModule) Inspect(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.inspect(ctx, a, t, true)
}
func (m *keepaliveModule) inspect(ctx context.Context, a *auth.Auth, t taskengine.Task, reconcile bool) (taskengine.Observation, error) {
	if ctx.Err() != nil {
		return taskengine.Observation{}, ctx.Err()
	}
	if m.vault == nil {
		return observation("manual", "会话安全存储尚未配置，不能自动刷新。"), nil
	}
	if strings.TrimSpace(a.RefreshToken) == "" || a.FilePath == "" {
		return observation("manual", "缺少可续期会话或安全凭据文件，请在官方服务重新登录。"), nil
	}
	record, revision, err := m.load(a)
	if err != nil {
		return taskengine.Observation{}, err
	}
	// A successful refresh may legitimately return the same token and expiry.
	// That response proves its original attempt, not all future keepalive runs.
	if record.Before == t.PeriodKey && (!record.Verified || record.After != record.Before || t.AcceptedStage != "" || t.PendingVerification) {
		current := m.owner.Pool.AuthByUID(a.UID)
		if current == nil {
			return observation("blocked", "账号已移除。"), nil
		}
		version := current.CredentialRevision()
		if version == record.Before && reconcile {
			if err = current.CommitTokenUpdate(record.Before, record.Update); err != nil {
				return taskengine.Observation{}, err
			}
			version = current.CredentialRevision()
		}
		if version != record.After {
			return observation("blocked", "账号在刷新期间发生变化，已保留原操作，请核验会话。"), nil
		}
		persisted, err := current.PersistedRevision()
		if err != nil || persisted != record.After {
			return observation("pending", "续期响应已安全保留，凭据文件尚待核验。"), err
		}
		if !record.Verified && reconcile {
			record.Verified = true
			if err = m.save(record, revision); err != nil {
				return taskengine.Observation{}, err
			}
		}
		if reconcile {
			m.owner.Pool.CompleteSessionRefresh(a.UID, current)
			m.owner.Pool.Flush()
		}
		return observation("done", "官方续期响应已核验，账号凭据已保存并重新读取确认。"), nil
	}
	if record.Before != "" && !record.Verified {
		return observation("blocked", "前次续期尚未确认，需先恢复或核验，不能再次刷新。"), nil
	}
	if t.PeriodKey != a.CredentialRevision() {
		return observation("blocked", "账号凭据版本已变化，请读取最新任务。"), nil
	}
	persisted, err := a.PersistedRevision()
	if err != nil {
		return observation("manual", "安全凭据文件不可读取，先修复本地存储。"), nil
	}
	if persisted != a.CredentialRevision() {
		return observation("blocked", "账号文件已由其他登录更新，请先重新载入账号。"), nil
	}
	return observation("ready", "通过官方接口续期一次，保存后核验；不生成内容、不领取奖励。"), nil
}
func (m *keepaliveModule) Execute(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	o, err := m.Inspect(ctx, a, t)
	if err != nil || o.Phase != "ready" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, o), err
	}
	current := m.owner.Pool.AuthByUID(a.UID)
	if current == nil || current.CredentialRevision() != t.PeriodKey || ctx.Err() != nil {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, observation("blocked", "账号已变化或操作已取消。")), ctx.Err()
	}
	sent := false
	accepted := false
	err = current.RefreshOnceContext(ctx, func(snapshot *auth.Auth) (auth.TokenUpdate, error) {
		if m.owner.Pool.AuthByUID(a.UID) != current {
			return auth.TokenUpdate{}, errors.New("account_identity_changed")
		}
		if _, err := m.owner.accountForTask(a.UID, true); err != nil {
			return auth.TokenUpdate{}, err
		}
		if snapshot.CredentialRevision() != t.PeriodKey || ctx.Err() != nil {
			return auth.TokenUpdate{}, errors.New("credential_revision_conflict")
		}
		_, revision, err := m.load(snapshot)
		if err != nil {
			return auth.TokenUpdate{}, err
		}
		sent = true
		update, err := m.refresh(ctx, snapshot)
		if err != nil {
			return auth.TokenUpdate{}, err
		}
		if strings.TrimSpace(update.AccessToken) == "" || update.ExpiresAt <= time.Now().Unix() {
			return auth.TokenUpdate{}, errors.New("credential_refresh_invalid")
		}
		next := snapshot.Snapshot()
		next.AccessToken = update.AccessToken
		if update.RefreshToken != "" {
			next.RefreshToken = update.RefreshToken
		}
		if update.Domain != "" {
			next.Domain = update.Domain
		}
		next.ExpiresAt = update.ExpiresAt
		next.BackfillRealm()
		record := refreshRecord{Provider: m.owner.Provider, Account: a.UID, Before: t.PeriodKey, After: next.CredentialRevision(), Update: update}
		if err = m.save(record, revision); err != nil {
			return auth.TokenUpdate{}, err
		}
		accepted = true
		if err = current.CommitTokenUpdate(record.Before, update); err != nil {
			return auth.TokenUpdate{}, err
		}
		return update, nil
	})
	if !sent {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, observation("pending", "其他刷新已完成或账号已变化，请重新核验。")), err
	}
	if err != nil {
		var rejected *upstream.TaskRefreshRejected
		if errors.As(err, &rejected) && rejected.Code == 12153 && m.owner.Pool.AuthByUID(a.UID) == current {
			m.owner.Pool.NoteSessionDead(a.UID)
			m.owner.Pool.Flush()
			return taskengine.Rejected(t, taskengine.StageExecute, 12153, observation("manual", "官方拒绝续期：会话已失效，连续失败将停用，请在官方服务重新登录。")), nil
		}
		return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "续期结果或凭据保存尚未确认，保留恢复记录，不能重复请求。")), err
	}
	if accepted {
		return taskengine.Accepted(t, taskengine.StageExecute, observation("pending", "官方续期已接受，正在核验本地持久凭据。")), nil
	}
	return taskengine.UnknownAction(t, taskengine.StageExecute, o), nil
}
func (m *keepaliveModule) Verify(ctx context.Context, a *auth.Auth, t taskengine.Task) (taskengine.Observation, error) {
	return m.Inspect(ctx, a, t)
}
