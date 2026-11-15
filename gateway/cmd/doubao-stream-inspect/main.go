// Offline validation only. This command never contacts upstream, prints content,
// or writes raw message material. Input paths must refer to private local captures.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
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
	b, _ := json.Marshal(map[string]any{"offline": true, "network_requests": 0, "events": state.Events, "replayed": state.Replayed, "messages": len(state.Messages), "text_update_count": updates, "text_bytes": textBytes, "protocol_terminal": cursor.Terminal, "async_handoffs": len(state.Handoffs), "native_content": state.NativeContent, "error": code, "reward_verified": false})
	fmt.Println(string(b))
	if failure != nil {
		os.Exit(1)
	}
}
