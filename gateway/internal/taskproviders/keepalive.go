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
	Version   int              `json:"version,omitempty"`
	Reference string           `json:"reference,omitempty"`
	State     string           `json:"state,omitempty"`
	Provider  string           `json:"provider"`
	Account   string           `json:"account"`
	Before    string           `json:"before"`
	After     string           `json:"after"`
	Update    auth.TokenUpdate `json:"update"`
	Verified  bool             `json:"verified"`
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
		if json.Unmarshal(raw, &record) != nil || record.Provider != m.owner.Provider || record.Account != a.UID || !refreshDigest(record.Before) {
			return refreshRecord{}, 0, errors.New("refresh_store_invalid")
		}
		if record.Version == 0 {
			// Legacy records remain readable for diagnosis, never authoritative
			// for a pending attempt: Before/After alone cannot identify a request.
			if !refreshDigest(record.After) || strings.TrimSpace(record.Update.AccessToken) == "" || record.Update.ExpiresAt <= 0 {
				return refreshRecord{}, 0, errors.New("refresh_store_invalid")
			}
		} else if record.Version != 2 || !refreshDigest(record.Reference) {
			return refreshRecord{}, 0, errors.New("refresh_store_invalid")
		} else {
			switch record.State {
			case "prepared", "rejected":
				if record.After != "" || record.Update != (auth.TokenUpdate{}) || record.Verified {
					return refreshRecord{}, 0, errors.New("refresh_store_invalid")
				}
			case "accepted":
				if !refreshDigest(record.After) || strings.TrimSpace(record.Update.AccessToken) == "" || record.Update.ExpiresAt <= 0 {
					return refreshRecord{}, 0, errors.New("refresh_store_invalid")
				}
			default:
				return refreshRecord{}, 0, errors.New("refresh_store_invalid")
			}
		}
		return record, row.Revision, nil
	}
	return record, 0, nil
}
func refreshDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (m *keepaliveModule) save(record refreshRecord, revision uint64) (uint64, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return 0, errors.New("refresh_store_invalid")
	}
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	stored, err := m.vault.Put(accountvault.Account{ID: refreshRecordID(record.Provider, record.Account), Provider: record.Provider, Enabled: true}, raw, revision)
	if err != nil {
		return 0, errors.New("refresh_store_failed")
	}
	return stored.Revision, nil
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
	reference := taskengine.ActionReference(t, taskengine.StageExecute)
	if record.Version == 2 && reference != "" && record.Reference == reference && record.Before == t.PeriodKey && record.State == "accepted" {
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
			if _, err = m.save(record, revision); err != nil {
				return taskengine.Observation{}, err
			}
		}
		if reconcile {
			m.owner.Pool.CompleteSessionRefresh(a.UID, current)
			m.owner.Pool.Flush()
		}
		return observation("done", "官方续期响应已核验，账号凭据已保存并重新读取确认。"), nil
	}
	if t.PendingVerification {
		return observation("pending", "本次续期缺少匹配的接受响应，历史成功不能确认本次结果。"), nil
	}
	if record.Before != "" && !record.Verified && record.State != "rejected" {
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
	reference := taskengine.ActionReference(t, taskengine.StageExecute)
	if reference == "" {
		return taskengine.NotSubmitted(t, taskengine.StageExecute, observation("blocked", "缺少本次执行身份，未提交续期。")), errors.New("refresh_attempt_missing")
	}
	sent := false
	accepted := false
	var reservation refreshRecord
	var reservationRevision uint64
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
		// Replace any historical proof before entering transport. If a crash
		// occurs before this write, the engine's new reference still cannot match
		// the old proof. If after it, prepared is explicitly not acceptance.
		reservation = refreshRecord{Version: 2, Reference: reference, State: "prepared", Provider: m.owner.Provider, Account: a.UID, Before: t.PeriodKey}
		reservationRevision, err = m.save(reservation, revision)
		if err != nil {
			return auth.TokenUpdate{}, err
		}
		if ctx.Err() != nil {
			reservation.State = "rejected"
			_, saveErr := m.save(reservation, reservationRevision)
			if saveErr != nil {
				return auth.TokenUpdate{}, saveErr
			}
			return auth.TokenUpdate{}, ctx.Err()
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
		record := reservation
		record.State = "accepted"
		record.After = next.CredentialRevision()
		record.Update = update
		if _, err = m.save(record, reservationRevision); err != nil {
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
			reservation.State = "rejected"
			if _, saveErr := m.save(reservation, reservationRevision); saveErr != nil {
				return taskengine.UnknownAction(t, taskengine.StageExecute, observation("pending", "拒绝结果尚未安全保存，保留核验状态。")), saveErr
			}
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
