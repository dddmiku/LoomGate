package doubao

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"time"
)

// The probe gets a cleanup window after caller cancellation. Closing stdin asks
// the helper to abort the owned fetch before detaching CDP; hard kill is last.
func (r *CommandRuntime) invokeProbe(ctx context.Context, request runtimeRequest) (runtimeReply, error) {
	cmd := exec.Command(r.command[0], r.command[1:]...)
	configureDesktopProcess(cmd)
	cmd.Stderr = io.Discard
	input, err := cmd.StdinPipe()
	if err != nil {
		return runtimeReply{}, runtimeError("desktop_unavailable")
	}
	var output boundedOutput
	cmd.Stdout = &output
	if err = cmd.Start(); err != nil {
		input.Close()
		return runtimeReply{}, runtimeError("desktop_unavailable")
	}
	defer input.Close()
	done := make(chan struct{})
	var processErr error
	go func() { processErr = cmd.Wait(); close(done) }()
	raw, _ := json.Marshal(request)
	wrote := make(chan error, 1)
	go func() { _, err := input.Write(append(raw, '\n')); wrote <- err }()
	select {
	case err = <-wrote:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err == nil {
		select {
		case <-done:
			err = processErr
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if ctx.Err() != nil || err != nil {
		input.Close()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(time.Second):
			}
			return runtimeReply{}, runtimeError("desktop_cleanup_uncertain")
		}
		if processErr != nil || output.exceeded {
			return runtimeReply{}, runtimeError("desktop_cleanup_uncertain")
		}
		var terminal runtimeReply
		decoder := json.NewDecoder(&output.buffer)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&terminal) != nil || decoder.Decode(new(any)) != io.EOF {
			return runtimeReply{}, runtimeError("desktop_cleanup_uncertain")
		}
		if terminal.Error == "desktop_cleanup_uncertain" || terminal.Error == "desktop_ownership_lost" {
			return runtimeReply{}, runtimeError(terminal.Error)
		}
		if ctx.Err() != nil {
			return runtimeReply{}, ctx.Err()
		}
		return runtimeReply{}, runtimeError("desktop_cleanup_uncertain")
	}
	if output.exceeded {
		return runtimeReply{}, runtimeError("desktop_protocol_invalid")
	}
	var reply runtimeReply
	decoder := json.NewDecoder(&output.buffer)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&reply) != nil || decoder.Decode(new(any)) != io.EOF {
		return runtimeReply{}, runtimeError("desktop_protocol_invalid")
	}
	if !reply.OK {
		return runtimeReply{}, runtimeError(reply.Error)
	}
	return reply, nil
}
