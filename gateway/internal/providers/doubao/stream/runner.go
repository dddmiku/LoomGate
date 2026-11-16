package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"
)

// Request is a private outgoing protocol envelope. Initial requests are never
// retried without a server ACK and observed sequence. Async resumes use the key
// returned by FETCH_STREAM and preserve their own independent cursor.
type Request struct {
	Path   string
	Body   json.RawMessage
	Resume bool
}

func (Request) String() string   { return "doubao.stream.Request[redacted]" }
func (Request) GoString() string { return "doubao.stream.Request[redacted]" }

type Opener interface {
	Open(context.Context, Request) (io.ReadCloser, error)
}

// Permanent transport failures (identity, policy, limits) must never resume.
type permanentError struct{ error }

func NoResume(err error) error          { return &permanentError{err} }
func (e *permanentError) Unwrap() error { return e.error }

type HTTP struct {
	Client  *http.Client
	Prepare func(context.Context, Request) (*http.Request, error)
}

func (h HTTP) Open(ctx context.Context, r Request) (io.ReadCloser, error) {
	if h.Prepare == nil {
		return nil, errors.New("doubao_stream_signer_unavailable")
	}
	req, err := h.Prepare(ctx, r)
	if err != nil {
		return nil, errors.New("doubao_stream_prepare_failed")
	}
	if req == nil || req.URL == nil || req.Method != http.MethodPost || req.URL.Path != r.Path || req.URL.Scheme != "https" || req.URL.Host != "www.doubao.com" || req.URL.User != nil {
		return nil, errors.New("doubao_stream_origin_rejected")
	}
	client := http.Client{}
	if h.Client != nil {
		client = *h.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, errors.New("doubao_stream_transport_failed")
	}
	media, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != 200 || parseErr != nil || media != "text/event-stream" {
		response.Body.Close()
		return nil, errors.New("doubao_stream_response_rejected")
	}
	return response.Body, nil
}

// PreparedJSON is for a verified, per-request signer to consume. This helper
// creates no signature and is not wired to production inference by itself.
func PreparedJSON(ctx context.Context, r Request, query url.Values, headers http.Header) (*http.Request, error) {
	u := url.URL{Scheme: "https", Host: "www.doubao.com", Path: r.Path, RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(r.Body))
	if err != nil {
		return nil, ErrMalformed
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return req, nil
}

type Options struct {
	Limits                           Limits
	Timeout, AttemptTimeout, Backoff time.Duration
	MaxResumes                       int
}
type Report struct {
	Events, Replayed, Messages, Handoffs, Attempts int
	ProtocolComplete, NativeContent                bool
	Acknowledged                                   bool
}
type Runner struct {
	Opener  Opener
	Options Options
}

func (r Runner) Run(ctx context.Context, initial json.RawMessage, emit func(Update) error) (Report, error) {
	o := r.Options
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Minute
	}
	if o.AttemptTimeout <= 0 {
		o.AttemptTimeout = 30 * time.Second
	}
	if o.Backoff <= 0 {
		o.Backoff = 250 * time.Millisecond
	}
	if o.MaxResumes < 0 || o.MaxResumes > 5 {
		return Report{}, ErrLimit
	}
	var initialBody map[string]json.RawMessage
	if decode(initial, &initialBody) != nil || initialBody == nil {
		return Report{}, ErrMalformed
	}
	if len(initial) > limits(o.Limits).EventBytes {
		return Report{}, ErrLimit
	}
	if r.Opener == nil {
		return Report{}, errors.New("doubao_stream_opener_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	state := NewState(o.Limits)
	var ownership struct {
		Meta struct {
			Local string `json:"local_conversation_id"`
		} `json:"client_meta"`
		Messages []struct {
			Local string `json:"local_message_id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(initial, &ownership)
	budget := &byteBudget{remaining: limits(o.Limits).TotalBytes}
	report := Report{}
	finish := func(err error) (Report, error) {
		report.Events = state.Events
		report.Replayed = state.Replayed
		report.Messages = len(state.Messages)
		report.Handoffs = len(state.Handoffs)
		report.NativeContent = state.NativeContent
		report.ProtocolComplete = err == nil
		return report, err
	}
	run := func(req Request, async bool) error {
		cursor := &Cursor{}
		for attempt := 0; ; attempt++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt > 0 {
				select {
				case <-time.After(o.Backoff * time.Duration(1<<uint(attempt-1))):
				case <-ctx.Done():
					return ctx.Err()
				}
				body, err := resumeBody(req.Body, cursor.LastID, async)
				if err != nil {
					return err
				}
				req.Body = body
				req.Resume = true
			}
			attemptCtx, stop := context.WithTimeout(ctx, o.AttemptTimeout)
			report.Attempts++
			body, err := r.Opener.Open(attemptCtx, req)
			readFailure := false
			if err == nil {
				if body == nil {
					stop()
					return ErrMalformed
				}
				onCancel := context.AfterFunc(attemptCtx, func() { body.Close() })
				reader := newReader(body, o.Limits, budget)
				for {
					event, readErr := reader.Next(attemptCtx)
					if readErr != nil {
						err = readErr
						readFailure = true
						if attemptCtx.Err() != nil {
							err = attemptCtx.Err()
						}
						break
					}
					if event.Name == "SSE_ACK" && !async && ownership.Meta.Local != "" && len(ownership.Messages) > 0 {
						var ack struct {
							Queries []struct {
								Local string `json:"local_message_id"`
							} `json:"query_list"`
							Meta struct {
								Local string `json:"local_conversation_id"`
							} `json:"ack_client_meta"`
						}
						valid := json.Unmarshal(event.Data, &ack) == nil && ack.Meta.Local == ownership.Meta.Local && len(ack.Queries) == len(ownership.Messages)
						seen := map[string]bool{}
						for _, q := range ack.Queries {
							seen[q.Local] = true
						}
						for _, m := range ownership.Messages {
							valid = valid && m.Local != "" && seen[m.Local]
						}
						if !valid {
							err = NoResume(errors.New("doubao_stream_ownership_mismatch"))
							break
						}
					}
					if err = state.Apply(cursor, event, emit); err != nil {
						break
					}
					if cursor.Acknowledged {
						report.Acknowledged = true
					}
					if cursor.Terminal {
						break
					}
				}
				onCancel()
				if closeErr := body.Close(); closeErr != nil {
					err = NoResume(closeErr)
				}
			}
			stop()
			if observer, ok := r.Opener.(interface{ ObserveAttempt(error) }); ok {
				observer.ObserveAttempt(err)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil && cursor.Terminal {
				return nil
			}
			var permanent *permanentError
			if errors.As(err, &permanent) {
				return err
			}
			// Only a known incomplete read is recoverable. HTTP/signature/provider
			// failures and unsupported actions are surfaced without a hidden retry.
			incomplete := readFailure && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrIncomplete) || errors.Is(err, context.DeadlineExceeded))
			if !incomplete {
				return err
			}
			if attempt >= o.MaxResumes || !cursor.Seen || !async && !cursor.Acknowledged {
				return ErrIncomplete
			}
		}
	}
	if err := run(Request{Path: "/chat/completion", Body: append(json.RawMessage(nil), initial...)}, false); err != nil {
		return finish(err)
	}
	for i := 0; i < len(state.Handoffs); i++ {
		h := state.Handoffs[i]
		if authorizer, ok := r.Opener.(interface {
			AuthorizeHandoff(context.Context, Handoff) error
		}); ok {
			if err := authorizer.AuthorizeHandoff(ctx, h); err != nil {
				return finish(err)
			}
		}
		body, err := json.Marshal(map[string]any{"task_id": h.Key, "seq_start": 0, "append_scene": h.Scene, "ext": map[string]any{}})
		if err != nil {
			return finish(ErrMalformed)
		}
		if err = run(Request{Path: "/chat/async/chunk_stream", Body: body}, true); err != nil {
			return finish(err)
		}
	}
	return finish(nil)
}
func resumeBody(raw json.RawMessage, seq int64, async bool) (json.RawMessage, error) {
	var body map[string]json.RawMessage
	if decode(raw, &body) != nil || body == nil {
		return nil, ErrMalformed
	}
	if async {
		b, _ := json.Marshal(seq)
		body["seq_start"] = b
	} else {
		var option map[string]json.RawMessage
		if decode(body["option"], &option) != nil || option == nil {
			return nil, ErrMalformed
		}
		var unique string
		if decode(option["unique_key"], &unique) != nil || unique == "" {
			return nil, ErrMalformed
		}
		b, _ := json.Marshal(seq)
		option["start_seq"] = b
		var recovery map[string]json.RawMessage
		if prior := option["recovery_option"]; prior != nil && decode(prior, &recovery) != nil {
			return nil, ErrMalformed
		}
		if recovery == nil {
			return nil, ErrMalformed
		}
		if recovery["req_create_time_sec"] == nil {
			return nil, ErrMalformed
		}
		recovery["is_recovery"] = json.RawMessage("true")
		option["recovery_option"], _ = json.Marshal(recovery)
		body["option"], _ = json.Marshal(option)
	}
	return json.Marshal(body)
}
