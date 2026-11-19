package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/requestlog"
)

func providerErrorCode(err error) string {
	if errors.Is(err, inference.ErrStreamRevised) {
		return "provider_stream_revised"
	}
	if errors.Is(err, inference.ErrStreamContract) {
		return "provider_stream_contract_invalid"
	}
	var coded interface{ PublicCode() string }
	if errors.As(err, &coded) {
		switch code := coded.PublicCode(); code {
		case "desktop_unavailable", "desktop_login_required", "desktop_identity_mismatch", "desktop_model_unavailable", "desktop_busy", "desktop_timeout", "desktop_submission_uncertain", "desktop_native_tools_unsupported", "desktop_ownership_lost", "desktop_cleanup_uncertain", "desktop_response_rejected", "desktop_version_unsupported", "desktop_protocol_invalid", "desktop_canceled":
			return code
		case "provider_context_unsupported", "provider_conversation_invalid", "provider_conversation_unavailable", "provider_conversation_ambiguous", "provider_context_mismatch", "provider_conversation_capacity", "provider_conversation_account_changed", "provider_turn_uncertain", "provider_conversation_limit", "provider_conversation_save_uncertain":
			return code
		case "provider_idempotency_invalid", "provider_idempotency_required", "provider_idempotency_conflict", "provider_request_already_processed":
			return code
		}
	}
	return "provider_request_failed"
}
func providerModelsSafely(ctx context.Context, p inference.Provider) (rows []inference.Model, err error) {
	defer func() {
		if recover() != nil {
			rows = nil
			err = errors.New("provider_panicked")
		}
	}()
	return p.Models(ctx)
}
func providerCompleteSafely(ctx context.Context, p inference.Provider, model, text string) (result inference.Result, err error) {
	defer func() {
		if recover() != nil {
			result = inference.Result{Attempted: true}
			err = errors.New("provider_panicked")
		}
	}()
	return p.Complete(ctx, model, text)
}
func (h *Handler) providerModels(ctx context.Context) ([]map[string]any, []map[string]string) {
	rows := []map[string]any{}
	issues := []map[string]string{}
	ids := []string{}
	for id := range h.cfg.Inference {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		read, cancel := context.WithTimeout(ctx, 8*time.Second)
		models, err := providerModelsSafely(read, h.cfg.Inference[id])
		cancel()
		if err != nil {
			issues = append(issues, map[string]string{"provider": id, "code": providerErrorCode(err)})
			continue
		}
		for _, m := range models {
			entry := map[string]any{"id": m.ID, "object": "model", "created": 0, "owned_by": id, "name": m.Name, "supports_tool_call": false, "supports_images": false, "single_turn_text_only": !m.MultiTurn, "supports_multi_turn": m.MultiTurn}
			entry["streaming_mode"] = "buffered"
			if m.Incremental {
				entry["streaming_mode"] = "incremental"
			}
			if m.ContextWindow > 0 {
				entry["context_length"] = m.ContextWindow
			}
			if m.Credits != "" {
				entry["credits"] = m.Credits
			}
			rows = append(rows, entry)
		}
	}
	return rows, issues
}

// This provider is text-only: it can carry one ordinary text conversation and
// nothing else. Fields the official wire format genuinely has no place for are
// accepted and ignored rather than rejected, because every mainstream OpenAI
// client sends at least one of them on every request; rejecting turned the whole
// provider into a permanent 400 for those clients. The model is never told the
// tools exist, and /v1/models reports supports_tool_call=false, so an ignored
// tools block degrades to a plain text answer instead of a false capability.
func (h *Handler) providerChat(w http.ResponseWriter, r *http.Request, body []byte, model string, streaming bool, st *chatStat, provider inference.Provider) {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	// Every request has to be read for the handful of fields this provider can
	// actually carry. Unknown or unsupported fields are IGNORED, never rejected:
	// OpenAI-compatible clients send an open-ended set (tools, sampling knobs,
	// reasoning switches, provider-specific extensions), and rejecting any one of
	// them turned a reachable model into a permanent 400 for that client. The
	// fields below are read because they change what is sent; everything else is
	// simply not read.
	if len(body) > 8<<20 {
		st.status = 413
		writeOpenAIError(w, 413, "request_body_too_large", "request body exceeds provider limit")
		return
	}
	var text string
	// Reasoning effort is optional metadata, not a hard capability. Parse it
	// before dispatch so an effort-aware provider can apply it and a plain one
	// can ignore it without failing the whole request.
	effort := ""
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		if raw := fields[key]; raw != nil {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				effort = strings.TrimSpace(value)
			}
			break
		}
	}
	dialogueProvider, hasDialogue := provider.(inference.DialogueProvider)
	var messages []struct {
		Role       string            `json:"role"`
		Content    json.RawMessage   `json:"content"`
		ToolCalls  []json.RawMessage `json:"tool_calls"`
		ToolCallID string            `json:"tool_call_id"`
	}
	if json.Unmarshal(fields["messages"], &messages) != nil {
		st.status = 400
		writeOpenAIError(w, 400, "provider_input_unsupported", "messages must be an array of role and content objects")
		return
	}
	// A provider that can carry tools must receive the transcript verbatim: an
	// assistant turn that asked for a tool and the "tool" turn answering it are
	// only meaningful with their ids and call structure intact. Flattening them
	// to text (the default below) silently turns a tool loop into one paragraph,
	// which is why an agent client saw a single sentence and then nothing.
	_, carriesTools := provider.(inference.ToolProvider)
	carriesTools = carriesTools && len(fields["tools"]) > 0
	// A system/developer role has no separate wire field, so its text is prepended
	// to the first user turn. The instruction still reaches the model; only the
	// role boundary is flattened. Rejecting it would make the provider unusable
	// from any client that always sends a system prompt.
	systemPrefix := ""
	turns := make([]struct {
		Role    string
		Content json.RawMessage
	}, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" || m.Role == "developer" {
			if inline := textOfContent(m.Content); strings.TrimSpace(inline) != "" {
				systemPrefix += inline + "\n\n"
			}
			continue
		}
		turns = append(turns, struct {
			Role    string
			Content json.RawMessage
		}{m.Role, m.Content})
	}
	// Any role sequence is accepted. The transcript is flattened into a single
	// prompt by the provider, so a client that sends two user turns in a row, a
	// trailing assistant prefill or an assistant-first history still works. Only
	// empty turns and size limits are enforced.
	parsed := []inference.Message{}
	total := 0
	text = ""
	if carriesTools {
		// Tool-aware path: keep roles, order, call ids and call structure.
		// Empty text is legal on an assistant turn that only requests tools and
		// on a tool turn, so the text emptiness check does not apply here.
		for _, m := range messages {
			value := textOfContent(m.Content)
			total += len(value)
			if total > 1<<20 {
				break
			}
			msg := inference.Message{Role: m.Role, Content: value, ToolCallID: m.ToolCallID}
			for _, rawCall := range m.ToolCalls {
				var call inference.ToolCall
				if json.Unmarshal(rawCall, &call) == nil {
					if call.Type == "" {
						call.Type = "function"
					}
					msg.ToolCalls = append(msg.ToolCalls, call)
				}
			}
			parsed = append(parsed, msg)
		}
	} else {
		for i, m := range turns {
			value := textOfContent(m.Content)
			if strings.TrimSpace(value) == "" {
				continue
			}
			total += len(value)
			if total > 1<<20 {
				break
			}
			if i == 0 && systemPrefix != "" {
				value = systemPrefix + value
			}
			parsed = append(parsed, inference.Message{Role: m.Role, Content: value})
			text = value
		}
	}
	if len(parsed) == 0 || total > 1<<20 || len(parsed) > 101 || (!hasDialogue && !carriesTools && len(parsed) != 1) {
		st.status = 400
		writeOpenAIError(w, 400, "provider_input_unsupported", "the request carried no readable text, or exceeded the size limit")
		return
	}
	if !carriesTools && text == "" {
		st.status = 400
		writeOpenAIError(w, 400, "provider_input_unsupported", "the request carried no readable text, or exceeded the size limit")
		return
	}
	var result inference.Result
	var err error
	var sink *providerStreamSink
	if hasDialogue {
		owner := fmt.Sprintf("%x", sha256.Sum256([]byte(r.Header.Get("Authorization"))))
		if info, ok := requestKeyInfo(r); ok {
			owner = fmt.Sprintf("%x", sha256.Sum256([]byte("api-key-id:"+info.ID)))
		} else {
			credential := r.Header.Get("Authorization")
			if credential == "" {
				credential = r.Header.Get("X-Api-Key")
			}
			if credential == "" {
				credential = r.Header.Get("X-Goog-Api-Key")
			}
			if credential == "" {
				credential = r.URL.Query().Get("key")
			}
			owner = fmt.Sprintf("%x", sha256.Sum256([]byte("credential:"+credential)))
		}
		q := inference.Dialogue{Model: model, Owner: owner, ConversationRef: r.Header.Get("X-LoomGate-Conversation"), Messages: parsed, IdempotencyKey: r.Header.Get("Idempotency-Key")}
		if carriesTools {
			var defs []inference.ToolDefinition
			var rawTools []inference.ToolDefinition
			if json.Unmarshal(fields["tools"], &rawTools) == nil {
				defs = rawTools
			}
			q.Tools = defs
			if raw := fields["tool_choice"]; len(raw) > 0 {
				var choice any
				if json.Unmarshal(raw, &choice) == nil {
					q.ToolChoice = choice
				}
			}
		}
		var emit func(string) error
		if streaming {
			sink = newProviderStreamSink(w, r, model, st)
			emit = sink.emit
		}
		// Prefer the reasoning-aware path: a thinking model streams its
		// reasoning first, and forwarding it is what keeps the client from
		// sitting blank through the whole think phase. Fall back to the
		// effort-aware path (reasoning_effort passthrough) when the provider
		// does not expose reasoning deltas.
		if toolCapable, ok := provider.(inference.ToolCapableProvider); ok && carriesTools && emit != nil {
			// Tool turns stream tool_call fragments, not prose; the sink emits
			// them as OpenAI tool_calls deltas so an agent client can continue
			// its loop instead of stopping after one sentence.
			result, err = providerStreamToolsSafely(r.Context(), toolCapable, q, sink.emitToolDelta)
		} else if reasoning, ok := provider.(inference.ReasoningProvider); ok && emit != nil {
			result, err = providerStreamReasoningSafely(r.Context(), reasoning, q, emit, sink.emitReasoning)
		} else if aware, ok := effortAware(provider); ok && effort != "" {
			result, err = providerEffortDialogueSafely(r.Context(), aware, q, effort, emit)
		} else {
			result, err = providerDialogueSafely(r.Context(), dialogueProvider, q, emit)
		}
		if err == nil && sink != nil && result.Complete && result.Acknowledged {
			err = sink.validate(result)
		}
	} else if progressive, ok := provider.(inference.StreamingProvider); streaming && ok {
		sink = newProviderStreamSink(w, r, model, st)
		// A provider whose model thinks before answering can stream those
		// deltas too. Without this the client sits silent for the whole
		// thinking phase, which is what turned a 3-second think into a
		// two-minute "hang" in the request log.
		if aware, ok := effortAware(provider); ok && effort != "" {
			result, err = providerStreamSafelyWithEffort(r.Context(), aware, model, text, effort, sink.emit)
		} else {
			result, err = providerStreamSafely(r.Context(), progressive, model, text, sink.emit)
		}
		if err == nil && result.Complete && result.Acknowledged {
			err = sink.validate(result)
		}
	} else {
		// Buffered (non-streaming) completion. An effort-aware provider still
		// receives the caller's level; providers without effort support take the
		// ordinary path unchanged.
		if aware, ok := effortAware(provider); ok && effort != "" {
			result, err = providerEffortCompleteSafely(r.Context(), aware, model, text, effort)
		} else {
			result, err = providerCompleteSafely(r.Context(), provider, model, text)
		}
	}
	if result.ConversationRef != "" {
		w.Header().Set("X-LoomGate-Conversation", result.ConversationRef)
	}
	st.upstreamStarted = result.Attempted
	st.uid = result.AccountRef
	st.unreported = !result.UsageReported
	// Carry the upstream's own counts into the request log. Without this the
	// provider path recorded only "usage was reported" and every row showed a
	// dash for input/output/cached, even when the upstream had sent exact
	// numbers.
	if result.UsageReported {
		st.prompt = result.PromptTokens
		st.toks = result.CompletionTokens
		if result.CachedTokens > 0 {
			st.cached = result.CachedTokens
		}
	} else {
		st.prompt, st.toks, st.cached = 0, -1, -1
	}
	if st.trace != nil {
		st.trace.account = result.AccountRef
		st.trace.model = model
		st.trace.record.UpstreamStarted = result.Attempted
		st.trace.record.AttemptCount = len(result.AttemptRecords)
		for i, a := range result.AttemptRecords {
			status := requestlog.StatusError
			if a.HTTPStatus == 200 && a.ErrorCode == "" {
				status = requestlog.StatusSuccess
			}
			if a.ErrorCode == "client_canceled" {
				status = requestlog.StatusCanceled
			}
			st.trace.record.Attempts = append(st.trace.record.Attempts, requestlog.Attempt{Number: i + 1, AccountID: result.AccountRef, Model: model, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, DurationMS: max(0, a.FinishedAt.Sub(a.StartedAt).Milliseconds()), HTTPStatus: a.HTTPStatus, Status: status, ErrorCode: traceCode(a.ErrorCode), UsageState: requestlog.UsageMissing, UsageReason: "usage_not_reported"})
		}
		if len(st.trace.record.Attempts) > 0 {
			st.trace.last = &st.trace.record.Attempts[len(st.trace.record.Attempts)-1]
		}
	}
	if err != nil || !result.Complete || !result.Acknowledged {
		st.status = 502
		// One structured line naming which contract term failed. Without it a 502
		// is indistinguishable between "stream broke", "no ACK" and "empty text",
		// and the client only ever sees the generic wrapper.
		log.Printf("WARN: [provider] 502 模型=%s 协议=%s err=%v complete=%v acknowledged=%v events=%d attempts=%d chars=%d",
			model, protocolNameOfProvider(), err, result.Complete, result.Acknowledged, result.Events, result.Attempts, len(result.Text))
		code := providerErrorCode(err)
		if strings.HasPrefix(code, "provider_context_") || code == "provider_conversation_invalid" {
			st.status = 400
		}
		if code == "provider_idempotency_invalid" || code == "provider_idempotency_required" {
			st.status = 400
		}
		if code == "provider_idempotency_conflict" || code == "provider_request_already_processed" {
			st.status = 409
		}
		if code == "provider_turn_uncertain" || code == "provider_conversation_account_changed" || code == "provider_conversation_ambiguous" {
			st.status = 409
		}
		if errors.Is(err, context.Canceled) {
			st.status = 499
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			st.status = 504
			code = "desktop_timeout"
		}
		if st.trace != nil {
			st.trace.record.ErrorCode = code
		}
		if sink != nil && sink.started {
			sink.fail(code)
			return
		}
		writeOpenAIError(w, st.status, code, "provider processing did not finish cleanly; inspect status before resubmitting")
		return
	}
	if sink != nil {
		err = sink.finish(result)
		st.failed = err != nil
		st.status = 200
		if err != nil {
			st.status = 502
		}
		return
	}
	id := "chatcmpl-" + requestID(r)
	created := time.Now().Unix()
	if result.ConversationRef != "" {
		w.Header().Set("X-LoomGate-Conversation", result.ConversationRef)
	}
	w.Header().Set("X-Gateway-Usage-Status", "not-reported")
	w.Header().Set("X-Gateway-Upstream-Complete", "true")
	w.Header().Set("X-Gateway-Upstream-Ack", "true")
	w.Header().Set("X-Gateway-Upstream-Request-Digest", result.RequestDigest)
	if result.UsageReported {
		w.Header().Set("X-Gateway-Usage-Status", "reported")
	}
	if !streaming {
		message := map[string]any{"role": "assistant", "content": result.Text}
		finishReason := "stop"
		if len(result.ToolCalls) > 0 {
			finishReason = "tool_calls"
			calls := make([]any, 0, len(result.ToolCalls))
			for _, call := range result.ToolCalls {
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
			message["tool_calls"] = calls
		}
		response := map[string]any{"id": id, "object": "chat.completion", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}}}
		if result.ConversationRef != "" {
			response["loomgate_conversation"] = result.ConversationRef
		}
		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(response)
	} else {
		// Editable upstream blocks may replace/delete prior text. Buffer until
		// the full reply terminates, so append-only OpenAI consumers never see
		// text that the provider later removed. No fabricated early stop frame.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		for _, choice := range []map[string]any{{"index": 0, "delta": map[string]string{"role": "assistant", "content": result.Text}, "finish_reason": nil}, {"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}} {
			var raw []byte
			raw, err = json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{choice}})
			if err != nil {
				break
			}
			_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
			if err != nil {
				break
			}
		}
		if err == nil {
			_, err = fmt.Fprint(w, "data: [DONE]\n\n")
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	st.failed = err != nil
	st.status = 200
	if err != nil {
		st.status = 502
	}
}

// textOfContent extracts plain text from an OpenAI message content value, which
// may be a string or an array of text parts. Unsupported parts contribute
// nothing rather than failing the whole request.
func textOfContent(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		var kind, text string
		if len(part) != 2 || json.Unmarshal(part["type"], &kind) != nil || kind != "text" || json.Unmarshal(part["text"], &text) != nil {
			continue
		}
		b.WriteString(text)
	}
	return b.String()
}

// ignoredToolLogs keeps the "tools ignored" notice to one line per model so a
// chatty client cannot flood the log.
var ignoredToolLogs sync.Map

func logProviderIgnoredTools(model string) {
	if _, loaded := ignoredToolLogs.LoadOrStore(model, struct{}{}); loaded {
		return
	}
	log.Printf("WARN: [provider] 模型 %s 不支持调用方自定义工具；本次请求的 tools 已忽略，按纯文本回答", model)
}

// protocolNameOfProvider is a placeholder kept out of the hot path; the request
// protocol is already available on the stat struct, and plumbing it here would
// touch every caller for a log line.
func protocolNameOfProvider() string { return "" }
