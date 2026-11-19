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
)

// LoginDriver starts and completes a phone + SMS login for AutoClaw accounts.
type LoginDriver struct {
	// Save persists the verified account into the provider vault.
	Save func(ctx context.Context, profile []byte) (accountID string, err error)
	// HTTP is optional; tests inject a transport.
	HTTP *http.Client
}

type loginState struct {
	Phone    string `json:"phone"`
	DeviceID string `json:"device_id"`
	// Stage tracks how far this flow has gone so a lost response before the code
	// was sent is never mistaken for one that was.
	Stage string `json:"stage"`
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
	st := loginState{DeviceID: loginDeviceID, Stage: "awaiting_phone"}
	secret, _ := json.Marshal(st)
	return authflow.Result{
		Status: "waiting_phone", InputMode: "phone", Secret: secret,
		Notice: "请输入 AutoClaw 账号绑定的手机号（11 位）。",
	}, nil
}

// sendCodeFlow performs the SMS send after the phone has been validated.
func (d *LoginDriver) sendCodeFlow(ctx context.Context, phone, device string) (authflow.Result, error) {
	st := loginState{Phone: phone, DeviceID: device, Stage: "code_requested"}
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
		Notice: "验证码已发送到 " + maskPhone(phone) + "，请输入收到的 6 位验证码。",
	}, nil
}

func (d *LoginDriver) Step(ctx context.Context, f authflow.Flow, action string, input authflow.Input) (authflow.Result, error) {
	var st loginState
	if json.Unmarshal(f.Secret, &st) != nil {
		return authflow.Result{}, authflow.ErrInvalid
	}
	if st.DeviceID == "" {
		st.DeviceID = loginDeviceID
	}
	switch action {
	case "send_code", "phone", "submit_phone":
		phone := strings.TrimSpace(input.Phone)
		if !isPhone(phone) {
			return authflow.Result{Status: "waiting_phone", InputMode: "phone",
				Notice: "手机号格式不正确，请输入 11 位大陆手机号。"}, nil
		}
		return d.sendCodeFlow(ctx, phone, st.DeviceID)
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
		if d.Save == nil {
			return authflow.Result{}, errors.New("autoclaw_login_storage_unavailable")
		}
		id, err := d.Save(ctx, profile)
		if err != nil {
			return authflow.Result{}, err
		}
		return authflow.Result{Status: "verified", AccountID: id,
			Notice: "登录成功，账号已保存到 AutoClaw 账号池。"}, nil
	default:
		return authflow.Result{}, authflow.ErrInvalid
	}
}

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
		id := "autoclaw-" + digits(s.Phone)
		// Reuse the existing row so re-login updates instead of duplicating.
		var revision uint64
		rows, err := vault.List()
		if err != nil {
			return "", err
		}
		for _, a := range rows {
			if a.ID == id {
				if a.Provider != "autoclaw" {
					return "", errors.New("account_provider_conflict")
				}
				revision = a.Revision
			}
		}
		if _, err := vault.Put(accountvault.Account{ID: id, Provider: "autoclaw", Enabled: true}, profile, revision); err != nil {
			return "", err
		}
		return id, nil
	}
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
		return "***"
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
