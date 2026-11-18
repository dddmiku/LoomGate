// Pure-HTTP Doubao completion: the same path the official desktop client uses,
// but driven from a captured request template instead of the client's JS runtime.
//
// Verified against the running official client (2.36.8):
//   - /alice/slot/action_bar_v3/brief_list and /chat/completion authorize on the
//     session cookies alone; a_bogus / msToken are not enforced (six control
//     probes: signed, unsigned, stripped-one, altered body, replaced body and
//     altered query all returned the same business result).
//   - /chat/completion replies with text/event-stream and the same event names
//     the stream package already decodes.
//
// The captured template is stored encrypted inside the account profile. Only the
// volatile identity fields (uuids, timestamps, prompt, model) are rewritten.
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

	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/doubao/stream"
)

// HTTPTextSession owns the request body for one completion. It is private and
// must never be logged or persisted outside the encrypted vault.
type HTTPTextSession struct {
	session    Session
	model      string
	prompt     string
	conversation string
	Body       json.RawMessage

	mu        sync.Mutex
	submitted bool
	closed    bool
	Attempts  []inference.Attempt
}

func (*HTTPTextSession) String() string   { return "doubao.HTTPTextSession[redacted]" }
func (*HTTPTextSession) GoString() string { return "doubao.HTTPTextSession[redacted]" }

// Submitted reports whether bytes actually reached upstream. Anything else is
// a pre-submission failure and must not be journaled as an attempt.
func (s *HTTPTextSession) Submitted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submitted
}

func (s *HTTPTextSession) VerifyIdentity(ctx context.Context) error {
	return nil
}

func (s *HTTPTextSession) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	for i := range s.Body {
		s.Body[i] = 0
	}
	return nil
}

func (s *HTTPTextSession) ObserveAttempt(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Attempts) == 0 {
		return
	}
	record := &s.Attempts[len(s.Attempts)-1]
	if !record.FinishedAt.IsZero() {
		return
	}
	record.FinishedAt = time.Now()
	if err != nil {
		record.ErrorCode = "provider_stream_failed"
		var safe *APIError
		if errors.As(err, &safe) {
			record.ErrorCode = safe.Kind
		}
		if errors.Is(err, context.Canceled) {
			record.ErrorCode = "client_canceled"
		}
	}
}

// Open is the stream.Opener implementation. Doubao text answers are a single
// completion SSE; async handoffs need the official signer and are rejected here
// rather than silently attempted without one.
func (s *HTTPTextSession) Open(ctx context.Context, req stream.Request) (io.ReadCloser, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, errors.New("desktop_session_closed")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if req.Path != "/chat/completion" {
		return nil, stream.NoResume(runtimeError("desktop_operation_unsupported"))
	}
	query := cloneValues(s.session.Query)
	query.Del("a_bogus")
	query.Del("msToken")
	u := url.URL{Scheme: "https", Host: "www.doubao.com", Path: req.Path, RawQuery: query.Encode()}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(req.Body))
	if err != nil {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	httpReq.Header = s.session.Headers.Clone()
	if httpReq.Header == nil {
		httpReq.Header = http.Header{}
	}
	for _, key := range []string{"Content-Length", "Host", "Connection", "Accept-Encoding", "Cookie"} {
		httpReq.Header.Del(key)
	}
	if cookie := s.session.Headers.Get("Cookie"); cookie != "" {
		httpReq.Header.Set("Cookie", cookie)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if httpReq.Header.Get("Referer") == "" {
		httpReq.Header.Set("Referer", "https://www.doubao.com/chat/")
	}
	if httpReq.Header.Get("agw-js-conv") == "" {
		httpReq.Header.Set("agw-js-conv", "str")
	}
	httpReq.Header.Set("x-request-id", newUUID())

	client := &http.Client{Timeout: 0, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(httpReq)
	// From here on the server may have accepted the turn: never report a clean
	// pre-submission failure after Do returns.
	s.mu.Lock()
	s.submitted = true
	s.Attempts = append(s.Attempts, inference.Attempt{StartedAt: time.Now(), HTTPStatus: responseStatusCode(response)})
	s.mu.Unlock()
	if err != nil {
		if canceled := contextFailure(ctx, err); canceled != nil {
			return nil, canceled
		}
		return nil, stream.NoResume(runtimeError("desktop_submission_uncertain"))
	}
	if response.StatusCode != 200 || !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body.Close()
		return nil, stream.NoResume(runtimeError("desktop_upstream_rejected"))
	}
	return response.Body, nil
}

func responseStatusCode(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}

// EffortLevels is the official reasoning-effort ladder. The wire value is an
// integer 3..7; the desktop UI shows it as 低/中/高/极高/最高. The captured
// template stores the same number in THREE places with DIFFERENT JSON types:
//
//	option.conversation_init_ext.reasoning_effort  string
//	option.model_config.reasoning_effort           int
//	option.aggregate_params.reasoning_effort       string
//
// All three must be written together; changing one alone is silently ignored by
// the client downstream.
const (
	EffortLow      = 3
	EffortMedium   = 4
	EffortHigh     = 5
	EffortVeryHigh = 6
	EffortMax      = 7
)

// ParseEffort maps a public effort token to its wire integer. Unknown tokens
// return ok=false so callers can fall back to the captured template value
// instead of silently downgrading the user's choice.
func ParseEffort(token string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "low", "minimal", "低", "3":
		return EffortLow, true
	case "medium", "mid", "中", "4":
		return EffortMedium, true
	case "high", "高", "5":
		return EffortHigh, true
	case "very_high", "veryhigh", "xhigh", "极高", "6":
		return EffortVeryHigh, true
	case "max", "highest", "最高", "7":
		return EffortMax, true
	default:
		return 0, false
	}
}

// ClampEffort keeps an effort inside a model's advertised ladder. Models publish
// non-contiguous sets (3/5/7 is common), so a requested level that does not exist
// must be DOWN-GRADED to the highest published level at or below it — never
// rounded up and never rejected.
//
// This mirrors gateway/internal/upstream.normalizeReasoningEffort so the
// reverse-proxy path and the legacy upstream path make the same decision:
//   - requested level supported                -> unchanged
//   - requested level missing, a lower one exists -> highest supported <= requested
//   - every supported level is above the request -> lowest supported (smallest deviation)
func ClampEffort(value int, allowed []int) int {
	if len(allowed) == 0 {
		return value
	}
	best, found := 0, false
	for _, level := range allowed {
		if level <= value && (!found || level > best) {
			best, found = level, true
		}
	}
	if found {
		return best
	}
	// Nothing at or below the request: fall back to the lowest published level.
	lowest := allowed[0]
	for _, level := range allowed[1:] {
		if level < lowest {
			lowest = level
		}
	}
	return lowest
}

// writeEffort sets the three wire fields with their original JSON types.
func writeEffort(option map[string]any, level int) {
	if node, ok := option["conversation_init_ext"].(map[string]any); ok {
		if _, exists := node["reasoning_effort"]; exists {
			node["reasoning_effort"] = strconv.Itoa(level)
		}
	}
	if node, ok := option["model_config"].(map[string]any); ok {
		if _, exists := node["reasoning_effort"]; exists {
			node["reasoning_effort"] = level
		}
	}
	if node, ok := option["aggregate_params"].(map[string]any); ok {
		if _, exists := node["reasoning_effort"]; exists {
			node["reasoning_effort"] = strconv.Itoa(level)
		}
	}
}

// buildCompletionBody rewrites only the volatile fields of the captured
// template, leaving every official protocol field intact.
func buildCompletionBody(template json.RawMessage, model, prompt, conversation string) (json.RawMessage, error) {
	return buildCompletionBodyWithEffort(template, model, prompt, conversation, 0, false)
}

// buildCompletionBodyWithEffort is buildCompletionBody plus an explicit reasoning
// effort. When withEffort is false the captured template value is preserved.
func buildCompletionBodyWithEffort(template json.RawMessage, model, prompt, conversation string, effort int, withEffort bool) (json.RawMessage, error) {
	return buildCompletionBodyFor(template, "", model, prompt, conversation, effort, withEffort)
}

// BodyProfile carries the catalog facts that the official client copies into
// option.model_config.model_extra_params. The service reads the window size from
// the request, not from the model_item_key alone, so an empty map makes a
// work-task request look truncated.
type BodyProfile struct {
	ContextWindow int64
	MemoryProfile string
}

// buildCompletionBodyFor stamps the account's own bot id into the template.
// The template ships with the official client's default assistant id, but the
// per-account entry must win: catalog and completion are both scoped to it.
func buildCompletionBodyFor(template json.RawMessage, botID, model, prompt, conversation string, effort int, withEffort bool, profiles ...BodyProfile) (json.RawMessage, error) {
	if len(template) == 0 || len(template) > 1<<20 || len(prompt) == 0 || len(prompt) > 65536 {
		return nil, runtimeError("desktop_binding_invalid")
	}
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(template))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	meta, ok := body["client_meta"].(map[string]any)
	if !ok {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	if botID != "" {
		if len(botID) > 32 {
			return nil, runtimeError("desktop_binding_invalid")
		}
		meta["bot_id"] = botID
	}
	if conversation == "" {
		meta["local_conversation_id"] = "local_" + randomDigits(16)
		meta["conversation_id"] = ""
	} else {
		if len(conversation) > 128 || strings.ContainsAny(conversation, "\r\n\x00") {
			return nil, runtimeError("desktop_binding_invalid")
		}
		// A continuing turn addresses the existing conversation by its server id.
		// The official client drops local_conversation_id and sends the section
		// cursor fields instead; keeping a fresh local id here is what made the
		// service treat the call as a new conversation.
		meta["conversation_id"] = conversation
		delete(meta, "local_conversation_id")
		meta["last_section_id"] = ""
		meta["last_message_index"] = nil
		meta["local_permissions"] = []any{}
	}

	rows, ok := body["messages"].([]any)
	if !ok || len(rows) != 1 {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	message, ok := rows[0].(map[string]any)
	if !ok {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	message["local_message_id"] = newUUID()
	blocks, ok := message["content_block"].([]any)
	if !ok || len(blocks) != 1 {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	block, ok := blocks[0].(map[string]any)
	if !ok {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	block["block_id"] = newUUID()
	content, ok := block["content"].(map[string]any)
	if !ok {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	textBlock, ok := content["text_block"].(map[string]any)
	if !ok {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	textBlock["text"] = prompt

	modeID, itemKey, err := splitModel(model)
	if err != nil {
		return nil, err
	}
	option, ok := body["option"].(map[string]any)
	if !ok {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	option["unique_key"] = newUUID()
	now := time.Now()
	option["create_time_ms"] = now.UnixMilli()
	option["need_create_conversation"] = conversation == ""
	setString(option, []string{"model_config", "model_item_key"}, itemKey)
	setString(option, []string{"aggregate_params", "model_item_key"}, itemKey)
	setString(option, []string{"aggregate_params", "mode_id"}, modeID)
	setString(option, []string{"conversation_init_ext", "model_item_key"}, itemKey)
	setString(option, []string{"conversation_init_ext", "mode_id"}, modeID)
	// mode_id "3" is the 工作 (work task) mode. The official client switches the
	// whole request shape for it: conversation_mode 2, no agent_mode (that field
	// belongs to the 对话 assistant), a general_task_param that declares a local
	// runtime, and an ext block with only the three work keys. Keeping the 对话
	// shape while naming a work model makes the service fall back to its free
	// default model and open a fresh conversation - which is exactly what the
	// desktop client showed.
	if isWorkMode(modeID) {
		applyWorkMode(option, body, itemKey, profiles)
	}
	if withEffort {
		writeEffort(option, effort)
	}
	if recovery, ok := option["recovery_option"].(map[string]any); ok {
		recovery["is_recovery"] = false
		recovery["req_create_time_sec"] = now.Unix()
	}
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > 1<<20 {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	return raw, nil
}

func setString(node map[string]any, path []string, value string) {
	for i := 0; i < len(path)-1; i++ {
		next, ok := node[path[i]].(map[string]any)
		if !ok {
			return
		}
		node = next
	}
	if _, exists := node[path[len(path)-1]]; exists {
		node[path[len(path)-1]] = value
	}
}

// isWorkMode reports whether the catalog mode is 工作 (work task). Mode "1" is
// 对话 and must keep the captured chat shape; mode "3" is a different protocol
// branch on the service side.
func isWorkMode(modeID string) bool { return modeID == "3" }

// workGeneralTaskParam is the exact object the official client sends when a
// work-mode conversation runs on the service side. The client picks the runtime
// plane with:
//
//	o = ready ? RuntimeType_Local_PC : RuntimeType_Cloud_VM
//
// and for Cloud_VM it returns `{runtime_type: 1}` — a single field. The
// agent_task_param block belongs to the Local_PC branch only, where it carries
// the desktop device identity. Mixing the two (cloud runtime plus an
// agent_task_param saying local) makes the service accept the turn but wait for a
// runtime that never answers: the stream then emits thousands of loading/heartbeat
// events with zero answer text.
func workGeneralTaskParam() map[string]any {
	return map[string]any{"runtime_type": 1}
}

// applyWorkMode rewrites the captured 对话 body into the 工作 body the official
// client sends. Only the shape changes; the prompt, bot id and model identity
// come from the caller.
//
// The captured template is a 对话 request: option.conversation_mode=1,
// option.agent_mode=2, a chat general_task_param and a chat ext block. Reusing
// that shape while naming a work-only model makes the service fall back to the
// free default model and open a separate conversation - the exact symptom seen
// in the desktop client.
func applyWorkMode(option, body map[string]any, itemKey string, profiles []BodyProfile) {
	option["conversation_mode"] = 2
	option["support_lazy_fetch_stream"] = false
	option["is_old_user"] = false
	option["message_storage_type"] = 0
	option["general_task_param"] = workGeneralTaskParam()
	// agent_mode belongs to the 对话 assistant. Work requests omit it entirely.
	delete(option, "agent_mode")
	// Work requests carry no conversation_init_option; the init data lives in
	// conversation_init_ext only.
	delete(option, "conversation_init_option")

	if aggregate, ok := option["aggregate_params"].(map[string]any); ok {
		aggregate["conversation_mode"] = "2"
		aggregate["agent_mode"] = ""
		delete(aggregate, "mention_skill_list")
		delete(aggregate, "mention_plugin_list")
		delete(aggregate, "mention_ext")
	}
	if config, ok := option["model_config"].(map[string]any); ok {
		extra := map[string]any{}
		if len(profiles) > 0 {
			if profiles[0].ContextWindow > 0 {
				extra["total_window_size"] = strconv.FormatInt(profiles[0].ContextWindow, 10)
			}
			extra["memory_profile"] = profiles[0].MemoryProfile
		}
		config["model_extra_params"] = extra
	}
	// The official work request's ext block has exactly these three keys. The
	// chat-only keys (agent_mode, use_deep_think, brief, bot_id, ...) are not
	// part of it, and leaving them in is visible to the service.
	body["ext"] = map[string]any{
		"general_task_param":              mustJSONString(workGeneralTaskParam()),
		"sub_conv_firstmet_type":          "1",
		"commerce_credit_config_enable":   "0",
	}
}

// mustJSONString renders a small object as compact JSON. The wire field is a
// string containing JSON, not a nested object.
func mustJSONString(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// splitModel turns "doubao:<mode>:<model_item_key>" into its two wire values.
func splitModel(model string) (string, string, error) {
	if !strings.HasPrefix(model, "doubao:") || len(model) > 256 {
		return "", "", runtimeError("desktop_binding_invalid")
	}
	rest := strings.TrimPrefix(model, "doubao:")
	mode, key, found := strings.Cut(rest, ":")
	if !found || mode == "" || key == "" || len(key) > 64 {
		return "", "", runtimeError("desktop_binding_invalid")
	}
	return mode, key, nil
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

// fetchSessionProfile reads the encrypted profile for one account without
// requiring a desktop runtime.
func (c *Client) fetchSessionProfile(ctx context.Context, account string) (Session, error) {
	if c.sessions == nil {
		return Session{}, runtimeError("desktop_unavailable")
	}
	return c.sessions.Session(ctx, account)
}

var _ = strconv.Itoa
