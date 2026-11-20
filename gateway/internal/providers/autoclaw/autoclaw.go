// Package autoclaw reverse-proxies the AutoClaw (AutoGLM / 智谱) desktop model
// channel as an OpenAI-compatible provider inside LoomGate.
//
// Unlike the Doubao path there is no client conversation protocol here. The
// upstream uses a chat/completions-shaped POST without a conversation identifier.
// Generic store/output-budget knobs are not sent because the service rejects
// them; omitting those fields is not a claim about the upstream retention policy.
// A request carries the account access token and the dialogue messages.
//
// Credentials live in the same encrypted accountvault as every other provider.
// The stored blob is the AutoClaw account record (accessToken / refreshToken /
// deviceId / phone); nothing here logs or returns it.
package autoclaw

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providerretry"
	"workbuddy2api/internal/upstream"
)

// Upstream facts. These are protocol constants of the AutoClaw desktop client,
// not secrets: the same values ship inside every copy of its installer.
const (
	apiBase   = "autoglm-acceleration-api.zhipuai.cn"
	chatPath  = "/autoclaw-proxy/proxy/autoclaw/chat/completions"
	refreshPt = "/userapi/v1/refresh"
	appID     = "100003"
	appKey    = "38d2391985e2369a5fb8227d8e6cd5e5"
	version   = "2.4.1"

	// The upstream requires this leading system instruction; without it the
	// request is rejected with a gate error.
	promptPrefix = "You are AutoClaw. Answer the user directly and concisely"
)

// Models is the fixed published set. AutoClaw does not expose a discovery
// endpoint, so the list is declared here and aliases map onto it.
// The AutoClaw channel serves the GLM-5.3 family, whose context window is 1M
// tokens. This is the window reported in the model picker; it is not a
// request-size cap and does not describe the output budget.
const contextWindow = 1_000_000

var Models = []inference.Model{
	{ID: "autoclaw:zai_glm-5.3-flash", Name: "GLM-5.3 Flash", ContextWindow: contextWindow, Incremental: true, MultiTurn: true, Credits: "1x"},
	{ID: "autoclaw:zai_auto", Name: "GLM Auto", ContextWindow: contextWindow, Incremental: true, MultiTurn: true, Credits: "1x"},
	{ID: "autoclaw:zaicoding_glm-5.3", Name: "GLM-5.3 Coding", ContextWindow: contextWindow, Incremental: true, MultiTurn: true, Credits: "1x"},
	{ID: "autoclaw:zai_glm-5.3-flashx-legal", Name: "GLM-5.3 FlashX", ContextWindow: contextWindow, Incremental: true, MultiTurn: true, Credits: "1x"},
}

var aliases = map[string]string{
	"glm-5.3-flash":        "zai_glm-5.3-flash",
	"glm-5.3":              "zai_glm-5.3-flash",
	"zai_glm-5.3":          "zai_glm-5.3-flash",
	"glm-5.3-flashx":       "zai_glm-5.3-flashx-legal",
	"glm-5.3-flashx-legal": "zai_glm-5.3-flashx-legal",
	"glm-5.3-coding":       "zaicoding_glm-5.3",
	"zaicoding-glm-5.3":    "zaicoding_glm-5.3",
	"auto":                 "zai_auto",
	"zai-autoglm":          "zai_auto",
}

// within returns the bare upstream model name for a public LoomGate model id.
// Unknown names fall back to the flash model, matching the desktop gateway so a
// typo never turns into a 500.
func within(model string) string {
	name := strings.TrimPrefix(model, "autoclaw:")
	if _, ok := aliases[name]; !ok {
		if isPublished(name) {
			return name
		}
	}
	if mapped, ok := aliases[name]; ok {
		return mapped
	}
	if isPublished(name) {
		return name
	}
	return "zai_glm-5.3-flash"
}

func isPublished(name string) bool {
	for _, m := range Models {
		if strings.TrimPrefix(m.ID, "autoclaw:") == name {
			return true
		}
	}
	return false
}

// stored is the encrypted profile for one AutoClaw account.
type stored struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	DeviceID         string `json:"deviceId,omitempty"`
	Phone            string `json:"phone,omitempty"`
	ExpiresAt        int64  `json:"expiresAt,omitempty"`
	RefreshPending   bool   `json:"refresh_pending,omitempty"`
	RefreshAttempt   string `json:"refresh_attempt,omitempty"`
	RefreshStartedAt int64  `json:"refresh_started_at,omitempty"`
}

// Provider implements inference.Provider for AutoClaw accounts.
type Provider struct {
	Vault  *accountvault.Vault
	Client *http.Client

	mu     sync.Mutex
	cursor int
	cool   *providerretry.Cooldown
}

func (p *Provider) PublicAccountID(id string) string {
	return publicAccountID(p.Vault, id)
}

func (p *Provider) httpClient() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 10 * time.Minute}
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

// accounts returns every enabled AutoClaw account in a stable order.
func (p *Provider) accounts() ([]accountvault.Account, error) {
	if p.Vault == nil {
		return nil, errors.New("autoclaw_vault_unavailable")
	}
	rows, err := p.Vault.List()
	if err != nil {
		return nil, err
	}
	out := make([]accountvault.Account, 0, len(rows))
	for _, a := range rows {
		if a.Provider == "autoclaw" && a.Enabled {
			out = append(out, a)
		}
	}
	return out, nil
}

func (p *Provider) session(id string) (stored, error) {
	_, s, err := p.loadSession(id)
	return s, err
}

func (p *Provider) loadSession(id string) (accountvault.Account, stored, error) {
	if p.Vault == nil {
		return accountvault.Account{}, stored{}, errors.New("autoclaw_vault_unavailable")
	}
	a, plain, err := p.Vault.Get(id)
	if err != nil {
		return accountvault.Account{}, stored{}, errors.New("autoclaw_session_unavailable")
	}
	defer wipe(plain)
	if a.Provider != "autoclaw" || !a.Enabled {
		return accountvault.Account{}, stored{}, errors.New("autoclaw_account_disabled")
	}
	var s stored
	if json.Unmarshal(plain, &s) != nil || s.AccessToken == "" {
		return accountvault.Account{}, stored{}, errors.New("autoclaw_session_invalid")
	}
	if s.RefreshPending {
		return accountvault.Account{}, stored{}, &refreshError{code: "autoclaw_refresh_pending"}
	}
	return a, s, nil
}

type refreshError struct{ code string }

func (e *refreshError) Error() string                   { return e.code }
func (e *refreshError) PublicCode() string              { return e.code }
func (e *refreshError) RetryClass() providerretry.Class { return providerretry.RetryAccount }

// A refresh token is single-use. The durable intent blocks every provider
// instance after an ambiguous exchange; only a newly committed login clears it.
func (p *Provider) refreshAccount(ctx context.Context, id, refreshToken string) (stored, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return stored{}, &refreshError{code: "autoclaw_refresh_unavailable"}
	}
	if err := ctx.Err(); err != nil {
		return stored{}, err
	}
	a, next, err := p.loadSession(id)
	if err != nil {
		return stored{}, err
	}
	if next.RefreshToken != refreshToken {
		return next, nil
	}
	var attempt [16]byte
	if _, err := rand.Read(attempt[:]); err != nil {
		return stored{}, &refreshError{code: "autoclaw_refresh_unavailable"}
	}
	next.RefreshPending = true
	next.RefreshAttempt = base64.RawURLEncoding.EncodeToString(attempt[:])
	next.RefreshStartedAt = time.Now().UTC().Unix()
	intent, err := json.Marshal(next)
	if err != nil {
		return stored{}, &refreshError{code: "autoclaw_refresh_unavailable"}
	}
	pending, err := p.Vault.Put(a, intent, a.Revision)
	wipe(intent)
	if err != nil {
		return stored{}, &refreshError{code: "autoclaw_refresh_conflict"}
	}
	access, refresh, err := exchangeRefresh(ctx, p.httpClient(), refreshToken, next.DeviceID)
	if err != nil {
		return stored{}, &refreshError{code: "autoclaw_refresh_pending"}
	}
	next.AccessToken, next.RefreshToken = access, refresh
	next.ExpiresAt = tokenExpiry(access)
	next.RefreshPending, next.RefreshAttempt, next.RefreshStartedAt = false, "", 0
	profile, err := json.Marshal(next)
	if err != nil {
		return stored{}, &refreshError{code: "autoclaw_refresh_pending"}
	}
	defer wipe(profile)
	if _, err := p.Vault.Put(pending, profile, pending.Revision); err != nil {
		return stored{}, &refreshError{code: "autoclaw_refresh_conflict"}
	}
	return next, nil
}

func refreshHint(status int, body []byte) string {
	if !needsTokenRefresh(status, string(body)) {
		return ""
	}
	return "token expired"
}

// Permission and quota failures must not consume a single-use refresh token.
func needsTokenRefresh(status int, body string) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	if status != http.StatusForbidden && status != http.StatusBadRequest {
		return false
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "token expired") || strings.Contains(lower, "token has expired") ||
		strings.Contains(lower, "invalid token") || strings.Contains(lower, "access_token expired")
}

// Models reports the fixed catalog. It never needs an account to answer, so the
// console can show the available models before one is added.
func (p *Provider) Models(context.Context) ([]inference.Model, error) {
	return append([]inference.Model(nil), Models...), nil
}

func (p *Provider) Complete(ctx context.Context, model, text string) (inference.Result, error) {
	return p.complete(ctx, model, []inference.Message{{Role: "user", Content: text}}, nil, "")
}

func (p *Provider) Stream(ctx context.Context, model, text string, emit func(string) error) (inference.Result, error) {
	if emit == nil {
		return inference.Result{}, inference.ErrStreamContract
	}
	return p.complete(ctx, model, []inference.Message{{Role: "user", Content: text}}, emit, "")
}

func (p *Provider) CompleteDialogue(ctx context.Context, q inference.Dialogue, emit func(string) error) (inference.Result, error) {
	return p.completeTurn(ctx, q, emit, nil, nil)
}

// SupportsTools reports that this upstream honours an OpenAI tools array. A
// probe with a tools definition returned finish_reason=tool_calls and a real
// tool_calls delta, so the capability is verified rather than assumed.
func (*Provider) SupportsTools() bool { return true }

// CompleteDialogueReasoning implements inference.ReasoningProvider. The
// upstream streams reasoning_content deltas for roughly the first second and
// only then starts the answer, so forwarding them is what makes the client
// show progress instead of a blank screen.
func (p *Provider) CompleteDialogueReasoning(ctx context.Context, q inference.Dialogue, emit, emitReasoning func(string) error) (inference.Result, error) {
	if emit == nil {
		return inference.Result{}, inference.ErrStreamContract
	}
	return p.completeTurn(ctx, q, emit, emitReasoning, nil)
}

// CompleteDialogueTools implements the tool-carrying path: the full transcript
// (including assistant tool_calls and tool results) plus the caller's tool
// catalog go upstream, and tool_call deltas come back to the caller.
func (p *Provider) CompleteDialogueTools(ctx context.Context, q inference.Dialogue, emit, emitReasoning func(string) error, emitTool func(inference.ToolCallDelta) error) (inference.Result, error) {
	return p.completeTurn(ctx, q, emit, emitReasoning, emitTool)
}

// complete runs one turn against the account pool. It rotates accounts only on
// failures that are plausibly account-scoped (auth, quota); a client error is
// returned as-is because another account cannot fix it.
func (p *Provider) complete(ctx context.Context, model string, messages []inference.Message, emit func(string) error, expectedAccount string) (inference.Result, error) {
	return p.completeTurn(ctx, inference.Dialogue{Model: model, Messages: messages, AccountID: expectedAccount}, emit, nil, nil)
}

// completeTurn is the single entry point for every request shape. Exactly one
// output mode is active: plain text, text+reasoning, or tool calls.
func (p *Provider) completeTurn(ctx context.Context, q inference.Dialogue, emit, emitReasoning func(string) error, emitTool func(inference.ToolCallDelta) error) (inference.Result, error) {
	if len(q.Messages) == 0 {
		return inference.Result{}, errors.New("autoclaw_messages_required")
	}
	expectedAccount := q.AccountID
	rows, err := p.accounts()
	if err != nil {
		return inference.Result{}, err
	}
	if len(rows) == 0 {
		return inference.Result{}, errors.New("autoclaw_no_account")
	}
	if expectedAccount != "" {
		filtered := rows[:0]
		for _, a := range rows {
			if a.ID == expectedAccount || publicAccountID(p.Vault, a.ID) == expectedAccount {
				filtered = append(filtered, a)
			}
		}
		if len(filtered) == 0 {
			return inference.Result{}, errors.New("autoclaw_session_invalid")
		}
		rows = filtered
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
	// Once a downstream callback has seen part of a generation, even a failure
	// classified as retryable cannot be spliced into that same stream.
	deliveryStarted := false
	trackText := func(fn func(string) error) func(string) error {
		if fn == nil {
			return nil
		}
		return func(delta string) error {
			deliveryStarted = deliveryStarted || delta != ""
			return fn(delta)
		}
	}
	textSink, reasoningSink := trackText(emit), trackText(emitReasoning)
	toolSink := emitTool
	if emitTool != nil {
		toolSink = func(delta inference.ToolCallDelta) error {
			deliveryStarted = true
			return emitTool(delta)
		}
	}
	var records []inference.Attempt
	var lastSubmitted inference.Result
	result, err := providerretry.Run(ctx, ids, 3, p.cooldown(), func(ctx context.Context, id string) (inference.Result, error) {
		value, turnErr := p.turnWith(ctx, byID[id], q, textSink, reasoningSink, toolSink)
		if value.Attempted {
			records = append(records, value.AttemptRecords...)
			lastSubmitted = value
		}
		if turnErr != nil && deliveryStarted {
			return value, &deliveredError{turnErr}
		}
		return value, turnErr
	})
	if err != nil {
		// Run returns a zero result on failure, but sent requests and observed
		// usage remain facts even when no account completed the turn.
		result = lastSubmitted
	}
	result.AttemptRecords = records
	result.Attempts = len(records)
	return result, err
}

type accountError struct {
	status int
	code   string
}

func (e *accountError) Error() string { return "autoclaw upstream http=" + strconv.Itoa(e.status) }

// PublicCode lets a dead credential surface as a credential problem instead of
// the generic 502 the console cannot act on. The console already has a label
// for access_token_expired; without this it was unreachable.
func (e *accountError) PublicCode() string {
	switch {
	case e.status == 401 || needsTokenRefresh(e.status, e.code):
		return "access_token_expired"
	case e.status == 403:
		return "autoclaw_forbidden"
	case e.status == 429:
		return "autoclaw_rate_limited"
	case e.status == 402:
		return "autoclaw_quota_exhausted"
	}
	return ""
}

// RetryClass keeps the same policy the legacy path uses: auth/quota/rate-limit
// and server faults implicate the account, a client error does not.
//
// 429/402 (rate limit, quota exhausted) get the long cooldown: retrying them
// after 30s just burns the same wall-clock wait again. 401/403 get the short
// one because the request path already tried a token renewal; reaching here
// means the account itself is bad, and the operator needs to see it fail
// rather than have it disappear for ten minutes.
func (e *accountError) RetryClass() providerretry.Class {
	switch {
	case e.status == 401 || e.status == 403 || e.status == 429 || e.status == 402 || e.status == 409:
		return providerretry.RetryAccount
	case e.status >= 500:
		return providerretry.RetryAccount
	}
	return providerretry.Fatal
}

// CooldownDuration lets a failure ask for a longer rest than the default.
func (e *accountError) CooldownDuration() time.Duration {
	if e.status == 429 || e.status == 402 {
		return 10 * time.Minute
	}
	return 0
}

// RetryClass for an empty answer: the account is fine, the upstream hiccupped.
func (e *emptyAnswerError) RetryClass() providerretry.Class { return providerretry.RetryTransient }

type emptyAnswerError struct{}

func (e *emptyAnswerError) Error() string { return "autoclaw_empty_answer" }

func (p *Provider) turnWith(ctx context.Context, row accountvault.Account, q inference.Dialogue, emit, emitReasoning func(string) error, emitTool func(inference.ToolCallDelta) error) (result inference.Result, retErr error) {
	s, err := p.session(row.ID)
	if err != nil {
		return result, err
	}
	body, err := buildDialogueBody(q.Model, q.Messages, q.Tools, q.ToolChoice, true)
	if err != nil {
		return result, err
	}
	defer wipe(body)
	defer func() {
		result.Attempts = len(result.AttemptRecords)
		if n := len(result.AttemptRecords); n > 0 {
			last := &result.AttemptRecords[n-1]
			if last.FinishedAt.IsZero() {
				last.FinishedAt = time.Now()
				last.Usage, last.FinishReason = result.Usage, result.FinishReason
				last.ErrorCode = attemptErrorCode(retErr)
			}
		}
	}()
	send := func(token string) (*http.Response, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiBase+chatPath, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		setHeaders(req, token, within(q.Model))
		result.Attempted = true
		result.AccountRef = publicAccountID(p.Vault, row.ID)
		result.AttemptRecords = append(result.AttemptRecords, inference.Attempt{AccountRef: result.AccountRef, StartedAt: time.Now()})
		index := len(result.AttemptRecords) - 1
		client := *p.httpClient()
		// 307/308 redirects can replay a billable POST outside attempt accounting.
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		last := &result.AttemptRecords[index]
		if resp != nil {
			last.HTTPStatus = resp.StatusCode
		}
		if err != nil {
			last.ErrorCode = attemptErrorCode(err)
			last.FinishedAt = time.Now()
		}
		return resp, err
	}
	rejection := func(resp *http.Response) *accountError {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		code := refreshHint(resp.StatusCode, raw)
		wipe(raw)
		failure := &accountError{status: resp.StatusCode, code: code}
		last := &result.AttemptRecords[len(result.AttemptRecords)-1]
		last.ErrorCode, last.FinishedAt = attemptErrorCode(failure), time.Now()
		return failure
	}
	resp, err := send(s.AccessToken)
	if err != nil {
		return result, err
	}
	if resp.StatusCode != http.StatusOK {
		failed := rejection(resp)
		if needsTokenRefresh(failed.status, failed.code) && s.RefreshToken != "" {
			renewed, err := p.refreshAccount(ctx, row.ID, s.RefreshToken)
			if err != nil {
				return result, err
			}
			resp, err = send(renewed.AccessToken)
			if err != nil {
				return result, err
			}
			if resp.StatusCode != http.StatusOK {
				return result, rejection(resp)
			}
		} else {
			return result, failed
		}
	}
	defer resp.Body.Close()
	buffered := emit == nil && emitTool == nil && emitReasoning == nil
	if emit == nil {
		emit = func(string) error { return nil }
	}
	if buffered {
		var reasoning strings.Builder
		emitReasoning = func(delta string) error { reasoning.WriteString(delta); return nil }
		defer func() { result.Reasoning = reasoning.String() }()
	}
	text, events, usage, calls, err := streamSSEWithTools(resp.Body, emit, emitReasoning, emitTool)
	result.Text, result.Events, result.ToolCalls = text, events, calls
	result.Usage = usage.observed()
	result.UsageReported = result.Usage.Reported()
	result.PromptTokens, result.CompletionTokens, result.CachedTokens = usage.Prompt, usage.Completion, usage.Cached
	result.FinishReason = usage.FinishReason
	if err != nil {
		return result, err
	}
	result.Complete, result.Acknowledged = true, true
	return result, nil
}

// forbiddenBodyFields never reach the upstream.
//
// The service rejects a client-supplied output budget with 406 and an EMPTY
// body, which is indistinguishable from every other 406 (bad banner, missing
// model). It cannot be diagnosed from the response, so the field has to be
// dropped before sending. max_output_tokens is the Responses-protocol twin.
var forbiddenBodyFields = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// strippedBodyFields are accepted by OpenAI clients but have no meaning on this
// endpoint; forwarding them risks the same opaque 406.
var strippedBodyFields = []string{
	"frequency_penalty", "presence_penalty", "logit_bias", "logprobs",
	"top_logprobs", "n", "seed", "user", "parallel_tool_calls", "store",
	"metadata", "service_tier", "response_format", "stop",
}

// foreignIdentities are phrases that make the CN gateway return 406: it
// requires the AutoClaw banner to be the assistant's identity and rejects a
// request that claims to be another agent (notably the Claude Agent SDK).
// Longest-first so a longer phrase is replaced before a shorter substring of it.
var foreignIdentities = []string{
	"you are claude code",
	"you are claude",
	"claude agent sdk",
	"you are anthropic's claude",
}

// buildBody renders the OpenAI-compatible request.
//
// The upstream is OpenAI-shaped but not OpenAI-compatible: it demands the
// AutoClaw banner in a system message, rejects client-supplied token budgets,
// and refuses bodies that identify as another agent. Each of those failures
// comes back as a 406 with no body, so all three are handled here rather than
// being diagnosed from a response.
func buildBody(model string, messages []inference.Message, stream bool) ([]byte, error) {
	return buildDialogueBody(model, messages, nil, nil, stream)
}

// buildDialogueBody also carries the caller's tool catalog. Tools and tool
// choice are forwarded verbatim: the upstream honours an OpenAI tools array
// (verified: a probe returned finish_reason=tool_calls), and rewriting a schema
// the gateway does not interpret would only break the caller's contract.
func buildDialogueBody(model string, messages []inference.Message, tools []inference.ToolDefinition, toolChoice any, stream bool) ([]byte, error) {
	msgs := make([]map[string]any, 0, len(messages)+1)
	hasBanner := false
	for _, m := range messages {
		if m.Role == "system" && strings.Contains(strings.ToLower(m.Content), strings.ToLower(promptPrefix)) {
			hasBanner = true
			break
		}
	}
	if !hasBanner {
		msgs = append(msgs, map[string]any{"role": "system", "content": promptPrefix + "."})
	}
	for _, m := range messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		content := m.Content
		if role == "system" {
			content = neutraliseIdentity(content)
		}
		entry := map[string]any{"role": role, "content": content}
		if len(m.ToolCalls) > 0 {
			calls := make([]any, 0, len(m.ToolCalls))
			for _, call := range m.ToolCalls {
				callType := call.Type
				if callType == "" {
					callType = "function"
				}
				calls = append(calls, map[string]any{
					"id":   call.ID,
					"type": callType,
					"function": map[string]any{
						"name":      call.Function.Name,
						"arguments": call.Function.Arguments,
					},
				})
			}
			entry["tool_calls"] = calls
		}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		msgs = append(msgs, entry)
	}
	payload := map[string]any{
		"model":    within(model),
		"messages": msgs,
		"stream":   stream,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
		if toolChoice != nil {
			payload["tool_choice"] = toolChoice
		}
	}
	for _, field := range forbiddenBodyFields {
		delete(payload, field)
	}
	for _, field := range strippedBodyFields {
		delete(payload, field)
	}
	return json.Marshal(payload)
}

// neutraliseIdentity replaces a foreign agent identity with the AutoClaw one.
// The CN gateway answers 406 when a request claims to be another agent, and a
// Claude Code / Agent SDK client always sends such a system prompt.
func neutraliseIdentity(content string) string {
	lower := strings.ToLower(content)
	for _, phrase := range foreignIdentities {
		if idx := strings.Index(lower, phrase); idx >= 0 {
			replacement := "You are AutoClaw"
			if phrase == "claude agent sdk" {
				replacement = "AutoClaw"
			}
			content = content[:idx] + replacement + content[idx+len(phrase):]
			lower = strings.ToLower(content)
		}
	}
	return content
}

func setHeaders(req *http.Request, accessToken, model string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "*")
	req.Header.Set("Content-Type", "application/json")
	// "node" is what the desktop client sends; the upstream gate is known to
	// be picky about it.
	// The Aliyun WAF answers 405 to Go's default UA and to a missing one; the
	// managed client identifies as AutoClaw/<version>.
	req.Header.Set("User-Agent", "AutoClaw/"+version)
	req.Header.Set("X-Agent-Id", "main")
	// Only attach the bearer pair when there is a token. The login and
	// refresh endpoints are unauthenticated, and sending "Bearer " with an
	// empty token is a malformed credential rather than an absent one.
	if accessToken != "" {
		req.Header.Set("X-Authorization", "Bearer "+accessToken)
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	req.Header.Set("x-channel", "cakegrowth")
	req.Header.Set("x-client-type", "pc")
	req.Header.Set("x-lang", "zh-CN")
	req.Header.Set("x-product", "autoclaw")
	req.Header.Set("x-request-model", model)
	// X-Tm accepts only win / mac / linux-x64; a bare "linux" is refused
	// outright by the overseas gateway.
	req.Header.Set("x-tm", "win")
	req.Header.Set("x-version", version)
	req.Header.Set("accept-encoding", "identity")

	// The released desktop gateway stamps four fresh ids plus a desktop trace
	// marker on every chat request. Omitting them is not a hard rejection, but
	// it makes each request look like an untagged bot call, and the upstream
	// then serves it on the slow path — the exact shape of our 120s TTFB
	// against the client's 9s.
	//
	// requestID and traceID share one value in the client, so they do here.
	requestID := newTraceID()
	req.Header.Set("x-query-id", newTraceID())
	req.Header.Set("x-request-id", requestID)
	req.Header.Set("x-session-id", newTraceID())
	req.Header.Set("x-trace-id", requestID)
	req.Header.Set("x_trace_id", "autoclaw-desktop")
	setSign(req)
}

// newTraceID returns a random UUIDv4 in the canonical hyphenated form the
// client uses for its per-request identifiers.
func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// aggregate folds an SSE body into plain text, for non-streaming callers.
func aggregate(raw []byte) (string, streamUsage, []inference.ToolCall, error) {
	var text strings.Builder
	var calls []inference.ToolCall
	text0, _, usage, toolCalls, err := streamSSEWithTools(bytes.NewReader(raw),
		func(delta string) error { text.WriteString(delta); return nil }, nil, nil)
	if err != nil {
		var empty *emptyAnswerError
		if errors.As(err, &empty) && len(toolCalls) > 0 {
			// A tool-only answer is not an empty answer. (A reasoning-only
			// answer never reaches this branch: reasoningSeen already makes
			// the stream a valid turn.)
			return "", usage, toolCalls, nil
		}
		return text0, usage, toolCalls, err
	}
	calls = toolCalls
	return text.String(), usage, calls, nil
}

// streamSSE forwards each text delta as it arrives and returns the joined text.
//
// The upstream streams reasoning_content deltas before any content: the desktop
// client renders those as "思考 Ns". They are counted as events and reported
// through onReasoning (when set) so the caller is not blind for the whole
// thinking phase, but they are never appended to the answer text.
func streamSSE(body io.Reader, emit func(string) error, onReasoning func(string) error) (string, int, streamUsage, error) {
	text, events, usage, _, err := streamSSEWithTools(body, emit, onReasoning, nil)
	return text, events, usage, err
}

// streamSSEWithTools additionally forwards tool_call fragments. Tool-call turns
// carry no answer text, so an empty text buffer is legal when the upstream
// reported any tool call — and so is a turn that only reasoned.
func streamSSEWithTools(body io.Reader, emit func(string) error, onReasoning func(string) error, onTool func(inference.ToolCallDelta) error) (string, int, streamUsage, []inference.ToolCall, error) {
	return decodeSSE(body, emit, onReasoning, onTool)
}

// streamUsage carries the upstream-reported token counts. A field is unknown
// until a chunk reports it; zero is a real value ("none cached"), so the
// presence flags decide what may be reported.
type streamUsage struct {
	Prompt, Completion, Cached int
	PromptSeen, CompletionSeen bool
	CachedSeen                 bool
	Credit                     *float64
	// FinishReason is the upstream's termination cause for this turn. Empty
	// means the upstream did not report one.
	FinishReason string
}

// parseUsage merges one chunk's usage object into the accumulated counts.
// Upstream sends it once, in the final chunk, but merging makes a repeated or
// split usage harmless.
func parseUsage(raw json.RawMessage, acc streamUsage) streamUsage {
	var u struct {
		PromptTokens         *int            `json:"prompt_tokens"`
		CompletionTokens     *int            `json:"completion_tokens"`
		PromptCacheHitTokens *int            `json:"prompt_cache_hit_tokens"`
		Credit               json.RawMessage `json:"credit"`
		PromptTokensDetails  *struct {
			CachedTokens *int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return acc
	}
	if u.PromptTokens != nil {
		acc.Prompt, acc.PromptSeen = *u.PromptTokens, true
	}
	if u.CompletionTokens != nil {
		acc.Completion, acc.CompletionSeen = *u.CompletionTokens, true
	}
	if u.PromptCacheHitTokens != nil {
		acc.Cached, acc.CachedSeen = *u.PromptCacheHitTokens, true
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
		acc.Cached, acc.CachedSeen = *u.PromptTokensDetails.CachedTokens, true
	}
	// Invalid optional billing metadata must not break a valid model response.
	if len(u.Credit) > 0 && u.Credit[0] != '"' {
		if value, ok := upstream.UsageCredit(json.Number(u.Credit)); ok && value <= 1e12 {
			acc.Credit = &value
		}
	}
	return acc
}

// tokenExpiry reads the JWT `exp` claim. Tokens that are not JWTs return 0,
// which the store treats as "unknown" rather than as expired.
func tokenExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return 0
	}
	return claims.Exp
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// RefreshToken performs a single bounded exchange. Account-backed callers must
// use refreshAccount so the single-use token has a durable intent first.
func RefreshToken(ctx context.Context, refreshToken string) (access, refresh string, err error) {
	return exchangeRefresh(ctx, nil, refreshToken, "")
}

func exchangeRefresh(ctx context.Context, client *http.Client, refreshToken, device string) (access, refresh string, err error) {
	if strings.TrimSpace(refreshToken) == "" {
		return "", "", errors.New("autoclaw_refresh_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload := map[string]any{"refresh_token": refreshToken, "source_id": "autoclaw"}
	if device != "" {
		payload["device_id"] = device
	}
	body, _ := json.Marshal(payload)
	defer wipe(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiBase+refreshPt, bytes.NewReader(body))
	if err != nil {
		return "", "", errors.New("autoclaw_refresh_unavailable")
	}
	setHeaders(req, "", "")
	req.Header.Del("X-Authorization")
	bounded := http.Client{Timeout: 30 * time.Second}
	if client != nil {
		bounded = *client
		if bounded.Timeout <= 0 || bounded.Timeout > 30*time.Second {
			bounded.Timeout = 30 * time.Second
		}
	}
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := bounded.Do(req)
	if err != nil {
		return "", "", errors.New("autoclaw_refresh_exchange_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New("autoclaw_refresh_rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	defer wipe(raw)
	if err != nil || len(raw) > 1<<20 {
		return "", "", errors.New("autoclaw_refresh_response_invalid")
	}
	var out struct {
		Code *int `json:"code"`
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Code == nil || *out.Code != 0 ||
		strings.TrimSpace(out.Data.AccessToken) == "" || strings.TrimSpace(out.Data.RefreshToken) == "" {
		return "", "", errors.New("autoclaw_refresh_response_invalid")
	}
	return out.Data.AccessToken, out.Data.RefreshToken, nil
}

// StoreProfile writes one account blob into the vault. `expected` is the
// revision the caller last saw (0 for a new account).
func StoreProfile(vault *accountvault.Vault, id string, profile []byte, expected uint64) (accountvault.Account, error) {
	if vault == nil {
		return accountvault.Account{}, errors.New("autoclaw_vault_unavailable")
	}
	return vault.Put(accountvault.Account{ID: id, Provider: "autoclaw", Enabled: true}, profile, expected)
}
