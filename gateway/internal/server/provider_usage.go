package server

import (
	"net/http"

	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/requestlog"
)

func providerUsage(result inference.Result) inference.TokenUsage {
	u := result.Usage
	if u.Reported() {
		return u
	}
	if !result.UsageReported {
		return u
	}
	// Older providers report input/output as a pair and cannot distinguish an
	// omitted cache count from zero.
	u = inference.TokenUsage{Prompt: &result.PromptTokens, Completion: &result.CompletionTokens}
	if result.CachedTokens > 0 {
		u.Cached = &result.CachedTokens
	}
	return u
}

func providerWireUsage(u inference.TokenUsage) map[string]any {
	if !u.Reported() {
		return nil
	}
	wire := make(map[string]any, 4)
	if u.Prompt != nil {
		wire["prompt_tokens"] = *u.Prompt
	}
	if u.Completion != nil {
		wire["completion_tokens"] = *u.Completion
	}
	if u.Complete() {
		wire["total_tokens"] = int64(*u.Prompt) + int64(*u.Completion)
	}
	if u.Cached != nil && (u.Prompt == nil || *u.Cached <= *u.Prompt) {
		wire["prompt_tokens_details"] = map[string]any{"cached_tokens": *u.Cached}
	}
	return wire
}

func providerCount(n *int) *int64 {
	if n == nil {
		return nil
	}
	return traceCount(*n)
}

func providerTokenValue(n *int) int {
	if n == nil {
		return -1
	}
	return *n
}

func recordProviderAttempts(st *chatStat, result inference.Result, model string, err error) {
	st.upstreamStarted = result.Attempted
	st.uid = result.AccountRef
	u := providerUsage(result)
	st.unreported = !u.Complete()
	st.prompt, st.toks, st.cached = providerTokenValue(u.Prompt), providerTokenValue(u.Completion), providerTokenValue(u.Cached)
	if st.trace == nil {
		return
	}
	trace := st.trace
	trace.account, trace.model = result.AccountRef, model
	trace.record.UpstreamStarted = result.Attempted
	records := result.AttemptRecords
	summarized := len(records) == 0 && result.Attempted
	if summarized {
		// Some providers signal submission but cannot report the transport status.
		records = []inference.Attempt{{AccountRef: result.AccountRef, FinishReason: result.FinishReason, Usage: u}}
		if err != nil || !result.Complete || !result.Acknowledged {
			records[0].ErrorCode = providerErrorCode(err)
		}
	}
	trace.record.AttemptCount = len(records)
	for i, a := range records {
		status := requestlog.StatusError
		switch {
		case a.ErrorCode == "client_canceled" || a.ErrorCode == "response_delivery_failed":
			status = requestlog.StatusCanceled
		case (a.HTTPStatus == http.StatusOK || summarized) && a.ErrorCode == "":
			status = requestlog.StatusSuccess
		}
		if len(records) == 1 {
			if a.AccountRef == "" {
				a.AccountRef = result.AccountRef
			}
			if status == requestlog.StatusSuccess && !a.Usage.Reported() {
				a.Usage = u
			}
		}
		attempt := requestlog.Attempt{
			Number: i + 1, AccountID: a.AccountRef, Model: model,
			StartedAt: a.StartedAt, FinishedAt: a.FinishedAt,
			DurationMS: max(0, a.FinishedAt.Sub(a.StartedAt).Milliseconds()),
			HTTPStatus: a.HTTPStatus, Status: status,
			ErrorCode: traceCode(a.ErrorCode), FinishReason: traceCode(a.FinishReason),
			InputTokens: providerCount(a.Usage.Prompt), OutputTokens: providerCount(a.Usage.Completion),
			CachedTokens: providerCount(a.Usage.Cached),
		}
		switch {
		case attempt.InputTokens != nil && attempt.OutputTokens != nil:
			attempt.UsageState = requestlog.UsageComplete
		case attempt.InputTokens != nil || attempt.OutputTokens != nil || attempt.CachedTokens != nil:
			attempt.UsageState, attempt.UsageReason = requestlog.UsagePartial, "partial_usage"
		default:
			attempt.UsageState, attempt.UsageReason = requestlog.UsageMissing, "usage_not_reported"
		}
		if i < requestlog.MaxAttempts {
			trace.record.Attempts = append(trace.record.Attempts, attempt)
			trace.last = &trace.record.Attempts[len(trace.record.Attempts)-1]
		} else {
			trace.record.AttemptsTruncated = true
			trace.last = &attempt
		}
	}
}

func providerFinishReason(reason string, toolCalls bool) string {
	if reason != "" {
		return reason
	}
	if toolCalls {
		return "tool_calls"
	}
	return "stop"
}
