package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type openFunc func(context.Context, Request) (io.ReadCloser, error)

func (f openFunc) Open(c context.Context, r Request) (io.ReadCloser, error) { return f(c, r) }

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestAsyncHandoffAndResumeUsesSameTaskNoDuplicateText(t *testing.T) {
	requests := []Request{}
	r := Runner{Options: Options{MaxResumes: 1, Backoff: time.Millisecond}, Opener: openFunc(func(c context.Context, req Request) (io.ReadCloser, error) {
		requests = append(requests, req)
		switch len(requests) {
		case 1:
			if req.Path != "/chat/completion" || req.Resume {
				t.Fatal("wrong initial request")
			}
			return io.NopCloser(strings.NewReader(frame("SSE_ACK", 1, ack) + frame("STREAM_MSG_NOTIFY", 2, notify) + frame("STREAM_CHUNK", 3, patch("hello", 1)) + frame("FETCH_STREAM", 4, `{"fetch_type":2,"fetch_key":"synthetic-task","msg_id":"assistant-fixture","append_scene":4}`) + frame("SSE_REPLY_END", 5, `{"end_type":3}`))), nil
		case 2:
			if req.Path != "/chat/async/chunk_stream" || req.Resume {
				t.Fatal("missing async request")
			}
			return io.NopCloser(strings.NewReader(frame("STREAM_CHUNK", 1, patch(" async", 1)))), nil
		case 3:
			var b map[string]any
			json.Unmarshal(req.Body, &b)
			if !req.Resume || b["task_id"] != "synthetic-task" || b["seq_start"] != float64(1) {
				t.Fatal("resume changed identity or sequence")
			}
			return io.NopCloser(strings.NewReader(frame("STREAM_CHUNK", 1, patch(" async", 1)) + frame("CHUNK_DELTA", 2, `{"text":" done"}`) + frame("SSE_REPLY_END", 3, `{"end_type":3}`))), nil
		}
		return nil, ErrMalformed
	})}
	var output strings.Builder
	report, err := r.Run(context.Background(), json.RawMessage(`{"option":{"unique_key":"synthetic-unique","recovery_option":{"req_create_time_sec":1}}}`), func(u Update) error { output.WriteString(u.Text); return nil })
	if err != nil || !report.ProtocolComplete || report.Attempts != 3 || report.Replayed != 1 || output.String() != "hello async done" {
		t.Fatalf("async recovery failed: %v %+v", err, report)
	}
}
func TestInitialUnacknowledgedEOFAndErrorsNeverReplayMutation(t *testing.T) {
	for _, body := range []string{frame("STREAM_MSG_NOTIFY", 1, notify), frame("SSE_ACK", 1, ack) + frame("SSE_REPLY_END", 2, `{"end_type":2}`), frame("DOWNLINK_CMD", 1, `{}`), frame("STREAM_ERROR", 1, `{"error_msg":"synthetic-private"}`)} {
		count := 0
		r := Runner{Options: Options{MaxResumes: 0}, Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
			count++
			return io.NopCloser(strings.NewReader(body)), nil
		})}
		report, err := r.Run(context.Background(), json.RawMessage(`{"option":{}}`), nil)
		if err == nil || report.ProtocolComplete || count != 1 {
			t.Fatal("incomplete/error operation reported success or replayed")
		}
	}
	count := 0
	r := Runner{Options: Options{MaxResumes: 3}, Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
		count++
		return io.NopCloser(strings.NewReader(frame("STREAM_MSG_NOTIFY", 1, notify))), nil
	})}
	_, err := r.Run(context.Background(), json.RawMessage(`{"option":{}}`), nil)
	if err != ErrIncomplete || count != 1 {
		t.Fatal("no ACK but retried initial generation")
	}
}
func TestSynchronousRecoveryPreservesKeyAndOnlyAcknowledgedCursor(t *testing.T) {
	count := 0
	r := Runner{Options: Options{MaxResumes: 1, Backoff: time.Millisecond}, Opener: openFunc(func(c context.Context, req Request) (io.ReadCloser, error) {
		count++
		if count == 1 {
			return io.NopCloser(strings.NewReader(frame("SSE_ACK", 1, ack))), nil
		}
		var b struct {
			Option struct {
				Key      string `json:"unique_key"`
				Seq      int    `json:"start_seq"`
				Recovery struct {
					Yes     bool `json:"is_recovery"`
					Created int  `json:"req_create_time_sec"`
				} `json:"recovery_option"`
			} `json:"option"`
		}
		json.Unmarshal(req.Body, &b)
		if b.Option.Key != "synthetic-id" || b.Option.Seq != 1 || !b.Option.Recovery.Yes || b.Option.Recovery.Created != 123 {
			t.Fatal("recovery envelope changed")
		}
		return io.NopCloser(strings.NewReader(frame("SSE_REPLY_END", 2, `{"end_type":3}`))), nil
	})}
	report, err := r.Run(context.Background(), json.RawMessage(`{"option":{"unique_key":"synthetic-id","recovery_option":{"req_create_time_sec":123}}}`), nil)
	if err != nil || !report.ProtocolComplete || count != 2 {
		t.Fatal("acknowledged resume failed")
	}
}
func TestHTTPRequiresSSEAndCancellationClosesBlockedRead(t *testing.T) {
	for _, kind := range []string{"application/json", "text/html"} {
		closed := false
		h := HTTP{Prepare: func(c context.Context, r Request) (*http.Request, error) { return PreparedJSON(c, r, nil, nil) }, Client: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, Body: &trackedBody{Reader: strings.NewReader(`{}`), close: func() { closed = true }}, Request: r}, nil
		})}}
		_, err := h.Open(context.Background(), Request{Path: "/chat/completion", Body: json.RawMessage(`{}`)})
		if err == nil || !closed {
			t.Fatal("HTTP200 non-stream accepted")
		}
	}
	var closed atomic.Bool
	reader, writer := io.Pipe()
	defer writer.Close()
	r := Runner{Options: Options{Timeout: 30 * time.Millisecond, AttemptTimeout: time.Second}, Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
		return &trackedBody{Reader: reader, close: func() { closed.Store(true); reader.Close() }}, nil
	})}
	_, err := r.Run(context.Background(), json.RawMessage(`{}`), nil)
	if !errors.Is(err, context.DeadlineExceeded) || !closed.Load() {
		t.Fatal("blocked stream not canceled")
	}
}

type trackedBody struct {
	io.Reader
	close func()
}

func (b *trackedBody) Close() error { b.close(); return nil }
func TestActualHTTPTransportStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completion" {
			t.Error("wrong transport path")
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		io.WriteString(w, frame("STREAM_MSG_NOTIFY", 1, notify)+frame("STREAM_CHUNK", 2, patch("synthetic over HTTP", 1))+frame("SSE_REPLY_END", 3, `{"end_type":3}`))
	}))
	defer server.Close()
	origin, _ := url.Parse(server.URL)
	transport := rtFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Scheme = origin.Scheme
		u.Host = origin.Host
		copy.URL = &u
		return http.DefaultTransport.RoundTrip(copy)
	})
	h := HTTP{Client: &http.Client{Transport: transport}, Prepare: func(c context.Context, r Request) (*http.Request, error) { return PreparedJSON(c, r, nil, nil) }}
	report, err := (Runner{Opener: h}).Run(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || !report.ProtocolComplete {
		t.Fatal("real HTTP fixture did not finish")
	}
}

func TestConsumerEOFIsNotRetriedAndCallsAreIsolated(t *testing.T) {
	var calls atomic.Int32
	r := Runner{Options: Options{MaxResumes: 2, Backoff: time.Millisecond}, Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
		calls.Add(1)
		return io.NopCloser(strings.NewReader(frame("SSE_ACK", 1, ack) + frame("STREAM_MSG_NOTIFY", 2, notify) + frame("STREAM_CHUNK", 3, patch("synthetic", 1)) + frame("SSE_REPLY_END", 4, `{"end_type":3}`))), nil
	})}
	report, err := r.Run(context.Background(), json.RawMessage(`{"option":{"unique_key":"synthetic","recovery_option":{"req_create_time_sec":1}}}`), func(Update) error { return io.EOF })
	if err != io.EOF || report.ProtocolComplete || calls.Load() != 1 {
		t.Fatal("consumer termination retried upstream")
	}
	report, err = r.Run(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || !report.ProtocolComplete || calls.Load() != 2 {
		t.Fatal("failed stream contaminated independent request")
	}
}
