package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/keylimit"
	"workbuddy2api/internal/requestlog"
)

type incrementalFixture struct {
	fixtureProvider
	run func(context.Context, func(string) error) (inference.Result, error)
}

func (p *incrementalFixture) Stream(ctx context.Context, _, _ string, emit func(string) error) (inference.Result, error) {
	return p.run(ctx, emit)
}
func completedText(text string) inference.Result {
	return inference.Result{Text: text, Attempted: true, Acknowledged: true, Complete: true, AccountRef: "synthetic-ref", AttemptRecords: []inference.Attempt{{StartedAt: time.Now().Add(-time.Millisecond), FinishedAt: time.Now(), HTTPStatus: 200}}}
}

const incrementalBody = `{"model":"fixture:dynamic","messages":[{"role":"user","content":"synthetic"}],"stream":true}`

func requestIncremental(t *testing.T, ctx context.Context, url, key string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/completions", strings.NewReader(incrementalBody))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	return r
}
func TestIncrementalProviderDeliversBeforeCompletionOverRealHTTP(t *testing.T) {
	h, _, key, _ := providerFixture(t, nil)
	release := make(chan struct{})
	defer close(release)
	var completed atomic.Bool
	p := &incrementalFixture{run: func(ctx context.Context, emit func(string) error) (inference.Result, error) {
		if err := emit("first "); err != nil {
			return inference.Result{Attempted: true}, err
		}
		select {
		case <-release:
		case <-ctx.Done():
			return inference.Result{Attempted: true}, ctx.Err()
		}
		if err := emit("second"); err != nil {
			return inference.Result{Attempted: true}, err
		}
		completed.Store(true)
		return completedText("first second"), nil
	}}
	h.cfg.Inference["fixture"] = p
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	response, err := http.DefaultClient.Do(requestIncremental(t, ctx, server.URL, key))
	if err != nil {
		t.Fatal("no response before provider completion")
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, `"content":"first "`) || completed.Load() {
		t.Fatal("first real chunk was buffered until completion")
	}
	if response.Header.Get("X-Gateway-Upstream-Complete") == "true" || response.Header.Get("X-Gateway-Streaming") != "incremental" {
		t.Fatal("stream advertised early completion")
	}
	// Use a second channel close slot to keep cleanup idempotent on failure.
	release <- struct{}{}
	tail, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(tail), `"content":"second"`) || strings.Count(string(tail), "[DONE]") != 1 {
		t.Fatal("stream finalization failed")
	}
	if response.Trailer.Get("X-Gateway-Upstream-Complete") != "true" || response.Trailer.Get("X-Gateway-Stream-Status") != "complete" {
		t.Fatal("verified completion trailers missing")
	}
}
func TestIncrementalProviderFailuresNeverSendSuccessOrLeakErrors(t *testing.T) {
	for _, mode := range []string{"before", "after", "panic", "mismatch", "rewrite"} {
		t.Run(mode, func(t *testing.T) {
			h, _, key, _ := providerFixture(t, nil)
			store, err := requestlog.Open(filepath.Join(t.TempDir(), "requests.jsonl"), requestlog.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			h.cfg.Requests = store
			h.cfg.Inference["fixture"] = &incrementalFixture{run: func(ctx context.Context, emit func(string) error) (inference.Result, error) {
				if mode == "before" {
					return inference.Result{Attempted: true}, errors.New("PRIVATE_SYNTHETIC_ERROR")
				}
				if err := emit("partial"); err != nil {
					return inference.Result{Attempted: true}, err
				}
				if mode == "panic" {
					panic("PRIVATE_SYNTHETIC_PANIC")
				}
				if mode == "mismatch" {
					return completedText("different"), nil
				}
				if mode == "rewrite" {
					return inference.Result{Attempted: true}, inference.ErrStreamRevised
				}
				return inference.Result{Attempted: true}, errors.New("PRIVATE_SYNTHETIC_ERROR")
			}}
			w := providerRequest(h, key, incrementalBody)
			raw := w.Body.String()
			if strings.Contains(raw, "[DONE]") || strings.Contains(raw, `"finish_reason":"stop"`) || strings.Contains(raw, "PRIVATE_SYNTHETIC") || !strings.Contains(raw, `"error"`) {
				t.Fatal("failed stream emitted success or leaked exception")
			}
			if mode == "before" && w.Code != 502 || mode != "before" && w.Code != 200 {
				t.Fatal("failure used wrong HTTP/SSE boundary")
			}
			page, err := store.List(requestlog.Query{})
			if err != nil || page.Total != 1 || page.Items[0].Status == requestlog.StatusSuccess {
				t.Fatal("partial provider failure audited as success")
			}
		})
	}
}
func TestIncrementalDisconnectCancelsAndReleasesAdmission(t *testing.T) {
	h, _, key, _ := providerFixture(t, &keylimit.Policy{MaxConcurrent: 1})
	canceled := make(chan struct{})
	var calls atomic.Int32
	h.cfg.Inference["fixture"] = &incrementalFixture{run: func(ctx context.Context, emit func(string) error) (inference.Result, error) {
		n := calls.Add(1)
		if err := emit("partial"); err != nil {
			return inference.Result{Attempted: true}, err
		}
		if n == 1 {
			<-ctx.Done()
			close(canceled)
			return inference.Result{Attempted: true}, ctx.Err()
		}
		return completedText("partial"), nil
	}}
	handled := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r); handled <- struct{}{} }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := http.DefaultClient.Do(requestIncremental(t, ctx, server.URL, key))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("disconnect did not reach upstream")
	}
	select {
	case <-handled:
	case <-ctx.Done():
		t.Fatal("canceled handler did not return")
	}
	response, err = http.DefaultClient.Do(requestIncremental(t, ctx, server.URL, key))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(raw), "[DONE]") || calls.Load() != 2 {
		t.Fatal("admission slot leaked after canceled stream")
	}
}

type failedFlushWriter struct{ *httptest.ResponseRecorder }

func (w *failedFlushWriter) FlushError() error { return io.ErrClosedPipe }
func TestIncrementalFlushErrorStopsProviderAndDoesNotFallback(t *testing.T) {
	h, _, key, _ := providerFixture(t, nil)
	var sawFailure bool
	p := &incrementalFixture{run: func(ctx context.Context, emit func(string) error) (inference.Result, error) {
		err := emit("partial")
		sawFailure = err != nil
		return completedText("partial"), err
	}}
	h.cfg.Inference["fixture"] = p
	r := requestIncremental(t, context.Background(), "http://localhost", key)
	w := &failedFlushWriter{httptest.NewRecorder()}
	h.ServeHTTP(w, r)
	if !sawFailure || p.calls.Load() != 0 || strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal("flush error did not stop stream or triggered buffered fallback")
	}
}
