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

// reasoningAndToolsProvider emits reasoning first, then a tool call — the shape
// a thinking model produces. Enabling tools used to replace the reasoning sink
// with nil, so the client lost every progress frame until the call arrived.
type reasoningAndToolsProvider struct{ reasoningProvider }

func (reasoningAndToolsProvider) SupportsTools() bool { return true }

func (p reasoningAndToolsProvider) CompleteDialogueTools(_ context.Context, _ inference.Dialogue, emit, emitReasoning func(string) error, emitTool func(inference.ToolCallDelta) error) (inference.Result, error) {
	if emitReasoning != nil {
		if err := emitReasoning("先想"); err != nil {
			return inference.Result{Attempted: true}, err
		}
	}
	if emitTool != nil {
		if err := emitTool(inference.ToolCallDelta{Index: 0, ID: "call_1", Name: "get_weather", Arguments: `{"city":"北京"}`}); err != nil {
			return inference.Result{Attempted: true}, err
		}
	}
	return inference.Result{
		Attempted: true, Acknowledged: true, Complete: true, Events: 2,
		ToolCalls: []inference.ToolCall{{ID: "call_1", Type: "function",
			Function: inference.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"北京"}`}}},
	}, nil
}

func TestReasoningStillStreamsOnAToolTurn(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	sink := newProviderStreamSink(rec, req, "thinking:model", &chatStat{})

	provider := reasoningAndToolsProvider{}
	if _, err := provider.CompleteDialogueTools(context.Background(), inference.Dialogue{Model: "thinking:model"},
		sink.emit, sink.emitReasoning, sink.emitToolDelta); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	reasoningAt := strings.Index(body, `"reasoning_content":"先想"`)
	toolAt := strings.Index(body, `"tool_calls"`)
	if reasoningAt < 0 {
		t.Fatalf("reasoning was dropped on a tool turn; the client sees no progress until the call:\n%s", body)
	}
	if toolAt < 0 {
		t.Fatalf("tool call missing:\n%s", body)
	}
	if reasoningAt > toolAt {
		t.Fatal("reasoning must arrive before the tool call, matching the upstream order")
	}
}

// Non-streaming tool requests have no sink. The handler used to build method
// values off a nil sink anyway; the first delta would then panic. A buffered
// tool turn must reach the provider with nil emitters and still return calls.
func TestNonStreamingToolTurnUsesNilEmitters(t *testing.T) {
	provider := reasoningAndToolsProvider{}
	result, err := provider.CompleteDialogueTools(context.Background(), inference.Dialogue{Model: "thinking:model"},
		nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("buffered tool turn lost the call: %+v", result)
	}
}

// A think-only turn delivered reasoning frames and nothing else. It must pass
// the stream contract: the client watched the model work, and failing the turn
// after that tells it to discard a valid response.
func TestThinkOnlyTurnSatisfiesTheStreamContract(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	sink := newProviderStreamSink(rec, req, "thinking:model", &chatStat{})
	if err := sink.emitReasoning("想了很久"); err != nil {
		t.Fatal(err)
	}
	result := inference.Result{Attempted: true, Acknowledged: true, Complete: true, Events: 1}
	if err := sink.validate(result); err != nil {
		t.Fatalf("think-only turn failed the contract: %v", err)
	}
	if err := sink.finish(result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), `"reasoning_content":"想了很久"`) {
		t.Fatalf("reasoning frame missing:\n%s", rec.Body.String())
	}
}
