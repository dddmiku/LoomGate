package doubao

import (
	"context"
	"crypto/rand"
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

// Passport logs into Doubao directly from the gateway, with no packet capture
// and no running desktop client.
//
// Verified against the live service (see D:/LoomGate-work capture probes):
//   - /passport/web/get_qrcode/ works anonymously: error_code 0, a token and a
//     scannable QR URL, with no a_bogus/msToken. Signing is not enforced.
//   - /passport/web/send_code/ accepts an anonymous request and answers with a
//     structured error_code (1003 for an invalid number), so SMS is reachable
//     without the official signer.
//   - `real_aid` is the ONE device parameter that makes the passport host return
//     error 22. It is needed by the chat host, so it is stripped only here.
//
// The login still runs against the real service: QR scanning and any SMS code
// are performed by the user. Nothing here fabricates a credential.
type Passport struct {
	// Save persists a verified session into the encrypted provider vault.
	Save func(context.Context, string, StoredProfile) error
	// Transport is injectable for tests; production uses the default.
	Transport http.RoundTripper
	// Host overrides the passport origin in tests only.
	Host string
	// Trace, when set, receives read-only startup responses during bot-id
	// discovery so a local diagnostic can inspect their structure. It must not
	// be wired in production; it never receives a response from a write.
	Trace func(endpoint string, payload any)
}

type passportState struct {
	Profile StoredProfile  `json:"profile"`
	Cookies []*http.Cookie `json:"cookies,omitempty"`
	QRToken string         `json:"qr_token,omitempty"`
	Phone   string         `json:"phone,omitempty"`
	UserID  string         `json:"user_id,omitempty"`
	// MobileTicket is a bounded opaque handle the service returns with some
	// send-code errors. It is kept only inside the encrypted flow secret.
	MobileTicket string `json:"mobile_ticket,omitempty"`
	// Mobile and DisplayName are non-secret display facts read from account-info;
	// the service masks the phone number itself.
	Mobile      string `json:"mobile,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	// CaptchaVerdict marks that the service demanded human verification, so the
	// flow can say so instead of silently appearing to have sent an SMS.
	CaptchaVerdict bool `json:"captcha_verdict,omitempty"`
}

func (passportState) String() string   { return "passportState[redacted]" }
func (passportState) GoString() string { return "passportState[redacted]" }

// Methods reports the login methods this build can actually perform. Both need
// only the network, so both are always available.
func (p *Passport) Methods() []authflow.Method {
	return []authflow.Method{
		{ID: "sms", Name: "手机号登录", Available: true},
		{ID: "qr", Name: "扫码登录", Available: true},
	}
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

func (p *Passport) host() string {
	if p.Host != "" {
		return p.Host
	}
	return "accounts.doubao.com"
}

// deviceQuery is the anonymous device context sent with every passport and chat
// request. These values are NOT arbitrary: the QR state probe answers error 22
// with a captcha demand when the context is incomplete, and several chat
// endpoints reject unknown device ids. The set and shapes mirror what the
// official desktop client sends; only the per-install identifiers are generated
// fresh for each login so two gateway logins never share a device fingerprint.
func deviceQuery() url.Values {
	q := url.Values{}
	q.Set("aid", "582478")
	q.Set("real_aid", "582478")
	q.Set("channel", "win")
	q.Set("pkg_type", "release_version")
	q.Set("device_platform", "web")
	q.Set("doubao_device_platform", "desktop")
	q.Set("web_platform", "desktop")
	q.Set("client_platform", "pc_client")
	q.Set("runtime", "web")
	q.Set("runtime_version", "3.46.3")
	q.Set("version_code", "20800")
	q.Set("pc_version", "2.36.10")
	q.Set("doubao_pc_version", "2.36.10")
	q.Set("chromium_version", "147.0.7727.149")
	q.Set("samantha_web", "1")
	q.Set("use-olympus-account", "1")
	q.Set("language", "zh")
	q.Set("region", "CN")
	q.Set("sys_region", "CN")
	q.Set("tz_name", "Asia/Shanghai")

	// Per-install identifiers.
	deviceID := randomDigits(16)
	q.Set("device_id", deviceID)
	q.Set("fp", "verify_"+deviceID)
	q.Set("tea_uuid", deviceID)
	q.Set("web_id", randomDigits(19))
	q.Set("web_tab_id", randomUUID())
	return q
}

func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

func (p *Passport) Start(ctx context.Context, r authflow.Request) (authflow.Result, error) {
	st := passportState{Profile: StoredProfile{Query: deviceQuery(), Headers: http.Header{}}}
	if r.Method == "sms" {
		return authflow.Result{
			Status:    "waiting_phone",
			Secret:    passportSecret(st),
			ActionURL: "https://www.doubao.com/chat/",
			Notice:    "手机号与验证码仅提交到本受保护表单，不写入日志。",
		}, nil
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
		return authflow.Result{Status: "manual_action", Secret: passportSecret(st), ErrorCode: "qr_response_unrecognized", Notice: "官方二维码响应尚未识别。"}, nil
	}
	return authflow.Result{Status: "waiting_scan", QRURL: qr, Secret: passportSecret(st), Notice: "使用豆包 App 扫描二维码，并在手机上确认。"}, nil
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
		values = url.Values{
			"mobile":         {PassportMix(in.Phone)},
			"type":           {PassportMix("24")},
			"mix_mode":       {"1"},
			"fixed_mix_mode": {"1"},
			"is6Digits":      {"1"},
		}
	case "submit_code":
		if !passportCode.MatchString(in.Code) || st.Phone == "" {
			return authflow.Result{Status: "waiting_code", Secret: passportSecret(st), ErrorCode: "verification_code_invalid"}, nil
		}
		path = "/passport/web/sms_login/"
		method = http.MethodPost
		values = url.Values{
			"mobile":         {PassportMix(st.Phone)},
			"code":           {PassportMix(in.Code)},
			"login_only":     {"false"},
			"mix_mode":       {"1"},
			"fixed_mix_mode": {"1"},
		}
	case "poll":
		if f.Method == "qr" && st.UserID == "" {
			path = "/passport/web/check_qrconnect/"
			method = http.MethodGet
			// The QR state probe requires the full device parameter set. With only
			// next+token the service answers error 22 with a captcha demand, which
			// looks like a risk-control block but is really a missing-context
			// rejection (verified against the live service).
			values = url.Values{"next": {"https://www.doubao.com/chat/"}, "token": {st.QRToken}}
		} else {
			return p.verify(ctx, &st)
		}
	case "resume":
		// A flow that already authenticated but failed a later commit step (for
		// example an interrupted bot-id lookup) keeps its cookies in the encrypted
		// secret. Re-read the account identity instead of asking the user to scan
		// again; the caller then issues an explicit commit.
		if st.UserID == "" {
			return authflow.Result{Status: "manual_action", Secret: passportSecret(st), ErrorCode: "resume_requires_login", Notice: "上次登录尚未完成，请重新扫码或获取验证码。"}, nil
		}
		return p.verify(ctx, &st)
	case "commit":
		return p.commit(ctx, f, &st)
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
		return authflow.Result{Status: "waiting_code", RetryAfter: retry, Secret: passportSecret(st), Notice: "验证码已发送，请输入收到的验证码。"}, nil
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
		return authflow.Result{Status: "expired", Notice: "二维码已过期，请重新开始。"}, nil
	case "refused":
		return authflow.Result{Status: "failed", ErrorCode: "login_refused", Notice: "已在手机上拒绝登录。"}, nil
	default:
		return authflow.Result{Status: "manual_action", Secret: passportSecret(st), ErrorCode: "qr_state_unrecognized"}, nil
	}
}

// verify reads the account identity. It is a GET with no side effect, so a
// timeout may safely be retried by the caller.
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
		return authflow.Result{Status: "manual_action", Secret: passportSecret(*st), ErrorCode: "session_not_verified", Notice: "授权尚未通过账号信息复核。"}, nil
	}
	st.UserID = id
	// The service already masks the phone number it returns, so this is a display
	// fact, not a credential: it lets the account list identify the login instead
	// of showing only an internal hash. name/screen_name are the account's own
	// display name and are equally non-secret.
	st.Mobile = passportString(data, "mobile")
	st.DisplayName = passportString(data, "name")
	if st.DisplayName == "" {
		st.DisplayName = passportString(data, "screen_name")
	}
	st.Phone = ""
	return authflow.Result{Status: "verified", Secret: passportSecret(*st), Notice: "账号信息已复核，等待保存会话。"}, nil
}

// commit seals the verified cookies into an encrypted account profile. It stores
// the cookies and device context only; it never injects a captured request body.
func (p *Passport) commit(ctx context.Context, f authflow.Flow, st *passportState) (authflow.Result, error) {
	if st.UserID == "" || p.Save == nil {
		return authflow.Result{}, authflow.ErrInvalid
	}
	digest := sha256.Sum256([]byte(st.UserID))
	id := "doubao-" + hex.EncodeToString(digest[:10])
	if f.AccountID != "" && f.AccountID != id {
		return authflow.Result{Status: "manual_action", ErrorCode: "account_identity_mismatch", Notice: "登录账号与要续期的账号不同，未覆盖原账号。"}, nil
	}

	// Cookiejar picks the cookies that actually apply to the chat origin. A login
	// cookie scoped to accounts.doubao.com is not a chat session, so requiring a
	// non-empty chat cookie prevents importing a login that cannot serve models.
	jar, _ := cookiejar.New(nil)
	authOrigin, _ := url.Parse("https://accounts.doubao.com/")
	jar.SetCookies(authOrigin, st.Cookies)
	chatOrigin, _ := url.Parse("https://www.doubao.com/")
	chatCookies := jar.Cookies(chatOrigin)
	if len(chatCookies) == 0 {
		return authflow.Result{Status: "manual_action", ErrorCode: "work_session_not_verified", Notice: "登录已核验，但尚未取得可用于模型服务的会话，未导入账号。"}, nil
	}

	headers := http.Header{}
	var parts []string
	for _, cookie := range chatCookies {
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	headers.Set("Cookie", strings.Join(parts, "; "))
	headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
	headers.Set("Referer", "https://www.doubao.com/chat/")
	headers.Set("agw-js-conv", "str")

	// The completion and catalog requests both require the account's own default
	// bot id. It is not returned by any passport endpoint and cannot be derived
	// from the account, so it must be discovered from the logged-in session.
	botID, botErr := p.discoverBotID(ctx, st, headers)
	if botErr != nil || botID == "" {
		return authflow.Result{
			Status:    "manual_action",
			ErrorCode: "bot_identity_unavailable",
			Notice:    "登录成功，但未能确认该账号的默认模型入口，未导入账号。请稍后重试，或在官方客户端打开一次对话后重试。",
		}, nil
	}

	profile := StoredProfile{
		Query:       cloneValues(st.Profile.Query),
		Headers:     headers,
		BotID:       botID,
		BindingFlow: "gateway-passport",
		VerifiedAt:  time.Now().UTC(),
		Mobile:      st.Mobile,
		DisplayName: st.DisplayName,
	}
	if err := p.Save(ctx, id, profile); err != nil {
		return authflow.Result{}, err
	}
	return authflow.Result{Status: "succeeded", AccountID: id, Notice: "豆包账号已登录并保存。"}, nil
}

// defaultAssistantBotID is the Doubao main-assistant bot id that the official
// desktop client hardcodes in its own bundle (it appears as the default entry
// for bot_list_profile in several webpack chunks). It is NOT per-account: the
// account-specific `samantha_bot` in the conversation feed is a customization,
// while this id is the model entry every account can talk to. Discovery still
// verifies it against the live service before it is stored.
const defaultAssistantBotID = "7338286299411103781"

// discoverBotID resolves the account's model entry bot id. It is read-only.
//
// Primary source: the client's hardcoded default assistant id, confirmed with
// one brief_list call (that endpoint rejects an unknown id with 710010202, so a
// code-0 answer proves the id is valid for this account).
// Secondary source: extra.coco_bot_id from the recent-conversation chain.
func (p *Passport) discoverBotID(ctx context.Context, st *passportState, headers http.Header) (string, error) {
	verified, verifyErr := p.verifyBotID(ctx, st, headers, defaultAssistantBotID)
	if p.Trace != nil {
		p.Trace("/alice/slot/action_bar_v3/brief_list", map[string]any{"code": verified, "error": errText(verifyErr)})
	}
	if verifyErr == nil {
		return defaultAssistantBotID, nil
	}
	if data, err := p.imRecentConv(ctx, st, headers); err == nil {
		if p.Trace != nil {
			p.Trace("/im/chain/recent_conv", data)
		}
		if id := findBotIDField(data, "coco_bot_id"); id != "" {
			return id, nil
		}
		if id := findBotIDField(data, "bot_id"); id != "" {
			return id, nil
		}
	} else if p.Trace != nil {
		p.Trace("/im/chain/recent_conv:error", map[string]any{"error": err.Error()})
	}
	return "", errors.New("bot_identity_unavailable")
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// verifyBotID issues the read-only catalog call with a candidate bot id. A
// business error means the candidate is not usable for this account.
func (p *Passport) verifyBotID(ctx context.Context, st *passportState, headers http.Header, candidate string) (int64, error) {
	payload, err := p.chatPost(ctx, st, "/alice/slot/action_bar_v3/brief_list", map[string]any{
		"bot_id":          candidate,
		"conversation_id": "",
		"language_code":   "zh",
	}, headers)
	if err != nil {
		return -1, err
	}
	if node, ok := payload.(map[string]any); ok {
		if code := passportInt(node["code"]); code != 0 {
			return code, errors.New("passport_request_rejected")
		}
	}
	return 0, nil
}

// imRecentConv calls the IM chain endpoint the client uses for its home feed.
// The response carries the account's Coco bot identity in extra.coco_bot_id.
func (p *Passport) imRecentConv(ctx context.Context, st *passportState, headers http.Header) (any, error) {
	// Direction 3 = FROM_LATEST (see the client's dj enum), conv_version 0 asks
	// for the newest page and makes the service include the Coco bot identity.
	uplink := map[string]any{
		"api_version":            1,
		"conv_version":           0,
		"direction":              3,
		"limit":                  20,
		"message_count_per_conv": 0,
		"ext":                    map[string]any{},
		"option": map[string]any{
			"not_need_message":           true,
			"need_complete_conversation": true,
			"need_coco_conversation":     true,
			"need_coco_bot":              true,
			"need_pc_pin_chain":          true,
			"pc_pin_query_type":          0,
		},
	}
	body := map[string]any{
		"cmd":         3200,
		"channel":     2,
		"version":     "1",
		"sequence_id": randomUUID(),
		"uplink_body": map[string]any{"pull_recent_conv_chain_uplink_body": uplink},
	}
	raw, err := p.chatPostRaw(ctx, st, "/im/chain/recent_conv", body, headers)
	if err != nil {
		return nil, err
	}
	if node, ok := raw.(map[string]any); ok {
		if code, exists := node["status_code"]; exists && passportInt(code) != 0 {
			return nil, errors.New("passport_request_rejected")
		}
		if down, exists := node["downlink_body"]; exists {
			if m, ok := down.(map[string]any); ok {
				if payload, ok := m["pull_recent_conv_chain_downlink_body"]; ok {
					return payload, nil
				}
			}
			return down, nil
		}
	}
	return raw, nil
}

// passportInt reads a numeric JSON value that may be a number or a string.
func passportInt(raw any) int64 {
	switch value := raw.(type) {
	case json.Number:
		n, _ := value.Int64()
		return n
	case float64:
		return int64(value)
	case string:
		n, _ := strconv.ParseInt(value, 10, 64)
		return n
	}
	return 0
}

// findBotIDField walks a JSON value for a specific field holding a bot id.
func findBotIDField(value any, field string) string {
	switch node := value.(type) {
	case map[string]any:
		if raw, ok := node[field]; ok {
			if text := botIDString(raw); text != "" {
				return text
			}
		}
		for _, child := range node {
			if found := findBotIDField(child, field); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range node {
			if found := findBotIDField(child, field); found != "" {
				return found
			}
		}
	}
	return ""
}

// firstBotID walks a JSON value for the first bot-id-shaped string field.
func firstBotID(value any) string {
	switch node := value.(type) {
	case map[string]any:
		for _, key := range []string{"bot_id", "botId", "default_bot_id"} {
			if raw, ok := node[key]; ok {
				if text := botIDString(raw); text != "" {
					return text
				}
			}
		}
		for _, key := range []string{"samantha_bot", "samanthaBotInfo", "bot", "default_bot", "entry_list", "data"} {
			if child, ok := node[key]; ok {
				if found := firstBotID(child); found != "" {
					return found
				}
			}
		}
		for _, child := range node {
			if found := firstBotID(child); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range node {
			if found := firstBotID(child); found != "" {
				return found
			}
		}
	}
	return ""
}

func botIDString(raw any) string {
	text, ok := raw.(string)
	if !ok || len(text) < 10 || len(text) > 25 {
		return ""
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return text
}

// chatPost performs an authenticated read-only POST against the chat host.
func (p *Passport) chatPost(ctx context.Context, st *passportState, path string, body map[string]any, headers http.Header) (any, error) {
	raw, err := p.chatPostRaw(ctx, st, path, body, headers)
	if err != nil {
		return nil, err
	}
	if envelope, ok := raw.(map[string]any); ok {
		if code := passportInt(envelope["code"]); code != 0 {
			return nil, errors.New("passport_request_rejected")
		}
		if data, exists := envelope["data"]; exists {
			return data, nil
		}
	}
	return raw, nil
}

// chatPostRaw returns the decoded JSON body without unwrapping `data`, for
// endpoints that carry their own status envelope.
func (p *Passport) chatPostRaw(ctx context.Context, st *passportState, path string, body map[string]any, headers http.Header) (any, error) {
	u := url.URL{Scheme: "https", Host: "www.doubao.com", Path: path, RawQuery: cloneValues(st.Profile.Query).Encode()}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("passport_request_invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(raw)))
	if err != nil {
		return nil, errors.New("passport_request_invalid")
	}
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", "application/json")
	jar, _ := cookiejar.New(nil)
	origin, _ := url.Parse("https://www.doubao.com/")
	jar.SetCookies(origin, st.Cookies)
	client := http.Client{Timeout: 20 * time.Second, Jar: jar, Transport: p.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("passport_transport_failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("passport_http_%d", response.StatusCode)
	}
	bounded, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(bounded) > 1<<20 {
		return nil, errors.New("passport_response_invalid")
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(string(bounded)))
	decoder.UseNumber()
	if decoder.Decode(&decoded) != nil {
		return nil, errors.New("passport_response_invalid")
	}
	return decoded, nil
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
	// Some rejections demand human verification. Say so explicitly instead of
	// letting the caller assume an SMS is on its way.
	notice := "官方未接受本次登录请求，请在官方界面检查；不会自动重发短信或验证码。"
	if _, ok := data["captcha"]; ok {
		notice = "官方要求人机验证，请在官方客户端完成登录后再重试。"
	}
	return authflow.Result{Status: "manual_action", Secret: passportSecret(st), ErrorCode: "official_request_rejected", UpstreamCode: number, ActionURL: "https://www.doubao.com/chat/", Notice: notice}, true
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
	u := url.URL{Scheme: "https", Host: p.host(), Path: path}
	q := cloneValues(st.Profile.Query)
	// real_aid is the only device parameter that makes the passport host answer
	// error 22; the chat host still needs it, so strip it here only.
	q.Del("real_aid")
	q.Del("a_bogus")
	q.Del("msToken")
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
	for key, list := range st.Profile.Headers {
		for _, value := range list {
			req.Header.Add(key, value)
		}
	}
	for _, key := range []string{"Cookie", "Authorization", "Host", "Content-Length", "Accept-Encoding", "Connection"} {
		req.Header.Del(key)
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Origin", "https://www.doubao.com")
	req.Header.Set("Referer", "https://www.doubao.com/")
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
	}

	jar, _ := cookiejar.New(nil)
	origin, _ := url.Parse("https://" + p.host() + "/")
	jar.SetCookies(origin, st.Cookies)
	client := http.Client{
		Timeout:   20 * time.Second,
		Jar:       jar,
		Transport: p.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// The QR state probe answers error 22 ("captcha required") when replayed with
	// the passport_csrf_token cookie that get_qrcode set: the official client's
	// poll is a stateless request carrying only the device context and token.
	// A send-code/sms-login flow does need its cookies, so this is scoped to the
	// anonymous QR check only.
	if path == "/passport/web/check_qrconnect/" {
		client.Jar = nil
		req.Header.Del("Cookie")
	}
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
