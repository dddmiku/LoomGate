package autoclaw

import (
	"context"
	"errors"
	"io"

	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providerretry"
)

type deliveredError struct{ error }

func (e *deliveredError) Error() string                   { return "autoclaw_response_interrupted" }
func (e *deliveredError) Unwrap() error                   { return e.error }
func (e *deliveredError) RetryClass() providerretry.Class { return providerretry.Fatal }

func attemptErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "client_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "upstream_timeout"
	}
	var public interface{ PublicCode() string }
	if errors.As(err, &public) && public.PublicCode() != "" {
		return public.PublicCode()
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "stream_incomplete"
	}
	return "upstream_failed"
}

func (u streamUsage) observed() inference.TokenUsage {
	var out inference.TokenUsage
	if u.PromptSeen {
		value := u.Prompt
		out.Prompt = &value
	}
	if u.CompletionSeen {
		value := u.Completion
		out.Completion = &value
	}
	if u.CachedSeen {
		value := u.Cached
		out.Cached = &value
	}
	return out
}
