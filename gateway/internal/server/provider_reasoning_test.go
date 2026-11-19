package server

// The upstream thinks before it answers: reasoning deltas arrive from the
// first second while the answer may take far longer. Forwarding only answer
// content left the client blank for the whole think phase, which is what made
// a three-second think look like a two-minute hang in the request log.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/inference"
)

// reasoningProvider is a StreamingProvider that also emits thinking deltas.
type reasoningProvider struct{}

func (reasoningProvider) Models(context.Context) ([]inference.Model, error) {
	return []inference.Model{{ID: "thinking:model"}}, nil
}

func (reasoningProvider) Complete(context.Context, string, string) (inference.Result, error) {
	return inference.Result{}, nil
}

func (reasoningProvider) Stream(context.Context, string, string, func(string) error) (inference.Result, error) {
	return inference.Result{}, nil
}

// CompleteDialogue sends reasoning first, then the answer, exactly as the
// upstream does.
func (reasoningProvider) CompleteDialogue(_ context.Context, _ inference.Dialogue, emit func(string) error) (inference.Result, error) {
	return reasoningProvider{}.CompleteDialogueReasoning(context.Background(), inference.Dialogue{}, emit, nil)
}

func (reasoningProvider) CompleteDialogueReasoning(_ context.Context, _ inference.Dialogue, emit, emitReasoning func(string) error) (inference.Result, error) {
	if emitReasoning != nil {
		for _, d := range []string{"先想", "一下"} {
			if err := emitReasoning(d); err != nil {
				return inference.Result{Attempted: true}, err
			}
		}
	}
	for _, d := range []string{"答", "案"} {
		if err := emit(d); err != nil {
			return inference.Result{Attempted: true}, err
		}
	}
	return inference.Result{Text: "答案", Attempted: true, Acknowledged: true, Complete: true, Events: 4}, nil
}

func TestReasoningIsForwardedBeforeTheAnswer(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	st := &chatStat{}
	sink := newProviderStreamSink(rec, req, "thinking:model", st)

	provider := reasoningProvider{}
	if _, err := provider.CompleteDialogueReasoning(context.Background(), inference.Dialogue{Model: "thinking:model"}, sink.emit, sink.emitReasoning); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()

	reasoningAt := strings.Index(body, `"reasoning_content":"先想"`)
	answerAt := strings.Index(body, `"content":"答"`)
	if reasoningAt < 0 {
		t.Fatalf("reasoning was never forwarded; the client stays blank through the think phase:\n%s", body)
	}
	if answerAt < 0 {
		t.Fatalf("answer content missing:\n%s", body)
	}
	if reasoningAt > answerAt {
		t.Fatal("reasoning must be emitted before the answer, matching the upstream order")
	}

	// The first frame opens the stream, so TTFB is recorded at the thinking
	// delta rather than when the answer finally starts.
	if st.ttfb == 0 {
		t.Fatal("TTFB was not recorded on the first frame")
	}

	// Reasoning must never be mistaken for answer text: the sink's validate()
	// compares against the provider's own result text.
	if got := sink.text.String(); got != "答案" {
		t.Fatalf("answer text=%q; reasoning leaked into it", got)
	}
	if sink.reasoning.String() != "先想一下" {
		t.Fatalf("reasoning accumulator=%q", sink.reasoning.String())
	}
}

// A client that ignores reasoning_content must still see the same frames it
// always saw, in order.
func TestReasoningFramesStayOutOfTheAnswerStream(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	sink := newProviderStreamSink(rec, req, "thinking:model", &chatStat{})
	_ = sink.emitReasoning("想")
	_ = sink.emit("答")

	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n")
	var sawReasoning, sawAnswer bool
	for _, line := range lines {
		if strings.Contains(line, `"reasoning_content"`) {
			if strings.Contains(line, `"content"`) {
				t.Fatalf("one frame carried both reasoning and content: %s", line)
			}
			sawReasoning = true
		}
		if strings.Contains(line, `"content"`) {
			sawAnswer = true
		}
	}
	if !sawReasoning || !sawAnswer {
		t.Fatalf("missing frames: reasoning=%v answer=%v\n%s", sawReasoning, sawAnswer, rec.Body.String())
	}
}
