// Package autoclaw reverse-proxies the AutoClaw (AutoGLM / 智谱) desktop model
// channel as an OpenAI-compatible provider inside LoomGate.
//
// Unlike the Doubao path there is no client conversation protocol here: the
// upstream endpoint is itself OpenAI-compatible (`/autoclaw-proxy/proxy/
// autoclaw/chat/completions`), it stores nothing (`store:false`), and it needs
// no per-conversation identifier. A request is a plain POST with the account's
// access token, so "only the model, nothing of the client" is the normal mode
// rather than an aspiration.
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
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	DeviceID     string `json:"deviceId,omitempty"`
	Phone        string `json:"phone,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
}

// Provider implements inference.Provider for AutoClaw accounts.
type Provider struct {
	Vault  *accountvault.Vault
	Client *http.Client

	mu     sync.Mutex
	cursor int
	cool   *providerretry.Cooldown
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
	_, plain, err := p.Vault.Get(id)
	if err != nil {
		return stored{}, err
	}
	defer wipe(plain)
	var s stored
	if json.Unmarshal(plain, &s) != nil || s.AccessToken == "" {
		return stored{}, errors.New("autoclaw_session_invalid")
	}
	return s, nil
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
func (p *Provider) CompleteDialogueTools(ctx context.Context, q inference.Dialogue, emit func(inference.ToolCallDelta) error) (inference.Result, error) {
	return p.completeTurn(ctx, q, nil, nil, emit)
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
			if a.ID == expectedAccount {
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
	return providerretry.Run(ctx, ids, 3, p.cooldown(), func(ctx context.Context, id string) (inference.Result, error) {
		return p.turnWith(ctx, byID[id], q, emit, emitReasoning, emitTool)
	})
}

type accountError struct {
	status int
	code   string
}

func (e *accountError) Error() string { return "autoclaw upstream: " + e.code }

// RetryClass keeps the same policy the legacy path uses: auth/quota/rate-limit
// and server faults implicate the account, a client error does not.
func (e *accountError) RetryClass() providerretry.Class {
	switch {
	case e.status == 401 || e.status == 403 || e.status == 429 || e.status == 402 || e.status == 409:
		return providerretry.RetryAccount
	case e.status >= 500:
		return providerretry.RetryAccount
	}
	return providerretry.Fatal
}

// RetryClass for an empty answer: the account is fine, the upstream hiccupped.
func (e *emptyAnswerError) RetryClass() providerretry.Class { return providerretry.RetryTransient }

type emptyAnswerError struct{}

func (e *emptyAnswerError) Error() string { return "autoclaw_empty_answer" }



func (p *Provider) turnWith(ctx context.Context, row accountvault.Account, q inference.Dialogue, emit, emitReasoning func(string) error, emitTool func(inference.ToolCallDelta) error) (inference.Result, error) {
	model, messages := q.Model, q.Messages
	s, err := p.session(row.ID)
	if err != nil {
		return inference.Result{}, err
	}
	body, err := buildDialogueBody(model, messages, q.Tools, q.ToolChoice, emit != nil || emitTool != nil)
	if err != nil {
		return inference.Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiBase+chatPath, bytes.NewReader(body))
	if err != nil {
		return inference.Result{}, err
	}
	setHeaders(req, s.AccessToken, within(model))

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return inference.Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		code := strings.TrimSpace(string(raw))
		if len(code) > 300 {
			code = code[:300]
		}
		return inference.Result{}, &accountError{status: resp.StatusCode, code: "http=" + strconv.Itoa(resp.StatusCode) + " " + code}
	}

	result := inference.Result{Attempted: true, Attempts: 1}
	// A tool turn has no text emitter but still needs the streaming parser:
	// tool_calls arrive as deltas, so buffering the body and aggregating text
	// would drop the call and report an empty answer.
	if emit == nil && emitTool == nil {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return result, err
		}
		text, usage, toolCalls, err := aggregate(raw)
		if err != nil {
			return result, err
		}
		result.Text = text
		result.ToolCalls = toolCalls
		result.UsageReported = usage.PromptSeen || usage.CompletionSeen
		result.PromptTokens = usage.Prompt
		result.CompletionTokens = usage.Completion
		result.CachedTokens = usage.Cached
		result.Complete = true
		result.Acknowledged = true
		result.Events = 1
		return result, nil
	}

	if emit == nil {
		// parseToolCallsOnly consumes the stream without forwarding prose.
		emit = func(string) error { return nil }
	}
	text, events, usage, toolCalls, err := streamSSEWithTools(resp.Body, emit, emitReasoning, emitTool)
	result.Text = text
	result.Events = events
	result.Attempted = true
	result.UsageReported = usage.PromptSeen || usage.CompletionSeen
	result.PromptTokens = usage.Prompt
	result.CompletionTokens = usage.Completion
	result.CachedTokens = usage.Cached
	if err != nil {
		return result, err
	}
	result.ToolCalls = toolCalls
	result.Complete = true
	result.Acknowledged = true
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
			// A tool-only answer is not an empty answer.
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
// reported any tool call.
func streamSSEWithTools(body io.Reader, emit func(string) error, onReasoning func(string) error, onTool func(inference.ToolCallDelta) error) (string, int, streamUsage, []inference.ToolCall, error) {
	var text strings.Builder
	var usage streamUsage
	var toolCalls []inference.ToolCall
	events := 0
	// Read line by line: SSE frames are newline delimited and deltas must be
	// forwarded immediately, not buffered until the stream ends.
	tmp := make([]byte, 8192)
	pending := ""
	for {
		n, err := body.Read(tmp)
		if n > 0 {
			pending += string(tmp[:n])
			for {
				idx := strings.IndexByte(pending, '\n')
				if idx < 0 {
					break
				}
				line := strings.TrimSpace(pending[:idx])
				pending = pending[idx+1:]
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if payload == "" || payload == "[DONE]" {
					continue
				}
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content          string `json:"content"`
							ReasoningContent string `json:"reasoning_content"`
							ToolCalls []struct {
								Index    *int   `json:"index"`
								ID       string `json:"id"`
								Type     string `json:"type"`
								Function struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								} `json:"function"`
							} `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
					Usage json.RawMessage `json:"usage"`
				}
				if json.Unmarshal([]byte(payload), &chunk) != nil {
					continue
				}
				if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
					usage = parseUsage(chunk.Usage, usage)
				}
				for _, c := range chunk.Choices {
					for _, tc := range c.Delta.ToolCalls {
						index := 0
						if tc.Index != nil {
							index = *tc.Index
						}
						for len(toolCalls) <= index {
							toolCalls = append(toolCalls, inference.ToolCall{Type: "function"})
						}
						if tc.ID != "" {
							toolCalls[index].ID = tc.ID
						}
						if tc.Function.Name != "" {
							toolCalls[index].Function.Name += tc.Function.Name
						}
						toolCalls[index].Function.Arguments += tc.Function.Arguments
						if onTool != nil {
							if err := onTool(inference.ToolCallDelta{
								Index:     index,
								ID:        tc.ID,
								Name:      tc.Function.Name,
								Arguments: tc.Function.Arguments,
							}); err != nil {
								return text.String(), events, usage, toolCalls, err
							}
						}
						events++
					}
					if c.Delta.ReasoningContent != "" {
						if onReasoning != nil {
							if err := onReasoning(c.Delta.ReasoningContent); err != nil {
								return text.String(), events, usage, toolCalls, err
							}
						}
						events++
					}
					if c.Delta.Content == "" {
						continue
					}
					if err := emit(c.Delta.Content); err != nil {
						return text.String(), events, usage, toolCalls, err
					}
					text.WriteString(c.Delta.Content)
					events++
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return text.String(), events, usage, toolCalls, err
		}
	}
	// A turn that asked for tools has no prose by design. Treating that as an
	// empty answer made every agent loop die after its first tool request.
	if text.Len() == 0 && len(toolCalls) == 0 {
		return "", events, usage, toolCalls, &emptyAnswerError{}
	}
	return text.String(), events, usage, toolCalls, nil
}

// streamUsage carries the upstream-reported token counts. A field is unknown
// until a chunk reports it; zero is a real value ("none cached"), so the
// presence flags decide what may be reported.
type streamUsage struct {
	Prompt, Completion, Cached int
	PromptSeen, CompletionSeen bool
	CachedSeen                 bool
}

// parseUsage merges one chunk's usage object into the accumulated counts.
// Upstream sends it once, in the final chunk, but merging makes a repeated or
// split usage harmless.
func parseUsage(raw json.RawMessage, acc streamUsage) streamUsage {
	var u struct {
		PromptTokens        *int `json:"prompt_tokens"`
		CompletionTokens    *int `json:"completion_tokens"`
		PromptCacheHitTokens *int `json:"prompt_cache_hit_tokens"`
		PromptTokensDetails *struct {
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

// RefreshToken exchanges a refresh token for a fresh access token. It mirrors
// the desktop gateway's renewal and is used by the panel's "reconnect" action.
func RefreshToken(ctx context.Context, refreshToken string) (access, refresh string, err error) {
	body, _ := json.Marshal(map[string]any{"refresh_token": refreshToken, "source_id": "autoclaw"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiBase+refreshPt, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	setHeaders(req, "", "")
	req.Header.Del("X-Authorization")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Code != 0 || out.Data.AccessToken == "" {
		return "", "", fmt.Errorf("autoclaw refresh failed: %d %s", out.Code, out.Msg)
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
