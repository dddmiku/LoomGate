package autoclaw

// SMS login for AutoClaw, implemented in-process against the same upstream
// endpoints the desktop client uses:
//
//	POST /userapi/v1/agent-send-code  {phone, source_id, device_id}
//	POST /userapi/v1/agent-login      {phone, code, source_id, device_id}
//
// The login response carries an "agent" audience token; the AutoClaw channel
// needs the "autoclaw" audience, so the refresh endpoint is used once to swap
// it, exactly as the desktop gateway does. No client, no QR, no token import.

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
)

const (
	sendCodePath  = "/userapi/v1/agent-send-code"
	agentLoginPt  = "/userapi/v1/agent-login"
	loginDeviceID = "loomgate-autoclaw"

	// The fission (invite) endpoints live on the web host, not the acceleration
	// host, and need the web header set. Both facts come from the released
	// gateway; guessing "acceleration host with the client headers" 404s.
	webBase         = "autoglm-api.zhipuai.cn"
	fissionBindPt   = "/agent-assetmgr/api/v1/fission-v2/bind"
	fissionActivity = "autoclaw_fission"
	// DefaultInviteCode is the referral code a freshly created AutoClaw account
	// binds to. Binding is best effort: it never blocks or fails a login.
	DefaultInviteCode = "H2QARJKM"
)

// LoginDriver starts and completes a phone + SMS login for AutoClaw accounts.
type LoginDriver struct {
	// Save persists the verified account into the provider vault.
	Save func(ctx context.Context, profile []byte) (accountID string, err error)
	// HTTP is optional; tests inject a transport.
	HTTP *http.Client
	// InviteCode is bound after a successful login when set. Empty uses
	// DefaultInviteCode; "-" disables binding.
	InviteCode string
}

type loginState struct {
	Phone    string `json:"phone"`
	DeviceID string `json:"device_id"`
	// Stage tracks how far this flow has gone so a lost response before the code
	// was sent is never mistaken for one that was.
	Stage string `json:"stage"`
	// Profile is the login result awaiting commit. It lives only inside the
	// flow's encrypted Secret between submit_code and commit; the tokens reach
	// the vault on commit and never enter a public response.
	Profile json.RawMessage `json:"profile,omitempty"`
	// InviteCode is what the operator typed when starting the login. Empty
	// means "use the provider default"; it is carried in the encrypted secret
	// so the first step can collect it and commit can use it.
	InviteCode string `json:"invite_code,omitempty"`
}

func (d *LoginDriver) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (*LoginDriver) Methods() []authflow.Method {
	return []authflow.Method{{
		ID: "sms", Name: "手机号验证码", Available: true, InputMode: "sms",
	}}
}

// Start only asks for the phone number. authflow's Request carries no user
// input, so the number arrives on the first Step; the SMS side effect happens
// there, where the intent has already been persisted.
func (d *LoginDriver) Start(ctx context.Context, r authflow.Request) (authflow.Result, error) {
	// A per-flow device id, matching the released gateway's "acg-"+uuid shape.
	// It must be stable across send-code → login → refresh for one login, and
	// must differ between accounts: reusing one constant across every login
	// makes the upstream treat unrelated accounts as the same device.
	st := loginState{DeviceID: newDeviceID(), Stage: "awaiting_phone"}
	secret, _ := json.Marshal(st)
	return authflow.Result{
		Status: "waiting_phone", InputMode: "phone", Secret: secret,
		Notice: "请输入 AutoClaw 账号绑定的手机号（11 位）。",
	}, nil
}

// sendCodeFlow performs the SMS send after the phone has been validated.
func (d *LoginDriver) sendCodeFlow(ctx context.Context, phone, device string) (authflow.Result, error) {
	return d.sendCodeFlowWith(ctx, phone, device, "")
}

func (d *LoginDriver) sendCodeFlowWith(ctx context.Context, phone, device, inviteCode string) (authflow.Result, error) {
	st := loginState{Phone: phone, DeviceID: device, Stage: "code_requested", InviteCode: inviteCode}
	secret, _ := json.Marshal(st)
	if err := d.sendCode(ctx, phone, device); err != nil {
		var gate *sliderGate
		if errors.As(err, &gate) {
			return authflow.Result{
				Status: "manual_action", ErrorCode: "slider_required",
				Notice: "上游要求滑块验证，无法在本机自动完成。请改用 AutoClaw 客户端登录一次，再把凭证导入；或换一个网络环境重试。",
				Secret: secret,
			}, nil
		}
		return authflow.Result{
			Status: "failed", ErrorCode: "code_send_failed",
			Notice: "验证码发送失败：" + err.Error(), Secret: secret,
		}, nil
	}
	return authflow.Result{
		Status: "waiting_code", InputMode: "code", Secret: secret,
		Notice: "验证码已发送到 " + maskPhone(phone) + "，请输入收到的验证码。",
	}, nil
}

// ValidateStep runs before authflow records an SMS send and starts its
// cooldown. Rejected local input has not contacted the provider and must be
// immediately correctable. Keep it shared with direct driver callers.
func (*LoginDriver) ValidateStep(_ authflow.Flow, action string, input authflow.Input) *authflow.Result {
	if action != "send_code" && action != "phone" && action != "submit_phone" {
		return nil
	}
	if !isPhone(strings.TrimSpace(input.Phone)) {
		return &authflow.Result{Status: "waiting_phone", InputMode: "phone",
			Notice: "手机号格式不正确，请输入 11 位大陆手机号。"}
	}
	if code := strings.TrimSpace(input.InviteCode); code != "" && !isInviteCode(code) {
		return &authflow.Result{Status: "waiting_phone", InputMode: "phone",
			Notice: "邀请码格式不正确，请检查后重新输入，或留空使用默认邀请码。"}
	}
	return nil
}

func (d *LoginDriver) Step(ctx context.Context, f authflow.Flow, action string, input authflow.Input) (authflow.Result, error) {
	var st loginState
	if json.Unmarshal(f.Secret, &st) != nil {
		return authflow.Result{}, authflow.ErrInvalid
	}
	if st.DeviceID == "" {
		st.DeviceID = loginDeviceID
	}
	if validation := d.ValidateStep(f, action, input); validation != nil {
		return *validation, nil
	}
	switch action {
	case "send_code", "phone", "submit_phone":
		phone := strings.TrimSpace(input.Phone)
		// Optional referral code typed by the operator. Blank keeps the
		// provider default; it is remembered in the encrypted secret so the
		// commit step can bind it without asking again.
		if code := strings.TrimSpace(input.InviteCode); code != "" {
			st.InviteCode = code
		}
		return d.sendCodeFlowWith(ctx, phone, st.DeviceID, st.InviteCode)
	case "resend":
		if !isPhone(st.Phone) {
			return authflow.Result{Status: "waiting_phone", InputMode: "phone",
				Notice: "请先输入手机号。"}, nil
		}
		if err := d.sendCode(ctx, st.Phone, st.DeviceID); err != nil {
			return authflow.Result{Status: "failed", ErrorCode: "code_send_failed", Notice: "重新发送失败：" + err.Error()}, nil
		}
		b, _ := json.Marshal(st)
		return authflow.Result{Status: "waiting_code", InputMode: "code", Secret: b,
			Notice: "验证码已重新发送到 " + maskPhone(st.Phone) + "。"}, nil
	case "submit_code", "submit", "verify":
		code := strings.TrimSpace(input.Code)
		if !isCode(code) {
			return authflow.Result{Status: "waiting_code", InputMode: "code",
				Notice: "请输入 6 位数字验证码。"}, nil
		}
		profile, err := d.login(ctx, st.Phone, code, st.DeviceID)
		if err != nil {
			return authflow.Result{Status: "waiting_code", InputMode: "code",
				ErrorCode: "code_rejected", Notice: "验证码不正确或已过期：" + err.Error()}, nil
		}
		// Identity is proven but nothing is written yet. authflow requires an
		// explicit commit before an account exists, exactly as the Doubao
		// passport does; returning "succeeded" here is rejected as
		// authentication_commit_required and the flow lands on uncertain.
		st.Profile = profile
		b, _ := json.Marshal(st)
		return authflow.Result{Status: "verified", Secret: b,
			Notice: "验证码已通过。正在核验账号并保存…"}, nil
	case "commit":
		return d.commit(ctx, &st)
	default:
		return authflow.Result{}, authflow.ErrInvalid
	}
}

// commit persists the profile proven by submit_code and binds the referral
// code. It is idempotent: re-running it with the same phone updates the
// existing vault row instead of creating a second account.
func (d *LoginDriver) commit(ctx context.Context, st *loginState) (authflow.Result, error) {
	if len(st.Profile) == 0 {
		return authflow.Result{Status: "manual_action", ErrorCode: "resume_requires_login",
			Notice: "上次登录的凭证已失效，请重新获取验证码。"}, nil
	}
	if d.Save == nil {
		return authflow.Result{}, errors.New("autoclaw_login_storage_unavailable")
	}
	id, err := d.Save(ctx, st.Profile)
	if err != nil {
		return authflow.Result{}, err
	}
	notice := "登录成功，账号已保存到 AutoClaw 账号池。"
	if code := d.inviteCode(st.InviteCode); code != "" {
		var s stored
		if json.Unmarshal(st.Profile, &s) == nil {
			if bindErr := fissionBind(ctx, s.AccessToken, code, s.DeviceID, d.client()); bindErr != nil {
				// A failed referral bind must not fail the login: the account
				// already works for inference.
				notice += "（邀请码 " + code + " 绑定失败：" + bindErr.Error() + "）"
			} else {
				notice += "已绑定邀请码 " + code + "。"
			}
		}
	}
	st.Profile = nil
	return authflow.Result{Status: "succeeded", AccountID: id, Notice: notice}, nil
}

// inviteCode resolves the referral code to bind after login.
// inviteCode resolves the referral code to bind after login. A code the
// operator typed during this flow wins; otherwise the provider default
// applies. "-" disables binding for an explicitly opted-out deployment.
func (d *LoginDriver) inviteCode(userCode string) string {
	if code := strings.TrimSpace(userCode); code != "" {
		return code
	}
	code := strings.TrimSpace(d.InviteCode)
	if code == "-" {
		return ""
	}
	if code == "" {
		return DefaultInviteCode
	}
	return code
}

// fissionBind binds a referral code to the freshly logged-in account.
//
// This uses the web host and the "fission" header set rather than the
// acceleration host the chat path uses: the referral API belongs to the web
// product, and the client headers are rejected there.
func fissionBind(ctx context.Context, accessToken, inviteCode, deviceID string, client *http.Client) error {
	if accessToken == "" || inviteCode == "" {
		return errors.New("autoclaw_invite_bind_incomplete")
	}
	payload, _ := json.Marshal(map[string]any{
		"activity_id": fissionActivity,
		"business":    "autoclaw",
		"invite_code": inviteCode,
		"source_id":   "autoclaw",
		"device_id":   deviceID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+webBase+fissionBindPt, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	setWebHeaders(req, accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return errors.New("invite response unreadable")
	}
	if out.Code != 0 {
		return fmt.Errorf("code=%d %s", out.Code, out.Msg)
	}
	return nil
}

// setWebHeaders applies the browser-shaped header set the web/referral API
// expects, including the same appid signature the client endpoints use.
func setWebHeaders(req *http.Request, accessToken string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36")
	req.Header.Set("X-Channel", "fission")
	req.Header.Set("X-Product", "rumination")
	req.Header.Set("X-Tm", "web")
	req.Header.Set("X-Version", "1.47.0")
	req.Header.Set("X-Lang", "zh")
	req.Header.Set("X-Agent-OS", "web")
	req.Header.Set("Origin", "https://autoglm.zhipuai.cn")
	req.Header.Set("Referer", "https://autoglm.zhipuai.cn/")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	setSign(req)
}

// setSign stamps the appid/sign/timestamp triple shared by both header sets.
func setSign(req *http.Request) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sum := md5.Sum([]byte(appID + "&" + ts + "&" + appKey))
	req.Header.Set("x-auth-appid", appID)
	req.Header.Set("x-auth-sign", hex.EncodeToString(sum[:]))
	req.Header.Set("x-auth-timestamp", ts)
}

// sliderGate 表示上游要求人工滑块验证。
type sliderGate struct{}

func (*sliderGate) Error() string { return "autoclaw slider verification required" }

func (d *LoginDriver) sendCode(ctx context.Context, phone, device string) error {
	body, _ := json.Marshal(map[string]any{"phone": phone, "source_id": "autoclaw", "device_id": device})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiBase+sendCodePath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	setHeaders(req, "", "")
	req.Header.Del("X-Authorization")
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return fmt.Errorf("upstream response unreadable")
	}
	if out.Code == 630014 {
		return &sliderGate{}
	}
	if out.Code != 0 {
		return fmt.Errorf("code=%d %s", out.Code, out.Msg)
	}
	return nil
}

// login exchanges the SMS code for tokens and swaps the audience to autoclaw.
func (d *LoginDriver) login(ctx context.Context, phone, code, device string) ([]byte, error) {
	n, err := strconv.Atoi(code)
	if err != nil {
		return nil, errors.New("code invalid")
	}
	body, _ := json.Marshal(map[string]any{"phone": phone, "code": n, "source_id": "autoclaw", "device_id": device})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiBase+agentLoginPt, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setHeaders(req, "", "")
	req.Header.Del("X-Authorization")
	resp, err := d.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			UserID       string `json:"user_id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Code != 0 {
		return nil, fmt.Errorf("code=%d %s", out.Code, out.Msg)
	}
	if out.Data.AccessToken == "" || out.Data.RefreshToken == "" || out.Data.UserID == "" {
		return nil, errors.New("login response incomplete")
	}
	at, rt := out.Data.AccessToken, out.Data.RefreshToken
	// The agent token is not accepted by the autoclaw channel; swap the audience.
	if swappedAt, swappedRt, swapErr := refreshAudience(ctx, rt, device, d.client()); swapErr == nil && swappedAt != "" {
		at, rt = swappedAt, swappedRt
	}
	profile := stored{
		AccessToken:  at,
		RefreshToken: rt,
		DeviceID:     device,
		Phone:        phone,
		ExpiresAt:    tokenExpiry(at),
	}
	return json.Marshal(profile)
}

// refreshAudience exchanges a refresh token for the autoclaw audience.
func refreshAudience(ctx context.Context, refreshToken, device string, client *http.Client) (string, string, error) {
	body := map[string]any{"refresh_token": refreshToken, "source_id": "autoclaw"}
	if device != "" {
		body["device_id"] = device
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiBase+refreshPt, bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	setHeaders(req, "", "")
	req.Header.Del("X-Authorization")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Code int `json:"code"`
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &out) != nil || out.Code != 0 || out.Data.AccessToken == "" {
		return "", "", errors.New("audience swap failed")
	}
	return out.Data.AccessToken, out.Data.RefreshToken, nil
}

// SaveProfile builds the persistence callback for a vault.
func SaveProfile(vault *accountvault.Vault) func(context.Context, []byte) (string, error) {
	return func(ctx context.Context, profile []byte) (string, error) {
		if vault == nil {
			return "", errors.New("autoclaw_vault_unavailable")
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var s stored
		if json.Unmarshal(profile, &s) != nil || s.Phone == "" {
			return "", errors.New("autoclaw_profile_invalid")
		}
		// Keep physical legacy rows for compatibility, but never return their
		// phone-bearing IDs to the login view or the account catalog.
		phoneDigits := digits(s.Phone)
		if !isPhone(phoneDigits) {
			return "", errors.New("autoclaw_profile_invalid")
		}
		opaqueID := vault.OpaqueID("autoclaw", phoneDigits)
		if opaqueID == "" {
			return "", errors.New("autoclaw_vault_unavailable")
		}
		legacyID := "autoclaw-" + phoneDigits
		var revision uint64
		id := opaqueID
		rows, err := vault.List()
		if err != nil {
			return "", err
		}
		for _, a := range rows {
			if a.ID != opaqueID && a.ID != legacyID {
				continue
			}
			if a.Provider != "autoclaw" {
				return "", errors.New("account_provider_conflict")
			}
			id, revision = a.ID, a.Revision
			break
		}
		if _, err := vault.Put(accountvault.Account{ID: id, Provider: "autoclaw", Enabled: true}, profile, revision); err != nil {
			return "", err
		}
		return opaqueID, nil
	}
}

// newDeviceID returns a fresh per-flow device id in the released gateway's
// "acg-"+uuid shape.
func newDeviceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "acg-fallback-device"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("acg-%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func isPhone(v string) bool {
	if len(v) != 11 {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return v[0] == '1'
}

// isInviteCode accepts the referral-code shape the service uses: letters and
// digits only, bounded length. Blank is handled by the caller as "use default".
func isInviteCode(v string) bool {
	if len(v) < 4 || len(v) > 32 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func isCode(v string) bool {
	if len(v) < 4 || len(v) > 8 {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isDeviceID(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func maskPhone(v string) string {
	if len(v) != 11 {
		return v
	}
	return v[:3] + "****" + v[7:]
}

func digits(v string) string {
	var b strings.Builder
	for _, r := range v {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ExportProfile renders one stored account in the standalone reference
// gateway's account format. The result holds live credentials: callers must
// write it with restrictive permissions and never log it.
func ExportProfile(vault *accountvault.Vault, id string) ([]byte, error) {
	if vault == nil {
		return nil, errors.New("autoclaw_vault_unavailable")
	}
	_, plain, err := vault.Get(id)
	if err != nil {
		return nil, err
	}
	defer wipe(plain)
	var s stored
	if json.Unmarshal(plain, &s) != nil {
		return nil, errors.New("autoclaw_profile_invalid")
	}
	if s.Phone == "" {
		return nil, errors.New("autoclaw_profile_incomplete")
	}
	// The reference format is camelCase and carries a JWT-derived uid.
	return json.Marshal(map[string]any{
		"phone":        s.Phone,
		"uid":          jwtClaim(s.AccessToken, "uid"),
		"accessToken":  s.AccessToken,
		"refreshToken": s.RefreshToken,
		"deviceId":     s.DeviceID,
		"expiresAt":    s.ExpiresAt,
	})
}

// ExportPhoneDigits returns the digits of the phone number, for filename use.
func ExportPhoneDigits(profile []byte) string {
	var v struct {
		Phone string `json:"phone"`
	}
	if json.Unmarshal(profile, &v) != nil {
		return "unknown"
	}
	return digits(v.Phone)
}

// jwtClaim reads one string claim from an unsigned JWT payload. A token that is
// not a JWT, or lacks the claim, yields an empty string.
func jwtClaim(token, claim string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	if value, ok := claims[claim].(string); ok {
		return value
	}
	return ""
}
