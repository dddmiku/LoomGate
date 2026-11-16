// Offline validation only. This command never contacts upstream, prints content,
// or writes raw message material. Input paths must refer to private local captures.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
	"workbuddy2api/internal/providers/doubao/stream"
)

func main() {
	path := flag.String("file", "", "private existing SSE capture")
	flag.Parse()
	if *path == "" {
		fmt.Println(`{"error":"input_required"}`)
		os.Exit(2)
	}
	file, err := os.Open(*path)
	if err != nil {
		fmt.Println(`{"error":"input_unavailable"}`)
		os.Exit(1)
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reader := stream.NewReader(file, stream.Limits{})
	state := stream.NewState(stream.Limits{})
	cursor := &stream.Cursor{}
	updates, textBytes := 0, 0
	var failure error
	for {
		e, err := reader.Next(ctx)
		if err == io.EOF {
			if !cursor.Terminal {
				failure = stream.ErrIncomplete
			}
			break
		}
		if err != nil {
			failure = err
			break
		}
		err = state.Apply(cursor, e, func(u stream.Update) error { updates++; textBytes += len(u.Text); return nil })
		if err != nil {
			failure = err
			break
		}
		if cursor.Terminal {
			break
		}
	}
	code := ""
	if failure != nil {
		code = failure.Error()
	}
	// Inspect the final assistant state; FULL_MSG_NOTIFY may only refer to the
	// human message. Never print the response itself or assume a final snapshot.
	var final strings.Builder
	for _, id := range state.Order {
		message := state.Messages[id]
		if message.UserType != 2 {
			continue
		}
		for _, block := range message.Blocks {
			key, field := "", ""
			switch block.Type {
			case 10000:
				key, field = "text_block", "text"
			case 10008:
				key, field = "code_block", "code"
			default:
				continue
			}
			var value map[string]json.RawMessage
			if json.Unmarshal(block.Content[key], &value) != nil {
				continue
			}
			var text string
			if json.Unmarshal(value[field], &text) == nil {
				final.WriteString(text)
			}
		}
	}
	b, _ := json.Marshal(map[string]any{"offline": true, "network_requests": 0, "events": state.Events, "replayed": state.Replayed, "messages": len(state.Messages), "text_update_count": updates, "text_bytes": textBytes, "protocol_terminal": cursor.Terminal, "async_handoffs": len(state.Handoffs), "native_content": state.NativeContent, "error": code, "reward_verified": false, "final_text_characters": utf8.RuneCountInString(final.String()), "final_text_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(final.String())))})
	fmt.Println(string(b))
	if failure != nil {
		os.Exit(1)
	}
}
