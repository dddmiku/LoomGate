package doubao

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
)

type ConnectionProof struct {
	Binding RuntimeBinding
	BotID   string
	Models  int
}

func (ConnectionProof) String() string   { return "doubao.ConnectionProof[redacted]" }
func (ConnectionProof) GoString() string { return "doubao.ConnectionProof[redacted]" }

type ConnectionRuntime interface {
	Bind(context.Context) (RuntimeBinding, error)
	Connection(context.Context, RuntimeBinding) (ConnectionProof, error)
}

func (r *CommandRuntime) Connection(ctx context.Context, expected RuntimeBinding) (ConnectionProof, error) {
	if expected.Identity != "" && !identityDigest.MatchString(expected.Identity) {
		return ConnectionProof{}, runtimeError("desktop_binding_invalid")
	}
	reply, err := r.run(ctx, runtimeRequest{Operation: "connection", Port: r.port, Identity: expected.Identity})
	if err != nil {
		return ConnectionProof{}, err
	}
	var data struct {
		BotID  string `json:"bot_id"`
		Models int    `json:"model_count"`
	}
	if !reply.Signed || !identityDigest.MatchString(reply.Identity) || json.Unmarshal(reply.Data, &data) != nil || !runtimeBotID.MatchString(data.BotID) || data.Models < 1 || data.Models > 4096 || expected.Identity != "" && expected.Identity != reply.Identity {
		return ConnectionProof{}, runtimeError("desktop_protocol_invalid")
	}
	return ConnectionProof{Binding: RuntimeBinding{Identity: reply.Identity}, BotID: data.BotID, Models: data.Models}, nil
}

// DesktopConnection shares the account vault with the real provider. Flow
// secrets contain identity digests and CAS snapshots, never upstream cookies.
type DesktopConnection struct {
	Vault    *accountvault.Vault
	Runtime  ConnectionRuntime
	Fallback authflow.Driver
	Now      func() time.Time
	mu       sync.Mutex
}
type bindingIntent struct {
	Nonce             string    `json:"nonce"`
	Target            string    `json:"target,omitempty"`
	ExpectedRevision  uint64    `json:"expected_revision"`
	ExpectedIdentity  string    `json:"expected_identity,omitempty"`
	CandidateIdentity string    `json:"candidate_identity,omitempty"`
	CandidateBot      string    `json:"candidate_bot,omitempty"`
	VerifiedAt        time.Time `json:"verified_at"`
}

func (bindingIntent) String() string { return "bindingIntent[redacted]" }
func (d *DesktopConnection) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}
func (d *DesktopConnection) Methods() []authflow.Method {
	methods := []authflow.Method{{ID: "desktop", Name: "连接已登录的官方客户端", Available: d.Runtime != nil, InputMode: "official", Reason: "official_desktop_connection_required"}}
	if d.Runtime != nil {
		methods[0].Reason = ""
	}
	if d.Fallback != nil {
		methods = append(methods, d.Fallback.Methods()...)
	}
	return methods
}
func (d *DesktopConnection) profile(id string) (accountvault.Account, StoredProfile, error) {
	a, b, err := d.Vault.Get(id)
	if err != nil {
		return a, StoredProfile{}, err
	}
	defer func() {
		for i := range b {
			b[i] = 0
		}
	}()
	var profile StoredProfile
	if a.Provider != "doubao" || json.Unmarshal(b, &profile) != nil {
		return a, profile, errors.New("account_binding_invalid")
	}
	return a, profile, nil
}
func intentResult(status, code, notice string, intent bindingIntent) authflow.Result {
	raw, _ := json.Marshal(intent)
	return authflow.Result{Status: status, ErrorCode: code, Notice: notice, Secret: raw}
}
func (d *DesktopConnection) initial(r authflow.Request) (bindingIntent, error) {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return bindingIntent{}, err
	}
	intent := bindingIntent{Nonce: hex.EncodeToString(token), Target: r.AccountID}
	if r.AccountID != "" {
		a, p, err := d.profile(r.AccountID)
		if err != nil || p.Runtime == nil || !identityDigest.MatchString(p.Runtime.Identity) {
			return intent, errors.New("account_binding_invalid")
		}
		if !a.Enabled {
			return intent, errors.New("account_disabled")
		}
		intent.ExpectedRevision = a.Revision
		intent.ExpectedIdentity = p.Runtime.Identity
	}
	return intent, nil
}
func (d *DesktopConnection) Start(ctx context.Context, r authflow.Request) (authflow.Result, error) {
	if r.Method != "desktop" {
		if d.Fallback == nil {
			return authflow.Result{}, authflow.ErrInvalid
		}
		return d.Fallback.Start(ctx, r)
	}
	if r.Provider != "doubao" || r.Realm != "" || d.Runtime == nil || d.Vault == nil {
		return authflow.Result{}, authflow.ErrInvalid
	}
	intent, err := d.initial(r)
	if err != nil {
		return authflow.Result{Status: "failed", ErrorCode: "account_binding_invalid", Notice: "目标账号已停用、被移除或不属于客户端绑定；请先核对账号状态。"}, nil
	}
	return d.verify(ctx, intent)
}
func (d *DesktopConnection) CanVerify(f authflow.Flow) bool {
	return f.Method == "desktop" && d.Runtime != nil
}
func (d *DesktopConnection) Verify(ctx context.Context, f authflow.Flow) (authflow.Result, error) {
	if !d.CanVerify(f) {
		return authflow.Result{}, authflow.ErrInvalid
	}
	var intent bindingIntent
	if len(f.Secret) == 0 {
		var err error
		intent, err = d.initial(f.Request)
		if err != nil {
			return authflow.Result{Status: "failed", ErrorCode: "account_binding_invalid"}, nil
		}
	} else if json.Unmarshal(f.Secret, &intent) != nil {
		return authflow.Result{}, authflow.ErrInvalid
	}
	// Recovery is read-only. A previously committed binding can be recognized,
	// but the service still requires a separate commit action to end the flow.
	return d.verify(ctx, intent)
}
func (d *DesktopConnection) verify(ctx context.Context, intent bindingIntent) (authflow.Result, error) {
	expected := intent.ExpectedIdentity
	if expected == "" {
		expected = intent.CandidateIdentity
	}
	proof, err := d.Runtime.Connection(ctx, RuntimeBinding{Identity: expected})
	if err != nil {
		code := "desktop_unavailable"
		var e *APIError
		if errors.As(err, &e) {
			code = e.Kind
		}
		notice := "官方客户端连接不可用。请打开客户端并恢复本机连接，再点“重新核验”。"
		switch code {
		case "desktop_login_required":
			notice = "请在豆包官方客户端使用手机号或扫码完成登录；验证码、扫码确认和额外验证都在官方界面操作，然后重新核验。"
		case "desktop_identity_mismatch":
			notice = "官方客户端当前账号与此流程不符。请切回原账号；如要添加另一个账号，请取消此流程后新建连接。"
		case "desktop_busy":
			notice = "客户端连接正在使用，请稍后重新核验。"
		case "desktop_version_unsupported":
			notice = "当前官方客户端版本尚未适配，请更新连接组件。"
		}
		return intentResult("waiting_authorization", code, notice, intent), nil
	}
	if !identityDigest.MatchString(proof.Binding.Identity) || !runtimeBotID.MatchString(proof.BotID) || proof.Models < 1 || proof.Models > 4096 || expected != "" && expected != proof.Binding.Identity {
		return intentResult("waiting_authorization", "desktop_protocol_invalid", "官方身份或目录核验结果不完整，请重新核验。", intent), nil
	}
	if ctx.Err() != nil {
		return authflow.Result{}, ctx.Err()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if intent.Target != "" && intent.ExpectedRevision == 0 {
		rows, err := d.Vault.List()
		if err != nil {
			return authflow.Result{}, err
		}
		exists := false
		for _, a := range rows {
			exists = exists || a.ID == intent.Target
		}
		if !exists {
			intent.Target = ""
		}
	}
	if intent.Target != "" {
		a, p, err := d.profile(intent.Target)
		if err != nil || !a.Enabled || p.Runtime == nil || p.Runtime.Identity != proof.Binding.Identity {
			return intentResult("waiting_authorization", "account_identity_mismatch", "账号绑定已变化或被停用，未覆盖账号。", intent), nil
		}
		if a.Revision != intent.ExpectedRevision && p.BindingFlow != intent.Nonce {
			return intentResult("waiting_authorization", "account_revision_conflict", "账号在核验期间被修改，请取消后重新连接。", intent), nil
		}
	} else {
		rows, err := d.Vault.List()
		if err != nil {
			return authflow.Result{}, err
		}
		matches := 0
		for _, a := range rows {
			if a.Provider != "doubao" {
				continue
			}
			_, p, err := d.profile(a.ID)
			if err != nil {
				return authflow.Result{}, err
			}
			if p.Runtime != nil && p.Runtime.Identity == proof.Binding.Identity {
				matches++
				intent.Target = a.ID
				intent.ExpectedRevision = a.Revision
				intent.ExpectedIdentity = proof.Binding.Identity
				if !a.Enabled {
					return intentResult("waiting_authorization", "account_disabled", "此账号已有绑定但已停用，请先在账号列表启用。", intent), nil
				}
			}
		}
		if matches > 1 {
			return intentResult("waiting_authorization", "duplicate_account_binding", "检测到重复账号绑定，请先整理账号后再连接。", intent), nil
		}
		if matches == 0 {
			intent.Target = "doubao-" + proof.Binding.Identity[:20]
			for _, a := range rows {
				if a.ID == intent.Target {
					return intentResult("waiting_authorization", "account_identity_mismatch", "目标账号标识已存在且身份不同，未覆盖。", intent), nil
				}
			}
		}
	}
	intent.CandidateIdentity = proof.Binding.Identity
	intent.CandidateBot = proof.BotID
	intent.VerifiedAt = d.now()
	result := intentResult("verified", "", "当前官方账号与模型目录已核验。保存将建立加密账号绑定，不复制官方 Cookie。", intent)
	result.AccountID = intent.Target
	return result, nil
}
func (d *DesktopConnection) Step(ctx context.Context, f authflow.Flow, action string, input authflow.Input) (authflow.Result, error) {
	if f.Method != "desktop" {
		if d.Fallback == nil {
			return authflow.Result{}, authflow.ErrInvalid
		}
		return d.Fallback.Step(ctx, f, action, input)
	}
	if action != "commit" || input.Phone != "" || input.Code != "" || input.Callback != "" {
		return authflow.Result{}, authflow.ErrInvalid
	}
	var intent bindingIntent
	if json.Unmarshal(f.Secret, &intent) != nil || intent.Nonce == "" || intent.Target == "" || !identityDigest.MatchString(intent.CandidateIdentity) {
		return authflow.Result{}, authflow.ErrInvalid
	}
	// Repeat official identity+signed directory verification immediately before CAS.
	proof, err := d.Runtime.Connection(ctx, RuntimeBinding{Identity: intent.CandidateIdentity})
	if err != nil {
		return intentResult("waiting_authorization", "desktop_unavailable", "保存前连接核验未完成，请恢复官方客户端后重新核验；账号未写入。", intent), nil
	}
	if proof.Binding.Identity != intent.CandidateIdentity || !identityDigest.MatchString(proof.Binding.Identity) || !runtimeBotID.MatchString(proof.BotID) || proof.Models < 1 || proof.Models > 4096 {
		return intentResult("waiting_authorization", "account_identity_mismatch", "保存前官方账号或目录已变化，未保存。", intent), nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.Vault.List()
	if err != nil {
		return authflow.Result{}, err
	}
	a := accountvault.Account{ID: intent.Target, Provider: "doubao", Enabled: true}
	stored := StoredProfile{}
	expected := uint64(0)
	for _, row := range rows {
		if row.Provider != "doubao" && row.ID != intent.Target {
			continue
		}
		existing, p, err := d.profile(row.ID)
		if err != nil {
			return authflow.Result{}, err
		}
		if row.ID != intent.Target {
			if p.Runtime != nil && p.Runtime.Identity == proof.Binding.Identity {
				return intentResult("waiting_authorization", "duplicate_account_binding", "同一账号已在另一个记录中绑定，未创建重复账号。", intent), nil
			}
			continue
		}
		if p.Runtime == nil || p.Runtime.Identity != proof.Binding.Identity || !existing.Enabled {
			return intentResult("waiting_authorization", "account_identity_mismatch", "目标账号身份或启用状态已变化，未覆盖。", intent), nil
		}
		if p.BindingFlow == intent.Nonce {
			return authflow.Result{Status: "succeeded", AccountID: existing.ID, Notice: "此前保存已完成，现已重新核验。"}, nil
		}
		if existing.Revision != intent.ExpectedRevision {
			return intentResult("waiting_authorization", "account_revision_conflict", "账号已被其他操作修改，未覆盖。", intent), nil
		}
		a = existing
		stored = p
		expected = existing.Revision
	}
	if expected == 0 && intent.ExpectedRevision != 0 {
		return intentResult("waiting_authorization", "account_revision_conflict", "原账号已不存在，未新建替代账号。", intent), nil
	}
	if ctx.Err() != nil {
		return authflow.Result{}, ctx.Err()
	}
	stored.Runtime, stored.BotID = &proof.Binding, proof.BotID
	stored.BindingFlow, stored.VerifiedAt = intent.Nonce, d.now()
	raw, _ := json.Marshal(stored)
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	if _, err = d.Vault.Put(a, raw, expected); err != nil {
		return authflow.Result{}, err
	}
	return authflow.Result{Status: "succeeded", AccountID: a.ID, Notice: "客户端账号绑定已保存。官方客户端管理登录会话，切换账号后请重新核验。"}, nil
}

type ConnectionAccount struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}
type ConnectionStatus struct {
	Configured bool                `json:"configured"`
	State      string              `json:"state"`
	Notice     string              `json:"notice"`
	CheckedAt  time.Time           `json:"checked_at"`
	Accounts   []ConnectionAccount `json:"accounts"`
}

func (d *DesktopConnection) Status(ctx context.Context) (ConnectionStatus, error) {
	out := ConnectionStatus{Configured: d.Runtime != nil, State: "not_configured", CheckedAt: d.now(), Accounts: []ConnectionAccount{}, Notice: "尚未配置官方客户端连接。"}
	rows, err := d.Vault.List()
	if err != nil {
		return out, err
	}
	current := ""
	if d.Runtime != nil {
		ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		binding, err := d.Runtime.Bind(ctx)
		if err == nil && !identityDigest.MatchString(binding.Identity) {
			err = runtimeError("desktop_protocol_invalid")
		}
		if err == nil {
			current = binding.Identity
			out.State = "unbound"
			out.Notice = "检测到已登录的官方客户端，可连接当前账号。"
		} else {
			out.State = "disconnected"
			out.Notice = "官方客户端连接不可用，请恢复本机连接后重新核验。"
			var e *APIError
			if errors.As(err, &e) {
				switch e.Kind {
				case "desktop_login_required":
					out.State = "login_required"
					out.Notice = "请在官方客户端用手机号或扫码登录。"
				case "desktop_busy":
					out.State = "busy"
					out.Notice = "客户端正在处理其他请求，请稍后刷新。"
				case "desktop_version_unsupported":
					out.State = "unsupported"
					out.Notice = "当前官方客户端版本尚未适配。"
				}
			}
		}
	}
	matches := 0
	for _, a := range rows {
		if a.Provider != "doubao" {
			continue
		}
		_, p, err := d.profile(a.ID)
		if err != nil {
			return out, err
		}
		row := ConnectionAccount{ID: a.ID, State: "legacy_session"}
		if !p.VerifiedAt.IsZero() {
			at := p.VerifiedAt
			row.VerifiedAt = &at
		}
		if p.Runtime != nil {
			row.State = "connection_unavailable"
			if current != "" {
				row.State = "switch_required"
				if p.Runtime.Identity == current {
					row.State = "ready"
					if a.Enabled {
						matches++
					}
				}
			}
		}
		if !a.Enabled {
			row.State = "disabled"
		}
		out.Accounts = append(out.Accounts, row)
	}
	if matches == 1 {
		out.State = "ready"
		out.Notice = "当前官方账号与已启用绑定一致。其他账号需在官方客户端切换后使用。"
	} else if matches > 1 {
		out.State = "conflict"
		out.Notice = "同一官方身份存在多个已启用绑定，请先整理账号。"
		for i := range out.Accounts {
			if out.Accounts[i].State == "ready" {
				out.Accounts[i].State = "conflict"
			}
		}
	}
	return out, nil
}
func (d *DesktopConnection) Route(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/workbench/client" {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return true
	}
	if strings.TrimSpace(r.URL.RawQuery) != "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client_query"})
		return true
	}
	status, err := d.Status(r.Context())
	if err != nil {
		w.WriteHeader(503)
		json.NewEncoder(w).Encode(map[string]string{"error": "client_status_unavailable"})
		return true
	}
	json.NewEncoder(w).Encode(status)
	return true
}
