package doubao

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/providers/doubao/stream"
)

func TestTextPipeChild(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--text-fixture" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadBytes('\n')
	var in map[string]json.RawMessage
	if json.Unmarshal(line, &in) != nil {
		os.Exit(2)
	}
	var operation, identity string
	_ = json.Unmarshal(in["operation"], &operation)
	_ = json.Unmarshal(in["identity"], &identity)
	enc := json.NewEncoder(os.Stdout)
	if operation == "identity" {
		_ = enc.Encode(runtimeReply{OK: true, Identity: strings.Repeat("a", 64)})
		os.Exit(0)
	}
	if operation == "prepare_text" {
		var model string
		_ = json.Unmarshal(in["model"], &model)
		data, _ := json.Marshal(map[string]any{"lease": "00000000-0000-4000-8000-000000000000", "model": model, "body": map[string]any{"option": map[string]any{"unique_key": "synthetic-session"}}})
		_ = enc.Encode(runtimeReply{OK: true, Identity: identity, Data: data})
		os.Exit(0)
	}
	if operation == "release_text" {
		_ = enc.Encode(runtimeReply{OK: true, Identity: identity})
		os.Exit(0)
	}
	_ = enc.Encode(streamFrame{Kind: "start", Status: 200, Signed: mode != "unsigned"})
	switch mode {
	case "protocol", "protocol-rewrite", "protocol-noack":
		body := ""
		if mode != "protocol-noack" {
			body += "event: SSE_ACK\nid: 1\ndata: {\"query_list\":[{}],\"ack_client_meta\":{\"synthetic\":true}}\n\n"
		}
		body += "event: STREAM_MSG_NOTIFY\nid: 2\ndata: {\"meta\":{\"message_id\":\"synthetic-assistant\",\"user_type\":2},\"content\":{\"content_block\":[{\"block_id\":\"text\",\"block_type\":10000,\"content\":{\"text_block\":{\"text\":\"first\"}}}]}}\n\n"
		tail := "first second"
		if mode == "protocol-rewrite" {
			tail = "changed"
		}
		body += fmt.Sprintf("event: FULL_MSG_NOTIFY\nid: 3\ndata: {\"message\":{\"message_id\":\"synthetic-assistant\",\"user_type\":2,\"content_block\":[{\"block_id\":\"text\",\"block_type\":10000,\"content\":{\"text_block\":{\"text\":%q}}}]}}\n\n", tail)
		body += "event: SSE_REPLY_END\nid: 4\ndata: {\"end_type\":3}\n\n"
		_ = enc.Encode(streamFrame{Kind: "chunk", Data: base64.StdEncoding.EncodeToString([]byte(body))})
		_ = enc.Encode(streamFrame{Kind: "end"})
	case "success":
		_ = enc.Encode(streamFrame{Kind: "chunk", Data: base64.StdEncoding.EncodeToString([]byte("synthetic stream"))})
		_ = enc.Encode(streamFrame{Kind: "end"})
	case "secret-error":
		_ = enc.Encode(streamFrame{Kind: "error", Error: "PRIVATE_SYNTHETIC_EXCEPTION"})
	case "large":
		fmt.Fprintln(os.Stdout, strings.Repeat("x", 200<<10))
	case "wait":
		_, _ = reader.ReadBytes('\n')
	}
	os.Exit(0)
}
func textFixture(t *testing.T, mode string) *CommandRuntime {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewCommandRuntime([]string{exe, "-test.run=^TestTextPipeChild$", "--", "--text-fixture", mode}, 19226)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestTextPipeCancellationBudgetAndSlotRelease(t *testing.T) {
	for _, mode := range []string{"success", "unsigned", "secret-error", "large", "wait"} {
		t.Run(mode, func(t *testing.T) {
			r := textFixture(t, mode)
			s, err := r.PrepareText(context.Background(), RuntimeBinding{strings.Repeat("a", 64)}, "doubao:synthetic:model", "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			start := time.Now()
			body, err := s.Open(ctx, stream.Request{Path: "/chat/completion", Body: s.Body})
			if err == nil {
				var raw []byte
				raw, err = io.ReadAll(body)
				body.Close()
				if mode == "success" && string(raw) != "synthetic stream" {
					t.Fatal("stream data lost")
				}
			}
			if mode != "success" && err == nil {
				t.Fatal("invalid stream accepted")
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE_SYNTHETIC") {
				t.Fatal("child error leaked")
			}
			s.Close()
			if time.Since(start) > 4*time.Second {
				t.Fatal("cancellation was unbounded")
			}
			ctx2, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			again, err := r.PrepareText(ctx2, RuntimeBinding{strings.Repeat("a", 64)}, "doubao:synthetic:model", "synthetic")
			if err != nil {
				t.Fatal("slot leaked")
			}
			again.Close()
		})
	}
}
