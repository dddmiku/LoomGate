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
var Models = []inference.Model{
	{ID: "autoclaw:zai_glm-5.3-flash", Name: "GLM-5.3 Flash", ContextWindow: 200000, Incremental: true, MultiTurn: true, Credits: "1x"},
	{ID: "autoclaw:zai_auto", Name: "GLM Auto", ContextWindow: 200000, Incremental: true, MultiTurn: true, Credits: "1x"},
	{ID: "autoclaw:zaicoding_glm-5.3", Name: "GLM-5.3 Coding", ContextWindow: 200000, Incremental: true, MultiTurn: true, Credits: "1x"},
	{ID: "autoclaw:zai_glm-5.3-flashx-legal", Name: "GLM-5.3 FlashX", ContextWindow: 200000, Incremental: true, MultiTurn: true, Credits: "1x"},
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
	return p.complete(ctx, q.Model, q.Messages, emit, q.AccountID)
}

// complete runs one turn against the account pool. It rotates accounts only on
// failures that are plausibly account-scoped (auth, quota); a client error is
// returned as-is because another account cannot fix it.
func (p *Provider) complete(ctx context.Context, model string, messages []inference.Message, emit func(string) error, expectedAccount string) (inference.Result, error) {
	if len(messages) == 0 {
		return inference.Result{}, errors.New("autoclaw_messages_required")
	}
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
		return p.turn(ctx, byID[id], model, messages, emit)
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

func (p *Provider) turn(ctx context.Context, row accountvault.Account, model string, messages []inference.Message, emit func(string) error) (inference.Result, error) {
	s, err := p.session(row.ID)
	if err != nil {
		return inference.Result{}, err
	}
	body, err := buildBody(model, messages, emit != nil)
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
	if emit == nil {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return result, err
		}
		text, usage, err := aggregate(raw)
		if err != nil {
			return result, err
		}
		result.Text = text
		result.UsageReported = usage
		result.Complete = true
		result.Acknowledged = true
		result.Events = 1
		return result, nil
	}

	text, events, err := streamSSE(resp.Body, emit)
	result.Text = text
	result.Events = events
	result.Attempted = true
	if err != nil {
		return result, err
	}
	result.Complete = true
	result.Acknowledged = true
	return result, nil
}

// buildBody renders the OpenAI-compatible request. `store:false` is explicit:
// the upstream is a stateless completion endpoint and nothing may be retained.
func buildBody(model string, messages []inference.Message, stream bool) ([]byte, error) {
	msgs := make([]map[string]any, 0, len(messages)+1)
	hasPrefix := false
	for _, m := range messages {
		if m.Role == "system" && strings.Contains(m.Content, promptPrefix) {
			hasPrefix = true
			break
		}
	}
	if !hasPrefix {
		msgs = append(msgs, map[string]any{"role": "system", "content": promptPrefix + "."})
	}
	for _, m := range messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": m.Content})
	}
	payload := map[string]any{
		"model":                 within(model),
		"messages":              msgs,
		"stream":                stream,
		"store":                 false,
		"max_completion_tokens": 4096,
	}
	return json.Marshal(payload)
}

func setHeaders(req *http.Request, accessToken, model string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "node")
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
	req.Header.Set("x-tm", "win")
	req.Header.Set("x-version", version)
	req.Header.Set("accept-encoding", "identity")
	setSign(req)
}

// aggregate folds an SSE body into plain text, for non-streaming callers.
func aggregate(raw []byte) (string, bool, error) {
	var text strings.Builder
	usage := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
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
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			text.WriteString(c.Delta.Content)
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage = true
		}
	}
	if text.Len() == 0 {
		return "", usage, &emptyAnswerError{}
	}
	return text.String(), usage, nil
}

// streamSSE forwards each text delta as it arrives and returns the joined text.
func streamSSE(body io.Reader, emit func(string) error) (string, int, error) {
	dec := json.NewDecoder(body)
	_ = dec
	var text strings.Builder
	events := 0
	// Read line by line: SSE frames are newline delimited and deltas must be
	// forwarded immediately, not buffered until the stream ends.
	buf := make([]byte, 0, 32<<10)
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
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(payload), &chunk) != nil {
					continue
				}
				for _, c := range chunk.Choices {
					if c.Delta.Content == "" {
						continue
					}
					if err := emit(c.Delta.Content); err != nil {
						return text.String(), events, err
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
			return text.String(), events, err
		}
	}
	_ = buf
	if text.Len() == 0 {
		return "", events, &emptyAnswerError{}
	}
	return text.String(), events, nil
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
