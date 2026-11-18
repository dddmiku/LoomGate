package server

import (
	"context"

	"workbuddy2api/internal/inference"
)

// Effort-aware provider seam.
//
// Reasoning effort is optional metadata on the OpenAI-compatible request. A
// provider that understands it exposes StreamWithEffort; one that does not
// keeps working and simply ignores the field. Nothing here may turn an unknown
// or unsupported effort into a request failure.
//
// The interface deliberately requires only StreamWithEffort: a provider might
// offer buffered completion plus effort support without implementing the full
// StreamingProvider contract.
type effortProvider interface {
	StreamWithEffort(context.Context, string, string, string, func(string) error) (inference.Result, error)
}

// effortAware reports whether the provider can honour an effort.
func effortAware(p inference.Provider) (effortProvider, bool) {
	aware, ok := p.(effortProvider)
	return aware, ok
}

func providerStreamSafelyWithEffort(ctx context.Context, p effortProvider, model, text, effort string, emit func(string) error) (result inference.Result, err error) {
	defer func() {
		if recover() != nil {
			result = inference.Result{Attempted: true}
			err = inference.ErrStreamContract
		}
	}()
	return p.StreamWithEffort(ctx, model, text, effort, emit)
}

// providerEffortCompleteSafely is the buffered counterpart: a nil emit tells the
// provider to accumulate text and return it in Result.Text.
func providerEffortCompleteSafely(ctx context.Context, p effortProvider, model, text, effort string) (result inference.Result, err error) {
	return providerStreamSafelyWithEffort(ctx, p, model, text, effort, nil)
}

// providerEffortDialogueSafely applies the effort to a dialogue while keeping
// the dialogue contract (owner, conversation ref, idempotency key) intact.
func providerEffortDialogueSafely(ctx context.Context, p effortProvider, q inference.Dialogue, effort string, emit func(string) error) (result inference.Result, err error) {
	return providerStreamSafelyWithEffort(ctx, p, q.Model, flattenDialogueForEffort(q), effort, emit)
}

// flattenDialogueForEffort mirrors the provider-side flattening so an
// effort-aware dialogue sees the same text it would have received otherwise.
func flattenDialogueForEffort(q inference.Dialogue) string {
	if len(q.Messages) == 1 {
		return q.Messages[0].Content
	}
	out := ""
	for _, m := range q.Messages {
		if m.Role == "assistant" {
			out += "Assistant: "
		} else {
			out += "User: "
		}
		out += m.Content
		out += "\n\n"
	}
	out += "Assistant: "
	return out
}
