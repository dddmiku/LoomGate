package server

import (
	"context"
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
			entry := map[string]any{"id": m.ID, "object": "model", "created": 0, "owned_by": id, "name": m.Name, "supports_tool_call": false, "supports_images": false, "single_turn_text_only": true}
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
		default:
			st.status = 400
			writeOpenAIError(w, 400, "provider_parameter_unsupported", "this provider currently supports one ordinary text message; the request contains an unsupported field")
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
	if json.Unmarshal(fields["messages"], &messages) != nil || len(messages) != 1 || messages[0].Role != "user" || json.Unmarshal(messages[0].Content, &text) != nil || strings.TrimSpace(text) == "" || len(text) > 65536 {
		st.status = 400
		writeOpenAIError(w, 400, "provider_input_unsupported", "this provider currently requires exactly one user message containing text (up to 65536 bytes)")
		return
	}
	var result inference.Result
	var err error
	var sink *providerStreamSink
	if progressive, ok := provider.(inference.StreamingProvider); streaming && ok {
		sink = newProviderStreamSink(w, r, model, st)
		result, err = providerStreamSafely(r.Context(), progressive, model, text, sink.emit)
		if err == nil && result.Complete && result.Acknowledged {
			err = sink.validate(result)
		}
	} else {
		result, err = providerCompleteSafely(r.Context(), provider, model, text)
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
	w.Header().Set("X-Gateway-Usage-Status", "not-reported")
	w.Header().Set("X-Gateway-Upstream-Complete", "true")
	w.Header().Set("X-Gateway-Upstream-Ack", "true")
	w.Header().Set("X-Gateway-Upstream-Request-Digest", result.RequestDigest)
	if !streaming {
		err = writeJSON(w, 200, map[string]any{"id": id, "object": "chat.completion", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": result.Text}, "finish_reason": "stop"}}})
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
