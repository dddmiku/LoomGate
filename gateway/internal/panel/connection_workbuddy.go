package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"workbuddy2api/internal/authflow"
)

// The same official device authorization protocol as cmd/login, with its state
// held in the encrypted workflow instead of temporary plaintext helper output.
func (d *legacyConnection) workbuddyCall(ctx context.Context, realm, path string, body any, bearer string) (map[string]any, bool, error) {
	base, origin := "https://copilot.tencent.com", "https://www.codebuddy.cn"
	if realm == "global" {
		base = "https://www.workbuddy.ai"
		origin = base
	}
	method := http.MethodGet
	var reader io.Reader
	if body != nil {
		method = http.MethodPost
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, false, errors.New("login_request_failed")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	r.Header.Set("Referer", origin+"/")
	r.Header.Set("X-Requested-With", "XMLHttpRequest")
	r.Header.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := d.client
	if client == nil {
		client = &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	response, err := client.Do(r)
	if err != nil {
		return nil, false, errors.New("official_authorization_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, false, errors.New("official_authorization_rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, false, errors.New("official_authorization_invalid")
	}
	var envelope struct {
		Code *int           `json:"code"`
		Msg  string         `json:"msg"`
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil {
		return nil, false, errors.New("official_authorization_invalid")
	}
	if *envelope.Code != 0 {
		if path != "" && strings.HasPrefix(path, "/v2/plugin/auth/token?") && strings.EqualFold(strings.TrimSpace(envelope.Msg), "login ing") {
			return nil, true, nil
		}
		return nil, false, errors.New("official_authorization_rejected")
	}
	if envelope.Data == nil {
		return nil, false, errors.New("official_authorization_invalid")
	}
	return envelope.Data, false, nil
}
func (d *legacyConnection) startWorkbuddy(ctx context.Context, r authflow.Request) (authflow.Result, error) {
	data, _, err := d.workbuddyCall(ctx, r.Realm, "/v2/plugin/auth/state?platform=CLI", map[string]any{}, "")
	if err != nil {
		return authflow.Result{}, err
	}
	st := legacyState{LoginID: jsonString(data["state"]), URL: jsonString(data["authUrl"])}
	if st.LoginID == "" || len(st.LoginID) > 4096 || !safeLoginURL(st.URL) {
		return authflow.Result{}, errors.New("official_authorization_invalid")
	}
	b, _ := json.Marshal(st)
	return authflow.Result{Status: "waiting_scan", ActionURL: st.URL, Secret: b, Notice: "请在官方授权页面登录或扫码确认。"}, nil
}
func (d *legacyConnection) pollWorkbuddy(ctx context.Context, f authflow.Flow, st legacyState) (authflow.Result, error) {
	path := "?" + url.Values{"state": {st.LoginID}}.Encode()
	tokens, pending, err := d.workbuddyCall(ctx, f.Realm, "/v2/plugin/auth/token"+path, nil, "")
	if err != nil {
		return authflow.Result{}, err
	}
	if pending {
		return authflow.Result{Status: "waiting_scan", ActionURL: st.URL, Secret: f.Secret, Notice: "等待官方授权确认。"}, nil
	}
	access := jsonString(tokens["accessToken"])
	if access == "" {
		return authflow.Result{}, errors.New("official_authorization_invalid")
	}
	account, _, err := d.workbuddyCall(ctx, f.Realm, "/v2/plugin/login/account"+path, nil, access)
	if err != nil || !uidRe.MatchString(jsonString(account["uid"])) {
		return authflow.Result{Status: "manual_action", ErrorCode: "account_identity_not_verified", Notice: "授权令牌尚未通过官方账号信息复核，未保存账号。"}, nil
	}
	st.Credentials = map[string]any{"uid": account["uid"], "nickname": account["nickname"], "enterprise_id": account["enterpriseId"], "access_token": access, "refresh_token": tokens["refreshToken"], "expires_in": tokens["expiresIn"], "domain": tokens["domain"], "realm": f.Realm}
	expires := jsonInt64(tokens["expiresIn"])
	if expires <= 0 {
		return authflow.Result{Status: "manual_action", ErrorCode: "session_expiry_missing", Notice: "官方授权结果缺少有效期，未保存账号。"}, nil
	}
	st.Credentials["verified_expires_at"] = time.Now().Add(time.Duration(expires) * time.Second).Unix()
	b, _ := json.Marshal(st)
	return authflow.Result{Status: "verified", Secret: b, Notice: "官方账号身份已核验，等待保存。"}, nil
}
