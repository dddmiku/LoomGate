package doubao

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
	"workbuddy2api/internal/inference"

	"workbuddy2api/internal/providers/doubao/stream"
)

// TextSession owns the runtime's single account slot until Close. Its request
// body is private; it must never be logged or persisted as a fixture.
type TextSession struct {
	runtime       *CommandRuntime
	binding       RuntimeBinding
	Lease         string          `json:"lease"`
	Body          json.RawMessage `json:"body"`
	Model         string          `json:"model"`
	RequestDigest string
	Submitted     bool
	Attempts      []inference.Attempt
	once          sync.Once
	closed        chan struct{}
	closeErr      error
}

func (*TextSession) String() string   { return "doubao.TextSession[redacted]" }
func (*TextSession) GoString() string { return "doubao.TextSession[redacted]" }

func (r *CommandRuntime) PrepareText(ctx context.Context, b RuntimeBinding, model, prompt string) (*TextSession, error) {
	if !identityDigest.MatchString(b.Identity) || !strings.HasPrefix(model, "doubao:") || len(model) > 256 || len(prompt) == 0 || len(prompt) > 65536 {
		return nil, runtimeError("desktop_binding_invalid")
	}
	select {
	case r.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	success := false
	defer func() {
		if !success {
			<-r.slot
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 11*time.Second)
	defer cancel()
	reply, err := r.invoke(ctx, runtimeRequest{Operation: "prepare_text", Port: r.port, Identity: b.Identity, Model: model, Prompt: prompt})
	if err != nil {
		return nil, err
	}
	var data struct {
		Lease string          `json:"lease"`
		Body  json.RawMessage `json:"body"`
		Model string          `json:"model"`
	}
	if json.Unmarshal(reply.Data, &data) != nil || reply.Identity != b.Identity || len(data.Lease) != 36 || data.Model != model || len(data.Body) == 0 || !json.Valid(data.Body) {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	success = true
	return &TextSession{runtime: r, binding: b, Lease: data.Lease, Body: data.Body, Model: model, closed: make(chan struct{})}, nil
}
func (s *TextSession) Close() error {
	s.once.Do(func() {
		close(s.closed)
		defer func() { <-s.runtime.slot }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, s.closeErr = s.runtime.invoke(ctx, runtimeRequest{Operation: "release_text", Port: s.runtime.port, Identity: s.binding.Identity, Lease: s.Lease})
		for i := range s.Body {
			s.Body[i] = 0
		}
	})
	return s.closeErr
}

func (s *TextSession) VerifyIdentity(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	reply, err := s.runtime.invoke(ctx, runtimeRequest{Operation: "identity", Port: s.runtime.port})
	if err != nil {
		return err
	}
	if reply.Identity != s.binding.Identity {
		return runtimeError("desktop_identity_mismatch")
	}
	return nil
}

func (s *TextSession) AuthorizeHandoff(ctx context.Context, h stream.Handoff) error {
	if h.Key == "" || len(h.Key) > 512 || !json.Valid(h.Scene) {
		return stream.ErrMalformed
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := s.runtime.invoke(ctx, runtimeRequest{Operation: "authorize_async", Port: s.runtime.port, Identity: s.binding.Identity, Lease: s.Lease, TaskID: h.Key, Scene: h.Scene})
	return err
}

type streamFrame struct {
	Kind          string `json:"kind"`
	Data          string `json:"data,omitempty"`
	Error         string `json:"error,omitempty"`
	Signed        bool   `json:"signed,omitempty"`
	Status        int    `json:"status,omitempty"`
	RequestDigest string `json:"request_digest,omitempty"`
}
type runtimeBody struct {
	cmd      *exec.Cmd
	input    io.WriteCloser
	output   io.ReadCloser
	scanner  *bufio.Scanner
	pending  []byte
	once     sync.Once
	ended    bool
	bytes    int
	closeErr error
}

func (b *runtimeBody) frame() (streamFrame, error) {
	if !b.scanner.Scan() {
		if b.scanner.Err() != nil {
			return streamFrame{}, stream.NoResume(runtimeError("desktop_stream_limit"))
		}
		return streamFrame{}, io.ErrUnexpectedEOF
	}
	var frame streamFrame
	if json.Unmarshal(b.scanner.Bytes(), &frame) != nil {
		return frame, stream.NoResume(runtimeError("desktop_protocol_invalid"))
	}
	return frame, nil
}
func (b *runtimeBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.ended {
			return 0, io.EOF
		}
		frame, err := b.frame()
		if err != nil {
			return 0, err
		}
		switch frame.Kind {
		case "chunk":
			data, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil || len(data) == 0 || len(data) > 32768 {
				return 0, stream.NoResume(runtimeError("desktop_protocol_invalid"))
			}
			b.bytes += len(data)
			if b.bytes > 32<<20 {
				return 0, stream.NoResume(runtimeError("desktop_stream_limit"))
			}
			b.pending = data
		case "end":
			b.ended = true
			return 0, io.EOF
		case "error":
			if frame.Error == "desktop_stream_interrupted" {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, stream.NoResume(runtimeError(frame.Error))
		default:
			return 0, stream.NoResume(runtimeError("desktop_protocol_invalid"))
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}
func (b *runtimeBody) Close() error {
	b.once.Do(func() {
		_ = b.input.Close()
		// Unblock both a pending Read and a child blocked on a full output pipe.
		_ = b.output.Close()
		wait := make(chan error, 1)
		go func() { wait <- b.cmd.Wait() }()
		select {
		case err := <-wait:
			if err != nil {
				b.closeErr = runtimeError("desktop_cleanup_uncertain")
			}
		case <-time.After(2 * time.Second):
			b.closeErr = runtimeError("desktop_cleanup_uncertain")
			_ = b.cmd.Process.Kill()
			select {
			case <-wait:
			case <-time.After(time.Second):
			}
		}
	})
	return b.closeErr
}
func (s *TextSession) Open(ctx context.Context, req stream.Request) (io.ReadCloser, error) {
	select {
	case <-s.closed:
		return nil, errors.New("desktop_session_closed")
	default:
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if req.Path != "/chat/completion" && req.Path != "/chat/async/chunk_stream" {
		return nil, runtimeError("desktop_origin_rejected")
	}
	port, err := s.runtime.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	payload := struct {
		Operation string          `json:"operation"`
		Port      int             `json:"port"`
		Identity  string          `json:"identity"`
		Lease     string          `json:"lease"`
		Path      string          `json:"path"`
		Body      json.RawMessage `json:"body"`
		Resume    bool            `json:"resume"`
	}{"stream_text", port, s.binding.Identity, s.Lease, req.Path, req.Body, req.Resume}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > 1<<20 {
		return nil, runtimeError("desktop_protocol_invalid")
	}
	cmd := exec.Command(s.runtime.command[0], s.runtime.command[1:]...)
	configureDesktopProcess(cmd)
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, runtimeError("desktop_unavailable")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, runtimeError("desktop_unavailable")
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, runtimeError("desktop_unavailable")
	}
	b := &runtimeBody{cmd: cmd, input: in, output: out, scanner: bufio.NewScanner(out)}
	b.scanner.Buffer(make([]byte, 4096), 128<<10)
	context.AfterFunc(ctx, func() { b.Close() })
	if _, err = in.Write(append(raw, '\n')); err != nil {
		b.Close()
		return nil, runtimeError("desktop_submission_uncertain")
	}
	s.Submitted = true
	s.Attempts = append(s.Attempts, inference.Attempt{StartedAt: time.Now()})
	first, err := b.frame()
	if err != nil {
		b.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if first.Kind == "error" {
		b.Close()
		return nil, runtimeError(first.Error)
	}
	if first.Kind != "start" || first.Status != 200 || !first.Signed {
		b.Close()
		return nil, runtimeError("desktop_protocol_invalid")
	}
	s.Attempts[len(s.Attempts)-1].HTTPStatus = first.Status
	s.RequestDigest = first.RequestDigest
	return b, nil
}

func (s *TextSession) ObserveAttempt(err error) {
	if len(s.Attempts) == 0 {
		return
	}
	record := &s.Attempts[len(s.Attempts)-1]
	if !record.FinishedAt.IsZero() {
		return
	}
	record.FinishedAt = time.Now()
	if err != nil {
		record.ErrorCode = "provider_stream_failed"
		var safe *APIError
		if errors.As(err, &safe) {
			record.ErrorCode = safe.Kind
		}
		if errors.Is(err, context.Canceled) {
			record.ErrorCode = "client_canceled"
		}
	}
}
