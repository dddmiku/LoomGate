package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
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
			rows = append(rows, entry)
		}
	}
	return rows, issues
}

// This initial provider capability is deliberately explicit: one ordinary text
// message. Unsupported fields are rejected before any upstream submission.
func (h *Handler) providerChat(w http.ResponseWriter, r *http.Request, body []byte, model string, streaming bool, st *chatStat, provider inference.Provider) {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	for key := range fields {
		switch key {
		case "model", "messages", "stream", "stream_options", "n":
		case "tools", "tool_choice", "functions", "function_call", "parallel_tool_calls":
			st.status = 400
			writeOpenAIError(w, 400, "provider_tools_unsupported", "official work tools are distinct from caller-defined functions; caller tools are not supported by this provider")
			return
		case "max_tokens", "max_completion_tokens", "max_output_tokens":
			st.status = 400
			writeOpenAIError(w, 400, "provider_output_budget_unsupported", "the signed official chat request has no verified output-token limit field; the request was not sent upstream")
			return
		default:
			st.status = 400
			writeOpenAIError(w, 400, "provider_parameter_unsupported", "the request contains a parameter that the official provider cannot honor")
			return
		}
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var rawMessages []map[string]json.RawMessage
	_ = json.Unmarshal(fields["messages"], &rawMessages)
	for _, message := range rawMessages {
		for key := range message {
			if key != "role" && key != "content" {
				st.status = 400
				writeOpenAIError(w, 400, "provider_parameter_unsupported", "message contains an unsupported field")
				return
			}
		}
	}
	if len(body) > 8<<20 {
		st.status = 413
		writeOpenAIError(w, 413, "request_body_too_large", "request body exceeds provider limit")
		return
	}
	var text string
	dialogueProvider, hasDialogue := provider.(inference.DialogueProvider)
	validMessages := json.Unmarshal(fields["messages"], &messages) == nil && len(messages) > 0 && len(messages) <= 101 && len(messages)%2 == 1
	parsed := []inference.Message{}
	total := 0
	for i, m := range messages {
		// System instructions have no verified carrier in the signed official
		// chat builder. Reject them explicitly instead of flattening them into
		// user text and pretending the model received a system prompt.
		if m.Role == "system" || m.Role == "developer" {
			st.status = 400
			writeOpenAIError(w, 400, "provider_system_instructions_unsupported", "this provider cannot carry a separate system instruction through the official client; the request was not sent upstream")
			return
		}
		expected := "user"
		if i%2 == 1 {
			expected = "assistant"
		}
		var value string
		contentErr := json.Unmarshal(m.Content, &value)
		if contentErr != nil {
			var parts []map[string]json.RawMessage
			if json.Unmarshal(m.Content, &parts) == nil && len(parts) > 0 {
				value = ""
				contentErr = nil
				for _, part := range parts {
					var kind, text string
					if len(part) != 2 || json.Unmarshal(part["type"], &kind) != nil || kind != "text" || json.Unmarshal(part["text"], &text) != nil {
						contentErr = errors.New("content_not_text")
						break
					}
					value += text
				}
			}
		}
		if m.Role != expected || contentErr != nil || strings.TrimSpace(value) == "" || len(value) > 65536 {
			validMessages = false
		}
		total += len(value)
		parsed = append(parsed, inference.Message{Role: m.Role, Content: value})
		text = value
	}
	if !validMessages || total > 1<<20 || (!hasDialogue && len(messages) != 1) {
		st.status = 400
		writeOpenAIError(w, 400, "provider_input_unsupported", "expected alternating user and assistant text messages ending with user; unsupported roles, content or size")
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
		var emit func(string) error
		if streaming {
			sink = newProviderStreamSink(w, r, model, st)
			emit = sink.emit
		}
		result, err = providerDialogueSafely(r.Context(), dialogueProvider, q, emit)
		if err == nil && sink != nil && result.Complete && result.Acknowledged {
			err = sink.validate(result)
		}
	} else if progressive, ok := provider.(inference.StreamingProvider); streaming && ok {
		sink = newProviderStreamSink(w, r, model, st)
		result, err = providerStreamSafely(r.Context(), progressive, model, text, sink.emit)
		if err == nil && result.Complete && result.Acknowledged {
			err = sink.validate(result)
		}
	} else {
		result, err = providerCompleteSafely(r.Context(), provider, model, text)
	}
	if result.ConversationRef != "" {
		w.Header().Set("X-LoomGate-Conversation", result.ConversationRef)
	}
	st.upstreamStarted = result.Attempted
	st.uid = result.AccountRef
	st.unreported = !result.UsageReported
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
	if !streaming {
		response := map[string]any{"id": id, "object": "chat.completion", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": result.Text}, "finish_reason": "stop"}}}
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
