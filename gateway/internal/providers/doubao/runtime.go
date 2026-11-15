package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

const catalogPath = "/alice/slot/action_bar_v3/brief_list"

// RuntimeBinding contains an identity digest, never a reusable login credential.
// It lives inside the encrypted account record; account switches fail closed.
type RuntimeBinding struct {
	Identity string `json:"identity"`
}

func (RuntimeBinding) String() string   { return "doubao.RuntimeBinding[redacted]" }
func (RuntimeBinding) GoString() string { return "doubao.RuntimeBinding[redacted]" }

type CatalogRuntime interface {
	Catalog(context.Context, RuntimeBinding, string) (json.RawMessage, error)
}

// CommandRuntime runs the restricted, local desktop helper without a shell or
// network listener. One process per read keeps cancellation and output bounded.
// The helper never exports cookies, signed URLs or SDK request headers.
type CommandRuntime struct {
	command []string
	port    int
	slot    chan struct{}
}

func NewCommandRuntime(command []string, port int) (*CommandRuntime, error) {
	if len(command) == 0 || !filepath.IsAbs(command[0]) || port < 1 || port > 65535 {
		return nil, errors.New("desktop_configuration_invalid")
	}
	return &CommandRuntime{command: append([]string(nil), command...), port: port, slot: make(chan struct{}, 1)}, nil
}

func NewDesktopRuntime(python, helper string, port int) (*CommandRuntime, error) {
	if !filepath.IsAbs(helper) {
		return nil, errors.New("desktop_configuration_invalid")
	}
	info, err := os.Stat(helper)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 {
		return nil, errors.New("desktop_configuration_invalid")
	}
	return NewCommandRuntime([]string{python, "-I", "-B", helper}, port)
}

type runtimeRequest struct {
	Operation string `json:"operation"`
	Port      int    `json:"port"`
	Identity  string `json:"identity,omitempty"`
	BotID     string `json:"bot_id,omitempty"`
}
type runtimeReply struct {
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Identity string          `json:"identity,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Signed   bool            `json:"signed,omitempty"`
}

var identityDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var runtimeBotID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (r *CommandRuntime) Bind(ctx context.Context) (RuntimeBinding, error) {
	reply, err := r.run(ctx, runtimeRequest{Operation: "identity", Port: r.port})
	if err != nil {
		return RuntimeBinding{}, err
	}
	if !identityDigest.MatchString(reply.Identity) {
		return RuntimeBinding{}, &APIError{Kind: "desktop_protocol_invalid"}
	}
	return RuntimeBinding{Identity: reply.Identity}, nil
}
func (r *CommandRuntime) Catalog(ctx context.Context, b RuntimeBinding, bot string) (json.RawMessage, error) {
	if !identityDigest.MatchString(b.Identity) || !runtimeBotID.MatchString(bot) {
		return nil, &APIError{Kind: "desktop_binding_invalid"}
	}
	reply, err := r.run(ctx, runtimeRequest{Operation: "catalog", Port: r.port, Identity: b.Identity, BotID: bot})
	if err != nil {
		return nil, err
	}
	if reply.Identity != b.Identity || !reply.Signed || len(reply.Data) == 0 {
		return nil, &APIError{Kind: "desktop_protocol_invalid"}
	}
	return reply.Data, nil
}

// Only these fixed codes cross the process boundary; upstream exception text,
// child stderr and process arguments are never included in errors or logs.
func runtimeError(code string) error {
	switch code {
	case "desktop_unavailable", "desktop_version_unsupported", "desktop_identity_mismatch", "desktop_login_required", "desktop_timeout", "desktop_busy", "desktop_protocol_invalid", "desktop_upstream_rejected":
		return &APIError{Kind: code}
	default:
		return &APIError{Kind: "desktop_protocol_invalid"}
	}
}

type boundedOutput struct {
	buffer   bytes.Buffer
	exceeded bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	if w.buffer.Len()+len(p) > 2<<20 {
		w.exceeded = true
		return 0, errors.New("output_limit")
	}
	return w.buffer.Write(p)
}
func (r *CommandRuntime) run(ctx context.Context, request runtimeRequest) (runtimeReply, error) {
	ctx, cancel := context.WithTimeout(ctx, 11*time.Second)
	defer cancel()
	select {
	case r.slot <- struct{}{}:
		defer func() { <-r.slot }()
	case <-ctx.Done():
		return runtimeReply{}, ctx.Err()
	}
	if ctx.Err() != nil {
		return runtimeReply{}, ctx.Err()
	}
	raw, _ := json.Marshal(request)
	cmd := exec.CommandContext(ctx, r.command[0], r.command[1:]...)
	configureDesktopProcess(cmd)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 500 * time.Millisecond
	var output boundedOutput
	cmd.Stdout = &output
	err := cmd.Run()
	if ctx.Err() != nil {
		return runtimeReply{}, ctx.Err()
	}
	if output.exceeded {
		return runtimeReply{}, &APIError{Kind: "desktop_protocol_invalid"}
	}
	if err != nil {
		return runtimeReply{}, &APIError{Kind: "desktop_unavailable"}
	}
	var reply runtimeReply
	d := json.NewDecoder(&output.buffer)
	d.DisallowUnknownFields()
	if d.Decode(&reply) != nil || d.Decode(new(any)) != io.EOF {
		return runtimeReply{}, &APIError{Kind: "desktop_protocol_invalid"}
	}
	if !reply.OK {
		return runtimeReply{}, runtimeError(reply.Error)
	}
	return reply, nil
}

// NewWithRuntime preserves the legacy profile transport for legacy accounts.
// Runtime-bound accounts never silently fall back to captured HTTP credentials.
func NewWithRuntime(s Sessions, runtime CatalogRuntime) *Client {
	c := New(s, nil)
	c.runtime = runtime
	return c
}
