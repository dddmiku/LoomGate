// Package doubao reverse-proxies Doubao's 工作模式 (work mode) completion
// endpoint as a first-class LoomGate provider.
//
// The protocol is the one the official desktop client speaks to
// POST https://www.doubao.com/chat/completion. It is driven here from a stored
// login Cookie plus a request template built in Go, with no desktop client and
// no captured request body. The full work-mode envelope matters: an incomplete
// one is accepted by the service but never produces an answer.
//
// Be explicit about one property: the upstream protocol IS conversation-shaped.
// `client_meta`, `need_create_conversation` and the server-assigned
// conversation id are part of the wire format, so a turn creates a Doubao-side
// conversation. The provider pins one conversation per account and never opens
// a second one, but it cannot avoid the first. Callers who need literally zero
// upstream conversation state must use AutoClaw instead.
package doubao

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providerretry"
)

const (
	chatURL = "https://www.doubao.com/chat/completion"
	// botID is the default assistant the work-mode models are scoped to. The
	// account's own binding overrides it when present.
	defaultBotID = "7338286299411103781"
	modeID       = "3"
)

// Model is one published model. Upstream is the model_item_key.
type Model struct {
	ID       string
	Upstream string
	Name     string
	Window   int64
	Profile  string
	// Credits is the published billing multiplier, exactly as the service
	// states it ("免费", "0.01x", "0.65x"). Empty = not published.
	Credits string
}

// Models is the published catalog. The keys are the values the desktop client
// sees today; an unknown requested name falls back to the default model rather
// than erroring, matching the reference gateway.
var Models = []Model{
	{ID: "auto", Upstream: "9", Name: "自动", Window: 256000, Profile: "256k", Credits: "免费"},
	{ID: "doubao-2.1-lite", Upstream: "seed-lite-7b", Name: "豆包 2.1 Lite", Window: 256000, Profile: "256k", Credits: "0.01x"},
	{ID: "doubao-2.1-pro", Upstream: "5", Name: "豆包 2.1 Pro", Window: 256000, Profile: "256k", Credits: "0.65x"},
	// Ids and names come from the service's own catalog
	// (evidence/doubao-model-catalog-safe.json). Note that Flash is V4.1 while
	// Pro is plain V4 — the upstream really does name them differently.
	{ID: "deepseek-v4.1-flash", Upstream: "person-deepseek-v4-1-flash-261017", Name: "DeepSeek V4.1 Flash", Window: 300000, Profile: "300k_230", Credits: "0.01x"},
	{ID: "deepseek-v4-pro", Upstream: "person-deepseek-v4-pro-ga-260813", Name: "DeepSeek V4 Pro", Window: 300000, Profile: "300k_230", Credits: "0.46x"},
	{ID: "kimi-k3", Upstream: "person-kimi-k3-261111", Name: "Kimi K3", Window: 300000, Profile: "300k_230", Credits: "1.48x"},
	{ID: "kimi-k3.1-pro", Upstream: "person-kimi-k3-1-pro-261111", Name: "Kimi K3.1 Pro", Window: 300000, Profile: "300k_230", Credits: "0.95x"},
	{ID: "kimi-k3.1-pro-fast", Upstream: "person-kimi-k3-1-pro-highspeed-261111", Name: "Kimi K3.1 Pro 极速", Window: 300000, Profile: "300k_230", Credits: "1.44x"},
	{ID: "glm-5.3", Upstream: "person-glm-5-3-261111", Name: "GLM-5.3", Window: 300000, Profile: "300k_230", Credits: "0.80x"},
	{ID: "glm-5.3-flash", Upstream: "person-glm-5-3-flash-261111", Name: "GLM-5.3 Flash", Window: 300000, Profile: "300k_230", Credits: "0.08x"},
}

const defaultModel = "deepseek-v4.1-flash"

// legacyAliases maps ids that were in use before the catalog names were
// corrected onto the current ids. A client configured with an old spelling
// keeps working instead of silently falling back to the default model.
var legacyAliases = map[string]string{
	"deepseek-v4-flash": "deepseek-v4.1-flash",
	"deepseek-v4.1-pro": "deepseek-v4-pro",
	"deepseek-v4":       "deepseek-v4-pro",
}

// query is the fixed client identity the endpoint expects. These are protocol
// parameters, not secrets: the same values ship in every client build.
var baseQuery = url.Values{
	"aid":                    {"582478"},
	"real_aid":               {"582478"},
	"channel":                {"win"},
	"client_platform":        {"pc_client"},
	"device_platform":        {"web"},
	"doubao_device_platform": {"desktop"},
	"doubao_pc_version":      {"2.30.1"},
	"pc_version":             {"2.30.1"},
	"pkg_type":               {"release_version"},
	"language":               {"zh"},
	"region":                 {"CN"},
	"sys_region":             {"CN"},
	"runtime":                {"web"},
	"runtime_version":        {"3.37.4"},
	"samantha_web":           {"1"},
	"use-olympus-account":    {"1"},
	"version_code":           {"20800"},
	"web_platform":           {"desktop"},
}

// Stored is the encrypted profile for one Doubao account.
//
// A profile is produced either by the gateway's own passport login (which fills
// Query/Headers with the real device identity the service assigned to this
// session) or by a plain Cookie import (which fills only Cookie). Both are
// valid: the request builder prefers the login-issued Query/Headers when they
// are present and falls back to the fixed client defaults otherwise.
type Stored struct {
	Cookie  string      `json:"cookie,omitempty"`
	Query   url.Values  `json:"query,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	BotID   string      `json:"bot_id,omitempty"`
	Device  string      `json:"device_id,omitempty"`
	WebID   string      `json:"web_id,omitempty"`
	LocalID string      `json:"local_app_id,omitempty"`
	// Conversation is the single server-side conversation this account reuses.
	Conversation string `json:"conversation_id,omitempty"`
	BindingFlow  string `json:"binding_flow,omitempty"`
	VerifiedAt   time.Time `json:"verified_at,omitempty"`
	Mobile       string    `json:"mobile,omitempty"`
	DisplayName  string    `json:"display_name,omitempty"`
}

type Provider struct {
	Vault  *accountvault.Vault
	Client *http.Client

	mu     sync.Mutex
	cursor int
	cool   *providerretry.Cooldown
}

// cooldown returns the provider's cool-down table, creating it on first use.
func (p *Provider) cooldown() *providerretry.Cooldown {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cool == nil {
		p.cool = providerretry.NewCooldown()
	}
	return p.cool
}

func (p *Provider) httpClient() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 0}
}

func (p *Provider) accounts() ([]accountvault.Account, error) {
	if p.Vault == nil {
		return nil, errors.New("doubao_vault_unavailable")
	}
	rows, err := p.Vault.List()
	if err != nil {
		return nil, err
	}
	out := make([]accountvault.Account, 0, len(rows))
	for _, a := range rows {
		if a.Provider == "doubao" && a.Enabled {
			out = append(out, a)
		}
	}
	return out, nil
}

func (p *Provider) session(id string) (Stored, error) {
	_, plain, err := p.Vault.Get(id)
	if err != nil {
		return Stored{}, err
	}
	defer wipe(plain)
	var s Stored
	if json.Unmarshal(plain, &s) != nil {
		return Stored{}, errors.New("doubao_session_invalid")
	}
	if strings.TrimSpace(s.Cookie) == "" && len(s.Headers) == 0 {
		return Stored{}, errors.New("doubao_session_invalid")
	}
	return s, nil
}

func (p *Provider) Models(context.Context) ([]inference.Model, error) {
	out := make([]inference.Model, 0, len(Models))
	for _, m := range Models {
		out = append(out, inference.Model{
			ID: "doubao:" + m.ID, Name: m.Name, ContextWindow: m.Window,
			Incremental: true, MultiTurn: true, Credits: m.Credits,
		})
	}
	return out, nil
}

func (p *Provider) Complete(ctx context.Context, model, text string) (inference.Result, error) {
	return p.complete(ctx, model, []inference.Message{{Role: "user", Content: text}}, nil)
}

func (p *Provider) Stream(ctx context.Context, model, text string, emit func(string) error) (inference.Result, error) {
	if emit == nil {
		return inference.Result{}, inference.ErrStreamContract
	}
	return p.complete(ctx, model, []inference.Message{{Role: "user", Content: text}}, emit)
}

func (p *Provider) CompleteDialogue(ctx context.Context, q inference.Dialogue, emit func(string) error) (inference.Result, error) {
	return p.complete(ctx, q.Model, q.Messages, emit)
}

func resolve(model string) Model {
	name := strings.TrimPrefix(model, "doubao:")
	if canonical, ok := legacyAliases[name]; ok {
		name = canonical
	}
	for _, m := range Models {
		if m.ID == name {
			return m
		}
	}
	for _, m := range Models {
		if m.ID == defaultModel {
			return m
		}
	}
	return Models[0]
}

func (p *Provider) complete(ctx context.Context, model string, messages []inference.Message, emit func(string) error) (inference.Result, error) {
	if len(messages) == 0 {
		return inference.Result{}, errors.New("doubao_messages_required")
	}
	rows, err := p.accounts()
	if err != nil {
		return inference.Result{}, err
	}
	if len(rows) == 0 {
		return inference.Result{}, errors.New("doubao_no_account")
	}
	p.mu.Lock()
	start := p.cursor
	p.cursor = (p.cursor + 1) % len(rows)
	p.mu.Unlock()

	order := make([]accountvault.Account, len(rows))
	for i := range rows {
		order[i] = rows[(start+i)%len(rows)]
	}
	ids := make([]string, len(order))
	byID := make(map[string]accountvault.Account, len(order))
	for i, row := range order {
		ids[i] = row.ID
		byID[row.ID] = row
	}
	// A transient empty answer is retried on the same account (the caller saw
	// nothing yet, so no duplicate output is possible); account-scoped failures
	// rotate, and the shared runner keeps both bounded.
	return providerretry.Run(ctx, ids, 3, p.cooldown(), func(ctx context.Context, id string) (inference.Result, error) {
		return p.turn(ctx, byID[id], model, messages, emit)
	})
}

type accountError struct {
	status int
	body   string
}

func (e *accountError) Error() string {
	if e.body == "" {
		return "doubao upstream http=" + strconv.Itoa(e.status)
	}
	return "doubao upstream http=" + strconv.Itoa(e.status) + " " + e.body
}

// summarizeBody keeps a short, non-secret fingerprint of an unexpected response
// so a failing shape can be diagnosed without logging a full payload. Only the
// code/message fields are surfaced; the body is otherwise discarded.
func summarizeBody(raw []byte) string {
	if len(raw) == 0 {
		return "empty"
	}
	var probe struct {
		Code     int    `json:"code"`
		Message  string `json:"message"`
		Msg      string `json:"msg"`
		ErrorMsg string `json:"error_msg"`
	}
	if json.Unmarshal(raw, &probe) == nil {
		switch {
		case probe.Message != "":
			return "code=" + strconv.Itoa(probe.Code) + " msg=" + truncate(probe.Message, 120)
		case probe.Msg != "":
			return "code=" + strconv.Itoa(probe.Code) + " msg=" + truncate(probe.Msg, 120)
		case probe.ErrorMsg != "":
			return "code=" + strconv.Itoa(probe.Code) + " msg=" + truncate(probe.ErrorMsg, 120)
		}
	}
	prefix := string(raw)
	if len(prefix) > 80 {
		prefix = prefix[:80]
	}
	return "content-type-not-event-stream prefix=" + truncate(strings.TrimSpace(prefix), 80)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// RetryClass keeps the same policy the legacy path uses.
func (e *accountError) RetryClass() providerretry.Class {
	if e.status == 401 || e.status == 403 || e.status == 429 || e.status >= 500 {
		return providerretry.RetryAccount
	}
	return providerretry.Fatal
}

// emptyAnswer is a transient upstream hiccup: the turn was acknowledged and
// then produced nothing. The account itself is healthy.
type emptyAnswer struct{}

func (emptyAnswer) Error() string                    { return "doubao_empty_answer" }
func (emptyAnswer) RetryClass() providerretry.Class { return providerretry.RetryTransient }

var errEmptyAnswer = emptyAnswer{}

// ErrStreamTruncated means the reply stream ended without the terminal
// SSE_REPLY_END (end_type=3). Any text received before that point is partial:
// the model had not finished when the connection closed. It is not retryable —
// the user already saw the partial answer — and it must never be reported as a
// completed turn.
var ErrStreamTruncated = errors.New("doubao_stream_truncated")

// send issues one work-mode request with the stored session identity.
func (p *Provider) send(ctx context.Context, s Stored, body []byte) (*http.Response, error) {
	query := baseQuery
	if len(s.Query) > 0 {
		query = mergeQuery(baseQuery, s.Query)
	}
	u := "https://www.doubao.com/chat/completion?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("agw-js-conv", "str, str")
	req.Header.Set("last-event-id", "undefined")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://www.doubao.com/")
	if cookie := sessionCookie(s); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	for key, list := range s.Headers {
		switch key {
		case "Cookie", "Content-Length", "Host", "Connection", "Accept-Encoding":
			continue
		}
		for _, v := range list {
			req.Header.Set(key, v)
		}
	}
	return p.httpClient().Do(req)
}

func (p *Provider) turn(ctx context.Context, row accountvault.Account, model string, messages []inference.Message, emit func(string) error) (inference.Result, error) {
	s, err := p.session(row.ID)
	if err != nil {
		return inference.Result{}, err
	}
	m := resolve(model)
	body, err := buildBody(s, m, messages)
	if err != nil {
		return inference.Result{}, err
	}
	resp, err := p.send(ctx, s, body)
	if err != nil {
		return inference.Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return inference.Result{}, &accountError{status: resp.StatusCode, body: summarizeBody(raw)}
	}

	// A 200 that is not an event stream is a protocol mismatch, not an empty
	// answer. Record its fingerprint so the failure is diagnosable instead of
	// being reported as "upstream returned nothing".
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); !strings.Contains(ct, "event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return inference.Result{Attempted: true, Attempts: 1},
			&accountError{status: resp.StatusCode, body: "non-sse content-type=" + ct + " " + summarizeBody(raw)}
	}

	result := inference.Result{Attempted: true, Attempts: 1}
	if emit == nil {
		text, _, err := pumpAll(resp.Body)
		if err != nil {
			return result, err
		}
		if strings.TrimSpace(text) == "" {
			return result, errEmptyAnswer
		}
		result.Text = text
		result.Complete = true
		result.Acknowledged = true
		result.Events = 1
		return result, nil
	}
	text, _, events, err := pumpStream(resp.Body, emit)
	result.Text = text
	result.Events = events
	result.Complete = err == nil
	result.Acknowledged = true
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(text) == "" {
		return result, errEmptyAnswer
	}
	return result, nil
}

// buildBody renders the complete 工作模式 envelope. Every block here is required:
// the service accepts a partial body and then waits for a runtime that never
// answers, which is the failure mode this shape replaces.
func buildBody(s Stored, m Model, messages []inference.Message) ([]byte, error) {
	prompt := flatten(messages)
	msgID := newUUID()
	localConv := "local_" + randomDigits(16)
	now := time.Now().UnixMilli()
	bot := s.BotID
	if bot == "" {
		bot = defaultBotID
	}
	// Every request starts its own conversation. The service always creates a
	// conversation for a chat turn — there is no storage-off switch and no
	// delete endpoint — so pinning one id per account only meant that every
	// caller's prompt landed in a single shared conversation in the user's
	// Doubao app. Emptying it here makes each call independent: the app still
	// records the turn, but unrelated callers never see each other's history
	// and nothing overwrites the user's own chats.
	convID := ""

	block := map[string]any{
		"block_type": 10000,
		"content": map[string]any{
			"text_block":     map[string]any{"text": prompt, "icon_url": "", "icon_url_dark": "", "summary": ""},
			"pc_event_block": "",
		},
		"block_id":      newUUID(),
		"parent_id":     "",
		"meta_info":     []any{},
		"append_fields": []any{},
	}
	body := map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": localConv,
			"conversation_id":       convID,
			"bot_id":                bot,
			"last_section_id":       "",
			"last_message_index":    nil,
			"local_permissions": []any{
				map[string]any{"permission_name": "ACCESS_COARSE_LOCATION", "status": 1},
				map[string]any{"permission_name": "ACCESS_FINE_LOCATION", "status": 1},
				map[string]any{"permission_name": "ACCESS_BACKGROUND_LOCATION", "status": 1},
			},
		},
		"messages": []any{map[string]any{
			"local_message_id": msgID,
			"content_block":    []any{block},
			"message_status":   0,
		}},
	}

	taskParam := map[string]any{
		"runtime_type":      2,
		"sandbox_auth_type": 1,
		"device_name":       deviceName(s),
		"folder_name":       "",
		"local_app_id":      localAppID(s),
		"local_device_id":   s.Device,
		"workspace":         "",
	}
	clientOpt := map[string]any{
		"enable_sandbox":     false,
		"os":                 "Windows",
		"shared_folder_path": []string{},
		"agent_workspace": map[string]any{
			"agent_workspace":   "",
			"local_skill_paths": []string{},
		},
		"client_env_id":     newUUID(),
		"sandbox_id":        "sandbox-" + newUUID(),
		"sandbox_auth_type": 1,
	}
	gtp := map[string]any{
		"action":                  0,
		"thread_local_message_id": []string{msgID},
		"client_option":           clientOpt,
		"runtime_type":            2,
		"agent_task_param":        taskParam,
		"agent_task_param_change": map[string]any{
			"runtime_changed": false, "device_changed": false, "sandbox_auth_type_changed": false,
		},
		"need_modify_conversation": false,
		"task_input_json": mustJSON(map[string]any{
			"agents_md":       map[string]any{"files": []any{}, "state": 2},
			"schema_version":  1,
			"home_dir":        "",
			"project_context": map[string]any{},
			"localConnectors": []any{},
		}),
	}
	option := map[string]any{
		"create_time_ms":            now,
		"answer_with_suggest":       false,
		"agent_mode":                1,
		"need_deep_think":           10001,
		"unique_key":                newUUID(),
		"start_seq":                 0,
		"need_create_conversation":  convID == "",
		"conversation_init_option":  map[string]any{"need_ack_conversation": true},
		"sse_recv_event_options":    map[string]any{"support_chunk_delta": true},
		"support_lazy_fetch_stream": true,
		"is_old_user":               true,
		"message_storage_type":      0,
		"conversation_mode":         2,
		"conversation_init_ext": map[string]any{
			"model_item_key":   m.Upstream,
			"reasoning_effort": "4",
			"mode_id":          modeID,
			"agent_task_param": mustJSON(taskParam),
		},
		"model_config": map[string]any{
			"model_item_key": m.Upstream,
			"model_extra_params": map[string]any{
				"memory_profile":    m.Profile,
				"total_window_size": strconv.FormatInt(m.Window, 10),
			},
			"reasoning_effort": 4,
		},
		"aggregate_params": map[string]any{
			"mention_skill_list":  "[]",
			"mention_plugin_list": "[]",
			"mention_ext":         "[{}]",
			"conversation_mode":   "2",
			"mode_id":             modeID,
			"model_item_key":      m.Upstream,
			"agent_mode":          "1",
			"reasoning_effort":    "4",
			"provider_id":         "",
		},
		"general_task_param": gtp,
		"ext": map[string]any{
			"general_task_param":            mustJSON(gtp),
			"use_deep_think":                "10001",
			"agent_mode":                    "1",
			"is_finish":                     "1",
			"conversation_init_option":      `{"need_ack_conversation":true}`,
			"commerce_credit_config_enable": "0",
		},
	}
	body["option"] = option
	body["user_context"] = []any{}
	return json.Marshal(body)
}

func flatten(messages []inference.Message) string {
	if len(messages) == 1 {
		return messages[0].Content
	}
	var b strings.Builder
	for _, m := range messages {
		if m.Role == "assistant" {
			b.WriteString("助手：")
		} else {
			b.WriteString("用户：")
		}
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// mergeQuery overlays the login-issued device identity on the fixed defaults.
func mergeQuery(base, override url.Values) url.Values {
	out := url.Values{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// sessionCookie prefers an explicit Cookie field, then a Cookie header captured
// during login, then the profile's cookie jar serialization.
func sessionCookie(s Stored) string {
	if strings.TrimSpace(s.Cookie) != "" {
		return s.Cookie
	}
	return s.Headers.Get("Cookie")
}

func deviceName(s Stored) string {
	if s.WebID != "" {
		return "PC-" + s.WebID
	}
	return "PC"
}

func localAppID(s Stored) string {
	if s.LocalID != "" {
		return s.LocalID
	}
	return "582478"
}

// pumpStream forwards text as it arrives. `sawChunkDelta` mirrors the reference
// implementation: when the service uses CHUNK_DELTA the STREAM_CHUNK text is a
// duplicate snapshot and must not be emitted twice.
//
// The stream is only complete on SSE_REPLY_END with end_type=3 (ReplyFinish).
// end_type=1 (MsgFinish) closes one message, 2 (AnswerFinish) closes the answer
// stage; neither marks the reply terminal. Treating a bare EOF as success made
// a mid-reply disconnect look like a finished answer, so the caller gets the
// partial text with ErrStreamTruncated instead.
func pumpStream(r io.Reader, emit func(string) error) (text, conversation string, events int, err error) {
	var out strings.Builder
	sawDelta := false
	sawReplyFinish := false
	tmp := make([]byte, 16<<10)
	pending := ""
	flush := func(frame string) error {
		ev, data := parseFrame(frame)
		if ev == "" || data == "" {
			return nil
		}
		switch ev {
		case "SSE_REPLY_END":
			var d struct {
				EndType int `json:"end_type"`
			}
			if json.Unmarshal([]byte(data), &d) == nil && d.EndType == 3 {
				sawReplyFinish = true
			}
		case "CHUNK_DELTA":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(data), &d) == nil && d.Text != "" {
				sawDelta = true
				events++
				if err := emit(d.Text); err != nil {
					return err
				}
				out.WriteString(d.Text)
			}
		case "STREAM_CHUNK":
			var d struct {
				PatchOp []struct {
					PatchValue struct {
						ContentBlock []struct {
							BlockType int `json:"block_type"`
							Content   struct {
								TextBlock *struct {
									Text string `json:"text"`
								} `json:"text_block"`
							} `json:"content"`
						} `json:"content_block"`
					} `json:"patch_value"`
				} `json:"patch_op"`
			}
			if json.Unmarshal([]byte(data), &d) != nil {
				return nil
			}
			for _, op := range d.PatchOp {
				for _, b := range op.PatchValue.ContentBlock {
					if b.BlockType == 10000 && !sawDelta && b.Content.TextBlock != nil && b.Content.TextBlock.Text != "" {
						events++
						if err := emit(b.Content.TextBlock.Text); err != nil {
							return err
						}
						out.WriteString(b.Content.TextBlock.Text)
					}
				}
			}
		case "SSE_ACK":
			var d struct {
				AckClientMeta struct {
					ConversationID string `json:"conversation_id"`
				} `json:"ack_client_meta"`
			}
			if json.Unmarshal([]byte(data), &d) == nil && d.AckClientMeta.ConversationID != "" {
				conversation = d.AckClientMeta.ConversationID
			}
		}
		return nil
	}
	for {
		n, readErr := r.Read(tmp)
		if n > 0 {
			pending += string(tmp[:n])
			for {
				idx, sep := frameEnd(pending)
				if idx < 0 {
					break
				}
				frame := pending[:idx]
				pending = pending[idx+sep:]
				if err = flush(frame); err != nil {
					return out.String(), conversation, events, err
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if strings.TrimSpace(pending) != "" {
					if err = flush(pending); err != nil {
						return out.String(), conversation, events, err
					}
				}
				if !sawReplyFinish {
					return out.String(), conversation, events, ErrStreamTruncated
				}
				return out.String(), conversation, events, nil
			}
			return out.String(), conversation, events, readErr
		}
	}
}

func pumpAll(r io.Reader) (text, conversation string, err error) {
	var out strings.Builder
	events := 0
	text, conversation, events, err = pumpStream(r, func(s string) error {
		out.WriteString(s)
		return nil
	})
	_ = events
	return text, conversation, err
}

// frameEnd returns the offset of the end of the next SSE frame and the length
// of its separator. A frame may be followed by more than one blank line (a
// joined stream commonly produces three newlines), so the separator consumes
// every consecutive newline; leaving a stray one would make the next frame
// start with a blank line and the "event:" prefix would never be seen.
func frameEnd(s string) (idx, sep int) {
	i := 0
	for i < len(s) {
		if i+3 < len(s) && s[i] == '\r' && s[i+1] == '\n' && s[i+2] == '\r' && s[i+3] == '\n' {
			j := i + 4
			for j < len(s) && (s[j] == '\n' || s[j] == '\r') {
				j++
			}
			return i, j - i
		}
		if i+1 < len(s) && s[i] == '\n' && s[i+1] == '\n' {
			j := i + 2
			for j < len(s) && (s[j] == '\n' || s[j] == '\r') {
				j++
			}
			return i, j - i
		}
		i++
	}
	return -1, 0
}

func parseFrame(frame string) (event, data string) {
	var dataLines []string
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return event, strings.Join(dataLines, "\n")
}

// Conversation ids are deliberately not persisted: each request starts its own
// conversation so one caller's prompts never land in another caller's history.
// The service still records every turn in the account's app — that is a
// protocol property, not a setting — but the gateway no longer funnels everyone
// into a single pinned conversation.

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
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

func randomDigits(n int) string {
	var out strings.Builder
	for i := 0; i < n; i++ {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			out.WriteByte('0')
			continue
		}
		out.WriteByte('0' + b[0]%10)
	}
	return out.String()
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// SaveProfile stores a Cookie-imported account. `expected` is the revision the
// caller last saw (0 for a new account).
func SaveProfile(vault *accountvault.Vault, id string, profile []byte, expected uint64) (accountvault.Account, error) {
	if vault == nil {
		return accountvault.Account{}, errors.New("doubao_vault_unavailable")
	}
	var s Stored
	if json.Unmarshal(profile, &s) != nil || strings.TrimSpace(s.Cookie) == "" {
		return accountvault.Account{}, errors.New("doubao_profile_invalid")
	}
	return vault.Put(accountvault.Account{ID: id, Provider: "doubao", Enabled: true}, profile, expected)
}
