package autoclaw

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"workbuddy2api/internal/providerretry"
)

func streamFrame(payload string) string { return "data: " + payload + "\n\n" }

func TestSSEEOFWithoutTerminalPreservesPartialFactsAndDoesNotRetry(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		t.Run(fmt.Sprintf("buffered=%t", buffered), func(t *testing.T) {
			vault, _ := refreshFixture(t)
			var calls atomic.Int32
			p := offlineRefreshProvider(t, vault, func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				response := refreshSuccess(r)
				response.Body = io.NopCloser(strings.NewReader(streamFrame(`{"choices":[{"delta":{"content":"partial fixture answer"}}],"usage":{"prompt_tokens":17}}`)))
				return response, nil
			})
			var output strings.Builder
			var emit func(string) error
			if !buffered {
				emit = func(s string) error { output.WriteString(s); return nil }
			}
			result, err := p.CompleteDialogue(t.Context(), attemptDialogue(), emit)
			if err == nil || result.Complete || result.Acknowledged {
				t.Fatal("plain EOF was reported as a completed generation")
			}
			if calls.Load() != 1 || !result.Attempted || result.Attempts != 1 || len(result.AttemptRecords) != 1 {
				t.Fatal("incomplete billable stream was replayed or lost")
			}
			if result.Text != "partial fixture answer" || !buffered && output.String() != result.Text {
				t.Fatal("partial output was discarded")
			}
			if result.Usage.Prompt == nil || *result.Usage.Prompt != 17 || result.Usage.Completion != nil {
				t.Fatal("partial usage became missing or an invented zero")
			}
			if result.AttemptRecords[0].Usage.Prompt == nil || result.AttemptRecords[0].ErrorCode == "" {
				t.Fatal("attempt lost observed usage or failure state")
			}
		})
	}
}

func TestSSEBadFramesCannotTurnPartialOutputIntoSuccess(t *testing.T) {
	for name, bad := range map[string]string{
		"invalid-json":   `{"choices":[`,
		"explicit-error": `{"error":{"code":"fixture_error","message":"SYNTHETIC_PRIVATE_UPSTREAM_DETAIL"}}`,
		"wrong-shape":    `{"choices":"fixture"}`,
		"null-root":      `null`,
		"empty-object":   `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			input := streamFrame(`{"choices":[{"delta":{"content":"partial"}}],"usage":{"prompt_tokens":5}}`) + streamFrame(bad) + streamFrame("[DONE]")
			text, _, usage, err := streamSSE(strings.NewReader(input), func(string) error { return nil }, nil)
			if err == nil {
				t.Fatal("bad upstream frame was silently accepted")
			}
			if text != "partial" || !usage.PromptSeen || usage.Prompt != 5 {
				t.Fatal("bad frame erased earlier observations")
			}
			if strings.Contains(err.Error(), "SYNTHETIC_PRIVATE") {
				t.Fatal("raw upstream error escaped the parser")
			}
		})
	}
}

func TestSSEFinalLineWithoutNewlineIsParsedWithTerminalReason(t *testing.T) {
	frames := `data: {"choices":[{"delta":{"reasoning_content":"fixture reasoning","content":"fixture answer"},"finish_reason":"length"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`
	var reasoning strings.Builder
	text, events, usage, err := streamSSE(strings.NewReader(frames), func(string) error { return nil }, func(s string) error { reasoning.WriteString(s); return nil })
	if err != nil || text != "fixture answer" || reasoning.String() != "fixture reasoning" || events != 2 {
		t.Fatalf("final unterminated line was not parsed: output=%t reasoning=%t events=%d err=%v", text != "", reasoning.Len() > 0, events, err)
	}
	if usage.FinishReason != "length" || !usage.CompletionSeen || usage.Completion != 2 {
		t.Fatal("final frame lost its reason or usage")
	}
}

type terminalThenFailure struct {
	first string
	reads int
}

func (r *terminalThenFailure) Read(p []byte) (int, error) {
	r.reads++
	if r.first != "" {
		n := copy(p, r.first)
		r.first = r.first[n:]
		return n, nil
	}
	return 0, errors.New("fixture_read_after_terminal")
}
func TestSSEDoneStopsReadingAndIgnoresTrailingData(t *testing.T) {
	reader := &terminalThenFailure{first: streamFrame(`{"choices":[{"delta":{"content":"answer"}}]}`) + streamFrame("[DONE]") + streamFrame(`{"choices":[{"delta":{"content":"must not emit"}}]}`)}
	text, _, _, err := streamSSE(reader, func(string) error { return nil }, nil)
	if err != nil || text != "answer" || reader.reads != 1 {
		t.Fatalf("DONE was not terminal: reads=%d text_matches=%t err=%v", reader.reads, text == "answer", err)
	}
}

func TestSSEInvalidToolIndexReturnsErrorInsteadOfPanicOrExpansion(t *testing.T) {
	for _, index := range []int{-1, 1024} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			defer func() {
				if recover() != nil {
					t.Error("invalid upstream index panicked")
				}
			}()
			input := streamFrame(fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":%d,"id":"fixture-call","type":"function","function":{"name":"fixture","arguments":"{}"}}]}}]}`, index)) + streamFrame("[DONE]")
			_, _, _, tools, err := streamSSEWithTools(strings.NewReader(input), func(string) error { return nil }, nil, nil)
			if err == nil || len(tools) > 64 {
				t.Fatal("unbounded tool index was accepted")
			}
		})
	}
}

func TestSSELineLimitRejectsOversizedFrame(t *testing.T) {
	input := "data: " + strings.Repeat(" ", (1<<20)+1)
	_, _, _, err := streamSSE(strings.NewReader(input), func(string) error { return nil }, nil)
	var code interface{ PublicCode() string }
	if !errors.As(err, &code) || code.PublicCode() != "autoclaw_stream_limit" {
		t.Fatal("oversized unterminated frame was not rejected at the input bound")
	}
}

func TestSSEReasoningOnlyRequiresTerminalButIsNotAnEmptyAnswer(t *testing.T) {
	frame := streamFrame(`{"choices":[{"delta":{"reasoning_content":"fixture thought"}}]}`)
	_, _, _, err := streamSSE(strings.NewReader(frame), func(string) error { return nil }, nil)
	var empty *emptyAnswerError
	if err == nil || errors.As(err, &empty) {
		t.Fatal("unfinished reasoning was treated as complete or as no output")
	}
	_, events, _, err := streamSSE(strings.NewReader(frame+streamFrame("[DONE]")), func(string) error { return nil }, nil)
	if err != nil || events != 1 {
		t.Fatal("terminal reasoning-only turn was rejected")
	}
}

func TestSSEEmptyEOFIsNotRetriedAsAnEmptyCompletedAnswer(t *testing.T) {
	vault, _ := refreshFixture(t)
	var calls atomic.Int32
	provider := offlineRefreshProvider(t, vault, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		response := refreshSuccess(r)
		response.Body = io.NopCloser(strings.NewReader(": fixture keepalive\n\n"))
		return response, nil
	})
	_, err := provider.CompleteDialogue(t.Context(), attemptDialogue(), nil)
	var empty *emptyAnswerError
	if err == nil || errors.As(err, &empty) || calls.Load() != 1 {
		t.Fatalf("unacknowledged empty EOF regenerated: calls=%d error=%t", calls.Load(), err != nil)
	}
}

func TestSSEErrorFrameRetainsItsReportedUsage(t *testing.T) {
	frames := streamFrame(`{"choices":[{"delta":{"content":"partial"}}]}`) + streamFrame(`{"error":{"message":"SYNTHETIC_PRIVATE_DETAIL"},"usage":{"prompt_tokens":9,"completion_tokens":2}}`)
	text, _, usage, err := streamSSE(strings.NewReader(frames), func(string) error { return nil }, nil)
	if err == nil || text != "partial" || !usage.PromptSeen || usage.Prompt != 9 || !usage.CompletionSeen || usage.Completion != 2 {
		t.Fatal("error envelope lost its own valid usage observations")
	}
}

func TestSSEFragmentedUTF8AndSplitUsagePreserveExactValues(t *testing.T) {
	frames := ": fixture keepalive\r\n" + streamFrame(`{"choices":[{"delta":{"reasoning_content":"合成思考"}}],"usage":{"prompt_tokens":9,"prompt_tokens_details":{"cached_tokens":0}}}`) +
		streamFrame(`{"choices":[{"delta":{"content":"合成答案"},"finish_reason":"stop"}],"usage":{"completion_tokens":2}}`) +
		streamFrame(`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2}}`) + "data: [DONE]"
	var reasoning strings.Builder
	text, events, usage, err := streamSSE(iotest.OneByteReader(strings.NewReader(frames)), nil, func(s string) error { reasoning.WriteString(s); return nil })
	if err != nil || text != "合成答案" || reasoning.String() != "合成思考" || events != 2 {
		t.Fatal("byte-fragmented input changed text or reasoning")
	}
	if !usage.PromptSeen || usage.Prompt != 9 || !usage.CompletionSeen || usage.Completion != 2 || !usage.CachedSeen || usage.Cached != 0 {
		t.Fatal("cumulative/split usage was summed, zero-filled or dropped")
	}
}

func TestSSEInvalidUsageCannotErasePriorObservation(t *testing.T) {
	for _, raw := range []string{`{"prompt_tokens":-1}`, `{"completion_tokens":"2"}`, `{"prompt_tokens":8,"prompt_tokens_details":{"cached_tokens":9}}`, `{"prompt_tokens":9223372036854775808}`} {
		input := streamFrame(`{"choices":[{"delta":{"content":"partial"}}],"usage":{"prompt_tokens":9}}`) + streamFrame(`{"choices":[],"usage":`+raw+`}`) + streamFrame("[DONE]")
		_, _, usage, err := streamSSE(strings.NewReader(input), nil, nil)
		if err == nil || !usage.PromptSeen || usage.Prompt != 9 {
			t.Fatal("malformed usage was accepted or replaced the known count")
		}
	}
}

func TestSSECallbackErrorStopsConsumptionAndSurvivesWrapping(t *testing.T) {
	failure := errors.New("fixture_sink_closed")
	reader := &terminalThenFailure{first: streamFrame(`{"choices":[{"delta":{"content":"first"}}]}`) + streamFrame(`{"choices":[{"delta":{"content":"second"}}]}`) + streamFrame("[DONE]")}
	calls := 0
	_, _, _, err := streamSSE(reader, func(string) error { calls++; return failure }, nil)
	if !errors.Is(err, failure) || calls != 1 || reader.reads != 1 {
		t.Fatal("callback failure was ignored or read further output")
	}
}

func TestSSEDecoderBoundsOutputAcrossSmallFrames(t *testing.T) {
	chunk := streamFrame(`{"choices":[{"delta":{"reasoning_content":"` + strings.Repeat("x", 32<<10) + `"}}]}`)
	_, _, _, err := streamSSE(io.MultiReader(strings.NewReader(strings.Repeat(chunk, 257)), strings.NewReader(streamFrame("[DONE]"))), nil, nil)
	var code interface{ PublicCode() string }
	if !errors.As(err, &code) || code.PublicCode() != "autoclaw_stream_limit" {
		t.Fatal("small frames bypassed the aggregate output bound")
	}
}

func TestSSEToolIdentityAndBoundsRejectConflicts(t *testing.T) {
	first := streamFrame(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"fixture-call","type":"function","function":{"name":"fixture","arguments":"{}"}}]}}]}`)
	for name, fragment := range map[string]string{
		"id-changed":   `{"index":0,"id":"other-call"}`,
		"id-reused":    `{"index":1,"id":"fixture-call","function":{"name":"other","arguments":"{}"}}`,
		"sparse-index": `{"index":3,"id":"other-call"}`,
		"wrong-type":   `{"index":0,"type":"other"}`,
		"long-name":    `{"index":0,"function":{"name":"` + strings.Repeat("n", 257) + `"}}`,
		"long-id":      `{"index":0,"id":"` + strings.Repeat("i", 257) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			frames := first + streamFrame(`{"choices":[{"delta":{"tool_calls":[`+fragment+`]}}]}`) + streamFrame("[DONE]")
			_, _, _, _, err := streamSSEWithTools(strings.NewReader(frames), nil, nil, nil)
			if err == nil {
				t.Fatal("conflicting or unbounded tool metadata was accepted")
			}
		})
	}
}

func TestSSEWireLimitCoversNonOutputKeepalives(t *testing.T) {
	comment := ":" + strings.Repeat(" ", 8190) + "\n"
	readers := make([]io.Reader, 0, 8193)
	for i := 0; i < 8193; i++ {
		readers = append(readers, strings.NewReader(comment))
	}
	_, _, _, err := streamSSE(io.MultiReader(readers...), nil, nil)
	var code interface{ PublicCode() string }
	if !errors.As(err, &code) || code.PublicCode() != "autoclaw_stream_limit" {
		t.Fatal("non-output data bypassed the bounded wire size")
	}
}

func TestSSEMalformedTurnIsFatalEvenBeforeDelivery(t *testing.T) {
	vault, _ := refreshFixture(t)
	var calls atomic.Int32
	p := offlineRefreshProvider(t, vault, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		response := refreshSuccess(r)
		response.Body = io.NopCloser(strings.NewReader(streamFrame(`{bad fixture`) + streamFrame("[DONE]")))
		return response, nil
	})
	_, err := p.CompleteDialogue(t.Context(), attemptDialogue(), nil)
	if err == nil || calls.Load() != 1 {
		t.Fatalf("malformed generation was accepted or regenerated: calls=%d failed=%t", calls.Load(), err != nil)
	}
	var classified providerretry.Classified
	if !errors.As(err, &classified) || classified.RetryClass() != providerretry.Fatal {
		t.Fatal("malformed stream is not explicitly non-retryable")
	}
}
