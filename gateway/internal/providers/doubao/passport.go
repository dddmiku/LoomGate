package doubao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/authflow"
)

// Passport uses parameters obtained from the official SDK context. It never
// invents device IDs or substitutes a paid API. Secrets live only in encrypted flows.
type Passport struct {
	Bootstrap func(context.Context) (StoredProfile, error)
	// Prepare applies the current official SDK context/signatures per request.
	// A captured signature is not a substitute. No signer means no network login.
	Prepare   func(context.Context, *http.Request) error
	Save      func(context.Context, string, StoredProfile) error
	Transport http.RoundTripper
}
type passportState struct {
	Profile StoredProfile  `json:"profile"`
	Cookies []*http.Cookie `json:"cookies,omitempty"`
	QRToken string         `json:"qr_token,omitempty"`
	Phone   string         `json:"phone,omitempty"`
	UserID  string         `json:"user_id,omitempty"`
}

func (passportState) String() string { return "passportState[redacted]" }
func (p *Passport) Methods() []authflow.Method {
	ready := p.Bootstrap != nil && p.Prepare != nil
	return []authflow.Method{{ID: "sms", Name: "手机号登录", Available: ready, Reason: passportSetupReason(ready)}, {ID: "qr", Name: "扫码登录", Available: ready, Reason: passportSetupReason(ready)}}
}
func passportSetupReason(ready bool) string {
	if ready {
		return ""
	}
	return "official_sdk_context_and_signer_required"
}

var passportPhone = regexp.MustCompile(`^\+?[0-9]{1,4} [0-9]{5,15}$`)
var passportCode = regexp.MustCompile(`^[0-9]{4,8}$`)

// PassportMix matches official module 491291 encrypt/encryptParams for numeric
// login fields. It is transport obfuscation, not secure storage encryption.
func PassportMix(value string) string {
	var out strings.Builder
	for _, b := range []byte(value) {
		out.WriteString(strconv.FormatUint(uint64(b^5), 16))
	}
	return out.String()
}
func passportSecret(st passportState) json.RawMessage { b, _ := json.Marshal(st); return b }
func (p *Passport) Start(ctx context.Context, r authflow.Request) (authflow.Result, error) {
	if p.Bootstrap == nil || p.Prepare == nil {
		return authflow.Result{Status: "manual_action", ErrorCode: "official_sdk_context_and_signer_required", Notice: "官方登录需要完整 SDK 上下文与实时签名，当前连接尚未就绪。请先在官方客户端登录。"}, nil
	}
	profile, err := p.Bootstrap(ctx)
	if err != nil {
		return authflow.Result{Status: "manual_action", ErrorCode: "official_sdk_context_unavailable"}, nil
	}
	if profile.Query.Get("aid") == "" || profile.Query.Get("device_platform") == "" {
		return authflow.Result{Status: "manual_action", ErrorCode: "official_sdk_context_invalid"}, nil
	}
	st := passportState{Profile: profile}
	st.Profile.Headers = canonicalPassportHeaders(profile.Headers)
	if st.Profile.Headers == nil {
		st.Profile.Headers = http.Header{}
	}
	// Account login is always a fresh authentication context, never a logout or
	// session swap on an existing signed-in account.
	st.Profile.Headers.Del("Cookie")
	st.Profile.Headers.Del("Authorization")
	st.Profile.Query = cloneValues(profile.Query)
	st.Profile.Query.Del("a_bogus")
	st.Profile.Query.Del("msToken")
	if r.Method == "sms" {
		return authflow.Result{Status: "waiting_phone", Secret: passportSecret(st), ActionURL: "https://www.doubao.com/chat/", Notice: "手机号和验证码仅提交到此受保护的管理表单，不写入日志。也可先在官方客户端完成登录。"}, nil
	}
	if r.Method != "qr" {
		return authflow.Result{}, authflow.ErrInvalid
	}
	data, err := p.request(ctx, &st, http.MethodGet, "/passport/web/get_qrcode/", url.Values{"next": {"https://www.doubao.com/chat/"}})
	if err != nil {
		return authflow.Result{}, err
	}
	if result, blocked := passportRejected(data, st); blocked {
		return result, nil
	}
	st.QRToken = passportString(data, "token")
	qr := passportString(data, "qrcode_index_url")
	if shorter := passportString(data, "qrcode_short_url"); shorter != "" {
		qr = shorter
	}
	if st.QRToken == "" || !safePassportURL(qr) {
		return authflow.Result{Status: "manual_action", Secret: passportSecret(st), ErrorCode: "qr_response_unrecognized", Notice: "官方二维码响应尚未识别，请在官方客户端登录。"}, nil
	}
	return authflow.Result{Status: "waiting_scan", QRURL: qr, Secret: passportSecret(st), Notice: "使用豆包官方客户端扫描二维码，并在手机上确认。"}, nil
}
func (p *Passport) Step(ctx context.Context, f authflow.Flow, action string, in authflow.Input) (authflow.Result, error) {
	var st passportState
	if json.Unmarshal(f.Secret, &st) != nil {
		return authflow.Result{}, authflow.ErrInvalid
	}
	var path, method string
	values := url.Values{}
	switch action {
	case "send_code":
		if !passportPhone.MatchString(in.Phone) {
			return authflow.Result{Status: "waiting_phone", Secret: passportSecret(st), ErrorCode: "phone_format_invalid", Notice: "请输入国家区号、空格及手机号。"}, nil
		}
		st.Phone = in.Phone
		path = "/passport/web/send_code/"
		method = http.MethodPost
		values = url.Values{"mobile": {PassportMix(in.Phone)}, "type": {PassportMix("24")}, "mix_mode": {"1"}, "fixed_mix_mode": {"1"}, "is6Digits": {"1"}}
	case "submit_code":
		if !passportCode.MatchString(in.Code) || st.Phone == "" {
			return authflow.Result{Status: "waiting_code", ErrorCode: "verification_code_invalid"}, nil
		}
		path = "/passport/web/sms_login/"
		method = http.MethodPost
		values = url.Values{"mobile": {PassportMix(st.Phone)}, "code": {PassportMix(in.Code)}, "login_only": {"false"}, "mix_mode": {"1"}, "fixed_mix_mode": {"1"}}
	case "poll":
		if f.Method == "qr" && st.UserID == "" {
			path = "/passport/web/check_qrconnect/"
			method = http.MethodGet
			values = url.Values{"next": {"https://www.doubao.com/chat/"}, "token": {st.QRToken}}
		} else {
			return p.verify(ctx, &st)
		}
	case "commit":
		if st.UserID == "" || p.Save == nil {
			return authflow.Result{}, authflow.ErrInvalid
		}
		digest := sha256.Sum256([]byte(st.UserID))
		id := "doubao-" + hex.EncodeToString(digest[:10])
		if f.AccountID != "" && f.AccountID != id {
			return authflow.Result{Status: "manual_action", ErrorCode: "account_identity_mismatch", Notice: "官方登录账号与要续期的账号不同，未覆盖原账号。"}, nil
		}
		jar, _ := cookiejar.New(nil)
		origin, _ := url.Parse("https://www.doubao.com/")
		authOrigin, _ := url.Parse("https://accounts.doubao.com/")
		jar.SetCookies(authOrigin, st.Cookies)
		request, _ := http.NewRequest(http.MethodGet, origin.String(), nil)
		for _, cookie := range jar.Cookies(origin) {
			request.AddCookie(cookie)
		}
		st.Profile.Headers.Set("Cookie", request.Header.Get("Cookie"))
		if request.Header.Get("Cookie") == "" {
			return authflow.Result{Status: "manual_action", ErrorCode: "work_session_not_verified", Notice: "官方登录已核验，但尚无适用于工作服务的会话，未导入账号。"}, nil
		}
		if err := p.Save(ctx, id, st.Profile); err != nil {
			return authflow.Result{}, err
		}
		return authflow.Result{Status: "succeeded", AccountID: id, Notice: "官方会话已核验并保存。"}, nil
	default:
		return authflow.Result{}, authflow.ErrInvalid
	}
	data, err := p.request(ctx, &st, method, path, values)
	if err != nil {
		return authflow.Result{}, err
	}
	if result, blocked := passportRejected(data, st); blocked {
		return result, nil
	}
	if action == "send_code" {
		retry := 60 * time.Second
		if seconds, e := strconv.Atoi(passportString(data, "retry_time")); e == nil && seconds > 0 && seconds < 3600 {
			retry = time.Duration(seconds) * time.Second
		}
		return authflow.Result{Status: "waiting_code", RetryAfter: retry, Secret: passportSecret(st), Notice: "短信发送已被官方接受，请输入收到的验证码。"}, nil
	}
	if action == "submit_code" {
		return p.verify(ctx, &st)
	}
	switch passportString(data, "status") {
	case "new":
		return authflow.Result{Status: "waiting_scan", QRURL: f.QRURL, Secret: passportSecret(st)}, nil
	case "scanned":
		return authflow.Result{Status: "scanned", QRURL: f.QRURL, Secret: passportSecret(st), Notice: "已扫码，请在手机上确认。"}, nil
	case "confirmed", "login":
		return p.verify(ctx, &st)
	case "expired":
		return authflow.Result{Status: "expired", Notice: "官方二维码已过期，请重新开始。"}, nil
	case "refused":
		return authflow.Result{Status: "failed", ErrorCode: "login_refused", Notice: "已在官方客户端拒绝登录。"}, nil
	default:
		return authflow.Result{Status: "manual_action", Secret: passportSecret(st), ErrorCode: "qr_state_unrecognized"}, nil
	}
}
func (p *Passport) verify(ctx context.Context, st *passportState) (authflow.Result, error) {
	data, err := p.request(ctx, st, http.MethodGet, "/passport/account/info/v2/", url.Values{})
	if err != nil {
		return authflow.Result{}, err
	}
	if result, blocked := passportRejected(data, *st); blocked {
		return result, nil
	}
	id := passportString(data, "user_id_str")
	if id == "" {
		id = passportString(data, "user_id")
	}
	if id == "" {
		return authflow.Result{Status: "manual_action", Secret: passportSecret(*st), ErrorCode: "session_not_verified", Notice: "授权尚未通过账号信息复核，请在官方界面确认。"}, nil
	}
	st.UserID = id
	st.Phone = ""
	return authflow.Result{Status: "verified", Secret: passportSecret(*st), Notice: "官方账号信息已复核，等待保存会话。"}, nil
}
func passportRejected(data map[string]any, st passportState) (authflow.Result, bool) {
	code := passportString(data, "error_code")
	if code == "" || code == "0" {
		return authflow.Result{}, false
	}
	var number *int64
	if n, err := strconv.ParseInt(code, 10, 64); err == nil && n >= 0 && n <= 999999999 {
		number = &n
	}
	// A nonzero business code is not proof of a captcha or risk-control failure.
	// Preserve only the bounded numeric code; descriptions/tickets stay private.
	return authflow.Result{Status: "manual_action", Secret: passportSecret(st), ErrorCode: "official_request_rejected", UpstreamCode: number, ActionURL: "https://www.doubao.com/chat/", Notice: "官方未接受本次登录请求。具体原因尚未核实，请在官方界面检查；不会自动重发短信或验证码。"}, true
}
func passportString(data map[string]any, key string) string {
	switch value := data[key].(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return ""
}
func safePassportURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && (u.Hostname() == "www.doubao.com" || u.Hostname() == "www.douyin.com" || u.Hostname() == "lf.douyin.com")
}
func cloneValues(values url.Values) url.Values {
	out := url.Values{}
	for k, v := range values {
		out[k] = append([]string(nil), v...)
	}
	return out
}
func (p *Passport) request(ctx context.Context, st *passportState, method, path string, values url.Values) (map[string]any, error) {
	if p.Prepare == nil {
		return nil, errors.New("official_sdk_signer_unavailable")
	}
	u := url.URL{Scheme: "https", Host: "accounts.doubao.com", Path: path}
	q := cloneValues(st.Profile.Query)
	for k, v := range values {
		if method == http.MethodGet {
			q[k] = v
		}
	}
	u.RawQuery = q.Encode()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errors.New("passport_request_invalid")
	}
	req.Header = canonicalPassportHeaders(st.Profile.Headers)
	for _, key := range []string{"Cookie", "Authorization", "Host", "Content-Length", "Accept-Encoding", "Connection"} {
		req.Header.Del(key)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://www.doubao.com")
	req.Header.Set("Referer", "https://www.doubao.com/chat/")
	if err := p.Prepare(ctx, req); err != nil {
		return nil, errors.New("official_sdk_prepare_failed")
	}
	if req.URL.Scheme != "https" || req.URL.Host != "accounts.doubao.com" || req.URL.Path != path || req.URL.User != nil || req.Method != method {
		return nil, errors.New("passport_origin_rejected")
	}
	jar, _ := cookiejar.New(nil)
	origin, _ := url.Parse("https://accounts.doubao.com/")
	jar.SetCookies(origin, st.Cookies)
	client := http.Client{Timeout: 20 * time.Second, Jar: jar, Transport: p.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("passport_transport_failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("passport_http_%d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("passport_response_invalid")
	}
	var envelope struct {
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&envelope) != nil || envelope.Data == nil {
		return nil, errors.New("passport_response_invalid")
	}
	if envelope.Message != "success" && passportString(envelope.Data, "error_code") == "" {
		return nil, errors.New("passport_response_unconfirmed")
	}
	// Preserve server cookie attributes across requests and encrypted restarts.
	for _, fresh := range response.Cookies() {
		if fresh.Domain != "" && fresh.Domain != ".doubao.com" && fresh.Domain != "doubao.com" && fresh.Domain != "accounts.doubao.com" {
			continue
		}
		replaced := false
		for i, old := range st.Cookies {
			if old.Name == fresh.Name && old.Path == fresh.Path {
				st.Cookies[i] = fresh
				replaced = true
				break
			}
		}
		if !replaced {
			st.Cookies = append(st.Cookies, fresh)
		}
	}
	return envelope.Data, nil
}

func canonicalPassportHeaders(headers http.Header) http.Header {
	out := http.Header{}
	for key, values := range headers {
		for _, value := range values {
			out.Add(key, value)
		}
	}
	return out
}
