package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
	"workbuddy2api/internal/inference"
)

func providerStreamSafely(ctx context.Context, p inference.StreamingProvider, model, text string, emit func(string) error) (result inference.Result, err error) {
	defer func() {
		if recover() != nil {
			result = inference.Result{Attempted: true}
			err = inference.ErrStreamContract
		}
	}()
	return p.Stream(ctx, model, text, emit)
}

func providerStreamToolsSafely(ctx context.Context, p inference.ToolCapableProvider, q inference.Dialogue, emit, emitReasoning func(string) error, emitTool func(inference.ToolCallDelta) error) (result inference.Result, err error) {
	defer func() {
		if recover() != nil {
			result = inference.Result{Attempted: true}
			err = inference.ErrStreamContract
		}
	}()
	return p.CompleteDialogueTools(ctx, q, emit, emitReasoning, emitTool)
}

func providerStreamReasoningSafely(ctx context.Context, p inference.ReasoningProvider, q inference.Dialogue, emit, emitReasoning func(string) error) (result inference.Result, err error) {
	defer func() {
		if recover() != nil {
			result = inference.Result{Attempted: true}
			err = inference.ErrStreamContract
		}
	}()
	return p.CompleteDialogueReasoning(ctx, q, emit, emitReasoning)
}

// All writes occur on the request goroutine. The outer boundedResponseWriter
// applies a deadline to each write/flush and cancels the provider on failure.
type providerStreamSink struct {
	w         http.ResponseWriter
	ctx       context.Context
	id, model string
	created   int64
	started   bool
	text      strings.Builder
	// reasoning accumulates thinking deltas so validate() can tell an answer
	// from a think-only stream; it is never part of result.Text.
	reasoning strings.Builder
	// sawToolCalls marks a turn whose answer is a tool request; such a turn
	// legitimately carries no prose.
	sawToolCalls bool
	err          error
	stat         *chatStat
}

func newProviderStreamSink(w http.ResponseWriter, r *http.Request, model string, st *chatStat) *providerStreamSink {
	return &providerStreamSink{w: w, ctx: r.Context(), id: "chatcmpl-" + requestID(r), model: model, created: time.Now().Unix(), stat: st}
}
func (s *providerStreamSink) frame(value any) error {
	if s.err != nil {
		return s.err
	}
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return err
	}
	raw, err := json.Marshal(value)
	if err == nil {
		_, err = fmt.Fprintf(s.w, "data: %s\n\n", raw)
	}
	if err == nil {
		err = flushHTTPResponse(s.w)
	}
	s.err = err
	return err
}
func (s *providerStreamSink) start() bool {
	if s.started {
		return false
	}
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("X-Accel-Buffering", "no")
	s.w.Header().Set("X-Gateway-Streaming", "incremental")
	s.w.Header().Set("X-Gateway-Usage-Status", "not-reported")
	s.w.Header().Set("X-Gateway-Upstream-Ack", "true")
	s.w.Header().Add("Trailer", "X-Gateway-Upstream-Complete, X-Gateway-Upstream-Request-Digest, X-Gateway-Stream-Status, X-LoomGate-Conversation")
	if s.stat != nil && s.stat.ttfb == 0 {
		s.stat.ttfb = time.Since(s.stat.start)
	}
	s.started = true
	return true
}

func (s *providerStreamSink) chunk(delta map[string]string, finish any) map[string]any {
	return map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
}
func (s *providerStreamSink) emit(delta string) error {
	if s.err != nil {
		return s.err
	}
	if !utf8.ValidString(delta) || s.text.Len()+len(delta) > 8<<20 {
		s.err = inference.ErrStreamContract
		return s.err
	}
	if delta == "" {
		return nil
	}
	first := s.start()
	content := map[string]string{"content": delta}
	if first {
		content["role"] = "assistant"
	}
	if err := s.frame(s.chunk(content, nil)); err != nil {
		return err
	}
	s.text.WriteString(delta)
	return nil
}

// emitToolDelta forwards one tool-call fragment as an OpenAI tool_calls delta.
// The first fragment for an index carries id and function name; the rest
// append arguments, exactly as the client expects to reassemble them.
func (s *providerStreamSink) emitToolDelta(delta inference.ToolCallDelta) error {
	if s.err != nil {
		return s.err
	}
	first := s.start()
	index := delta.Index
	call := map[string]any{
		"index": index,
		"type":  "function",
		"function": map[string]any{
			"name":      delta.Name,
			"arguments": delta.Arguments,
		},
	}
	if delta.ID != "" {
		call["id"] = delta.ID
	}
	deltaMap := map[string]any{"tool_calls": []any{call}}
	if first {
		deltaMap["role"] = "assistant"
	}
	if err := s.frame(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": deltaMap, "finish_reason": nil}},
	}); err != nil {
		return err
	}
	s.sawToolCalls = true
	return nil
}

// emitReasoning forwards a thinking delta. It opens the stream and sets TTFB
// exactly as emit does, because the client has genuinely received bytes at
// that moment — the model is working, and staying silent until the answer
// starts made a 3-second think look like a two-minute hang.
//
// Reasoning is reported as reasoning_content, the field OpenAI-compatible
// clients already understand for models that think before answering. It never
// enters the answer text, so a client that ignores the field still gets the
// same final content it would have without it.
func (s *providerStreamSink) emitReasoning(delta string) error {
	if s.err != nil {
		return s.err
	}
	if delta == "" {
		return nil
	}
	if !utf8.ValidString(delta) || s.reasoning.Len()+len(delta) > 8<<20 {
		s.err = inference.ErrStreamContract
		return s.err
	}
	first := s.start()
	content := map[string]string{"reasoning_content": delta}
	if first {
		content["role"] = "assistant"
	}
	if err := s.frame(s.chunk(content, nil)); err != nil {
		return err
	}
	s.reasoning.WriteString(delta)
	return nil
}
func (s *providerStreamSink) validate(result inference.Result) error {
	if s.err != nil {
		return s.err
	}
	// A tool-call turn has no answer text by design, so the text comparison
	// only applies to prose turns. A think-only turn is the same: the model
	// produced output, it just went out as reasoning_content.
	if s.sawToolCalls {
		return nil
	}
	if s.text.Len() == 0 {
		if result.Text == "" && len(result.ToolCalls) == 0 && result.FinishReason == "content_filter" {
			// An explicit upstream filter is a terminal outcome, not an empty
			// transport failure that should be generated again.
			return nil
		}
		if s.reasoning.Len() > 0 && result.Text == "" {
			// Reasoning was already streamed; treating this as a contract
			// violation would fail a turn the client watched succeed.
			return nil
		}
		return inference.ErrStreamContract
	}
	if !s.started || s.text.String() != result.Text {
		return inference.ErrStreamContract
	}
	return nil
}
func (s *providerStreamSink) fail(code string) {
	s.w.Header().Set("X-Gateway-Upstream-Complete", "false")
	s.w.Header().Set("X-Gateway-Stream-Status", "failed")
	_ = s.frame(map[string]any{"error": map[string]string{"type": "upstream_error", "code": code, "message": "provider stream did not complete; discard the incomplete answer and inspect status before resubmitting"}})
}
func (s *providerStreamSink) finish(result inference.Result) error {
	// The finish_reason tells an agent client what to do next: tool_calls means
	// "run these and send the results back", stop means the turn is complete.
	// The upstream's own cause wins when it reported one: rewriting "length" or
	// "content_filter" to "stop" hid a truncated answer from the caller.
	reason := providerFinishReason(result.FinishReason, s.sawToolCalls || len(result.ToolCalls) > 0)
	last := s.chunk(map[string]string{}, reason)
	// The final chunk is where OpenAI-compatible clients read usage from.
	// Omitted when the upstream reported none, so a client never sees a
	// fabricated zero.
	if usage := providerWireUsage(providerUsage(result)); usage != nil {
		last["usage"] = usage
	}
	if result.ConversationRef != "" {
		last["loomgate_conversation"] = result.ConversationRef
	}

	s.start()
	if err := s.frame(last); err != nil {
		return err
	}
	if _, err := fmt.Fprint(s.w, "data: [DONE]\n\n"); err != nil {
		s.err = err
		return err
	}
	if err := flushHTTPResponse(s.w); err != nil {
		s.err = err
		return err
	}
	s.w.Header().Set("X-Gateway-Upstream-Complete", "true")
	s.w.Header().Set("X-Gateway-Upstream-Request-Digest", result.RequestDigest)
	s.w.Header().Set("X-Gateway-Stream-Status", "complete")
	if result.ConversationRef != "" {
		s.w.Header().Set("X-LoomGate-Conversation", result.ConversationRef)
	}
	return nil
}
