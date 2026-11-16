// Replays an existing private capture through Runner. No network implementation
// exists here. Only protocol facts, output length and digest leave the process.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/providers/doubao/stream"
)

type capture struct {
	path  string
	opens int
}

func (c *capture) Open(ctx context.Context, r stream.Request) (io.ReadCloser, error) {
	c.opens++
	if c.opens != 1 || r.Path != "/chat/completion" || r.Resume {
		return nil, errors.New("capture_attempt_not_available")
	}
	return os.Open(c.path)
}
func run() error {
	request := flag.String("request", "", "private request capture")
	response := flag.String("response", "", "private SSE capture")
	flag.Parse()
	f, err := os.Open(*request)
	if err != nil {
		return errors.New("input_unavailable")
	}
	defer f.Close()
	var envelope struct {
		Body json.RawMessage `json:"body"`
	}
	if json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&envelope) != nil || len(envelope.Body) == 0 {
		return errors.New("input_invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opener := &capture{path: *response}
	parts := map[string]string{}
	order := []string{}
	report, err := (stream.Runner{Opener: opener}).Run(ctx, envelope.Body, func(u stream.Update) error {
		key := u.MessageID + "/" + u.BlockID
		if u.Operation == 3 {
			delete(parts, key)
			kept := order[:0]
			for _, k := range order {
				if k != key {
					kept = append(kept, k)
				}
			}
			order = kept
			return nil
		}
		if u.Kind != "text" {
			return nil
		}
		if _, ok := parts[key]; !ok {
			order = append(order, key)
		}
		if u.Operation == 1 {
			parts[key] += u.Text
		} else {
			parts[key] = u.Text
		}
		return nil
	})
	if err != nil {
		return errors.New("offline_protocol_failed")
	}
	var text strings.Builder
	for _, k := range order {
		text.WriteString(parts[k])
	}
	sum := sha256.Sum256([]byte(text.String()))
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"offline": true, "network_requests": 0, "acknowledged": report.Acknowledged, "complete": report.ProtocolComplete, "events": report.Events, "attempts": report.Attempts, "reply_bytes": text.Len(), "reply_sha256": hex.EncodeToString(sum[:])})
}
func main() {
	if err := run(); err != nil {
		fmt.Println(`{"result":"FAIL","error":"offline_replay_failed"}`)
		os.Exit(1)
	}
}
