package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
)

// ManagedClient controls only its own official profile. Credentials stay in
// that protected profile; this interface never returns reusable session data.
type ManagedClient interface {
	Endpoint(context.Context, bool) (int, error)
	Login(context.Context, string, string, string) (string, string, error)
}

type ManagedCommand struct {
	command          []string
	root, executable string
	slot             chan struct{}
}

func NewManagedCommand(python, helper, root, executable string) (*ManagedCommand, error) {
	if !filepath.IsAbs(python) || !filepath.IsAbs(helper) || !filepath.IsAbs(root) || executable != "" && !filepath.IsAbs(executable) {
		return nil, errors.New("desktop_configuration_invalid")
	}
	if info, err := os.Stat(helper); err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 {
		return nil, errors.New("desktop_configuration_invalid")
	}
	if err := accountvault.ProtectDirectory(root); err != nil {
		return nil, err
	}
	return &ManagedCommand{command: []string{python, "-I", "-B", helper}, root: root, executable: executable, slot: make(chan struct{}, 1)}, nil
}

type managedRequest struct {
	Action     string `json:"action"`
	Root       string `json:"root"`
	Executable string `json:"executable,omitempty"`
	Nonce      string `json:"nonce,omitempty"`
	Method     string `json:"method,omitempty"`
}
type managedData struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Port   int    `json:"port,omitempty"`
}

func (m *ManagedCommand) call(ctx context.Context, request managedRequest) (managedData, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	select {
	case m.slot <- struct{}{}:
		defer func() { <-m.slot }()
	case <-ctx.Done():
		return managedData{}, ctx.Err()
	}
	request.Root, request.Executable = m.root, m.executable
	raw, _ := json.Marshal(request)
	cmd := exec.CommandContext(ctx, m.command[0], m.command[1:]...)
	configureDesktopProcess(cmd)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 500 * time.Millisecond
	var output boundedOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return managedData{}, errors.New("desktop_management_unavailable")
	}
	var reply runtimeReply
	d := json.NewDecoder(&output.buffer)
	d.DisallowUnknownFields()
	if output.exceeded || d.Decode(&reply) != nil || d.Decode(new(any)) != io.EOF {
		return managedData{}, runtimeError("desktop_protocol_invalid")
	}
	if !reply.OK {
		return managedData{}, managedError(reply.Error)
	}
	var data managedData
	if json.Unmarshal(reply.Data, &data) != nil {
		return data, runtimeError("desktop_protocol_invalid")
	}
	return data, nil
}
func managedError(code string) error {
	switch code {
	case "desktop_not_installed", "desktop_management_conflict", "desktop_management_invalid", "desktop_management_unavailable", "desktop_management_platform_unsupported", "desktop_login_attempt_missing", "desktop_login_attempt_conflict", "desktop_login_open_unconfirmed", "desktop_close_unconfirmed":
		return &APIError{Kind: code}
	default:
		return runtimeError(code)
	}
}
func (m *ManagedCommand) Endpoint(ctx context.Context, start bool) (int, error) {
	action := "status"
	if start {
		action = "ensure"
	}
	data, err := m.call(ctx, managedRequest{Action: action})
	if err != nil {
		return 0, err
	}
	if data.State != "running" || data.Port < 1 || data.Port > 65535 {
		return 0, runtimeError("desktop_protocol_invalid")
	}
	return data.Port, nil
}
func (m *ManagedCommand) Login(ctx context.Context, action, nonce, method string) (string, string, error) {
	data, err := m.call(ctx, managedRequest{Action: action, Nonce: nonce, Method: method})
	return data.State, data.Reason, err
}

// ManagedConnection adds attended official QR/SMS login to the same identity
// and revision-checked binding implementation. Its initial intent is persisted
// BEFORE the official window is opened.
type ManagedConnection struct {
	Binding *DesktopConnection
	Client  ManagedClient
}
type managedIntent struct {
	ExistingSession bool          `json:"existing_session,omitempty"`
	Binding         bindingIntent `json:"binding"`
	Nonce           string        `json:"nonce"`
	Method          string        `json:"method"`
}

func (d *ManagedConnection) Methods() []authflow.Method {
	return []authflow.Method{
		{ID: "qr", Name: "官方扫码登录", Available: d.Client != nil, InputMode: "official"},
		{ID: "sms", Name: "官方手机号登录", Available: d.Client != nil, InputMode: "official"},
		{ID: "desktop", Name: "连接已登录的官方客户端", Available: d.Client != nil, InputMode: "official"},
	}
}
func (d *ManagedConnection) PrepareStart(r authflow.Request) (json.RawMessage, error) {
	if r.Provider != "doubao" || r.Realm != "" || d.Client == nil || d.Binding == nil {
		return nil, authflow.ErrInvalid
	}
	if r.Method != "qr" && r.Method != "sms" && r.Method != "desktop" {
		return nil, authflow.ErrInvalid
	}
	b, err := d.Binding.initial(r)
	if err != nil {
		return nil, err
	}
	return json.Marshal(managedIntent{Binding: b, Nonce: b.Nonce, Method: r.Method})
}
func (d *ManagedConnection) Start(context.Context, authflow.Request) (authflow.Result, error) {
	return authflow.Result{}, authflow.ErrInvalid
}
func (d *ManagedConnection) StartPrepared(ctx context.Context, f authflow.Flow) (authflow.Result, error) {
	var st managedIntent
	if json.Unmarshal(f.Secret, &st) != nil || st.Nonce == "" {
		return authflow.Result{}, authflow.ErrInvalid
	}
	if _, err := d.Client.Endpoint(ctx, true); err != nil {
		code := "desktop_management_unavailable"
		notice := "无法启动独立官方客户端，请检查是否已安装豆包和连接组件；未发起登录。"
		var api *APIError
		if errors.As(err, &api) && api.Kind == "desktop_management_platform_unsupported" {
			// managed.py owns a Windows desktop client; on other hosts it
			// refuses before touching a profile or opening a window. Say that
			// plainly instead of asking the user to reinstall a client that
			// this platform can never launch.
			code = api.Kind
			notice = "当前网关平台不能启动豆包官方客户端；请使用 Windows 桌面版网关，或在已运行官方客户端的本机配置回环调试端口后连接现有会话。"
		}
		return d.pending(st, "manual_action", code, notice), nil
	}
	if st.Method == "desktop" {
		return d.verifyBinding(ctx, st)
	}
	state, reason, err := d.Client.Login(ctx, "login_start", st.Nonce, st.Method)
	return d.loginResult(ctx, st, state, reason, err)
}
func (d *ManagedConnection) pending(st managedIntent, status, code, notice string) authflow.Result {
	raw, _ := json.Marshal(st)
	return authflow.Result{Status: status, ErrorCode: code, Notice: notice, Secret: raw, InputMode: "official"}
}
func (d *ManagedConnection) verifyBinding(ctx context.Context, st managedIntent) (authflow.Result, error) {
	result, err := d.Binding.verify(ctx, st.Binding)
	if err != nil {
		return result, err
	}
	if json.Unmarshal(result.Secret, &st.Binding) != nil {
		return authflow.Result{}, authflow.ErrInvalid
	}
	result.Secret, _ = json.Marshal(st)
	result.InputMode = "official"
	if result.Status == "verified" {
		result.Authentication = "official_login"
		if st.Method == "desktop" || st.ExistingSession {
			result.Authentication = "existing_session"
			result.Notice = "已核验现有官方会话。本次没有重新发送短信或创建扫码登录，保存将建立加密连接。"
		}
	}
	return result, nil
}
func (d *ManagedConnection) loginResult(ctx context.Context, st managedIntent, state, reason string, err error) (authflow.Result, error) {
	if err != nil {
		return d.pending(st, "uncertain", "official_login_unconfirmed", "官方登录状态未确认。请检查官方窗口后重新核验；不会重新打开二维码或发送短信。"), nil
	}
	switch state {
	case "verified":
		return d.verifyBinding(ctx, st)
	case "already_authenticated":
		st.ExistingSession = true
		return d.pending(st, "waiting_authorization", "official_already_authenticated", "独立客户端已有登录账号；本次没有发起新登录。可重新核验并保存现有连接，或在官方客户端切换账号。"), nil
	case "waiting_phone":
		return d.pending(st, "waiting_phone", "", "请在已打开的豆包官方窗口输入手机号并获取验证码，所有登录信息均留在官方界面。"), nil
	case "waiting_code":
		return d.pending(st, "waiting_code", "", "请在豆包官方窗口输入短信验证码并完成登录；不要把验证码发送到聊天。"), nil
	case "waiting_scan":
		return d.pending(st, "waiting_scan", "", "请在已打开的豆包官方窗口扫码，并在手机上确认。二维码仅显示在官方窗口。"), nil
	case "scanned":
		return d.pending(st, "scanned", "", "已扫码，请在手机上确认官方登录。"), nil
	case "expired":
		return d.pending(st, "expired", "official_login_expired", "官方登录已过期。需要时重新开始；不会自动刷新二维码或重发短信。"), nil
	case "canceled":
		return d.pending(st, "failed", "official_login_canceled", "此次官方登录已取消。"), nil
	case "manual_action":
		notice := "请在官方窗口完成所需操作后重新核验。"
		if reason == "official_code_rejected" {
			notice = "官方界面提示验证码无效或过期，请在官方窗口处理。不会自动重发短信。"
		}
		if reason == "official_verification_required" {
			notice = "请在豆包官方窗口完成安全验证，再返回核验。"
			return d.pending(st, "manual_action", "official_mfa_required", notice), nil
		}
		return d.pending(st, "manual_action", "official_action_required", notice), nil
	default:
		return d.pending(st, "uncertain", "official_state_unrecognized", "官方状态暂未识别，请在官方窗口核对后重新核验。"), nil
	}
}
func (d *ManagedConnection) CanVerify(f authflow.Flow) bool {
	return len(f.Secret) > 0 && d.Client != nil
}
func (d *ManagedConnection) Verify(ctx context.Context, f authflow.Flow) (authflow.Result, error) {
	var st managedIntent
	if json.Unmarshal(f.Secret, &st) != nil || st.Nonce == "" {
		return authflow.Result{}, authflow.ErrInvalid
	}
	// An explicit readback may recognize a completed login even if the opening
	// command lost its response. It never starts the client or opens a window.
	if st.Method == "desktop" || st.ExistingSession {
		return d.verifyBinding(ctx, st)
	}
	state, reason, pollErr := d.Client.Login(ctx, "login_poll", st.Nonce, st.Method)
	return d.loginResult(ctx, st, state, reason, pollErr)
}
func (d *ManagedConnection) Step(ctx context.Context, f authflow.Flow, action string, in authflow.Input) (authflow.Result, error) {
	if in.Phone != "" || in.Code != "" || in.Callback != "" {
		return authflow.Result{}, authflow.ErrInvalid
	}
	if action == "poll" {
		return d.Verify(ctx, f)
	}
	if action == "resume" {
		var st managedIntent
		if json.Unmarshal(f.Secret, &st) != nil || st.Nonce == "" {
			return authflow.Result{}, authflow.ErrInvalid
		}
		state, reason, err := d.Client.Login(ctx, "login_resume", st.Nonce, st.Method)
		return d.loginResult(ctx, st, state, reason, err)
	}
	if action != "commit" {
		return authflow.Result{}, authflow.ErrInvalid
	}
	var st managedIntent
	if json.Unmarshal(f.Secret, &st) != nil {
		return authflow.Result{}, authflow.ErrInvalid
	}
	f.Method = "desktop"
	f.Secret, _ = json.Marshal(st.Binding)
	result, err := d.Binding.Step(ctx, f, action, in)
	if len(result.Secret) > 0 {
		if json.Unmarshal(result.Secret, &st.Binding) != nil {
			return authflow.Result{}, authflow.ErrInvalid
		}
		result.Secret, _ = json.Marshal(st)
	}
	result.InputMode = "official"
	return result, err
}
func (d *ManagedConnection) Cancel(ctx context.Context, f authflow.Flow) error {
	var st managedIntent
	if json.Unmarshal(f.Secret, &st) != nil {
		return authflow.ErrInvalid
	}
	if st.Method == "desktop" || st.ExistingSession {
		return nil
	}
	_, _, err := d.Client.Login(ctx, "login_cancel", st.Nonce, st.Method)
	return err
}
