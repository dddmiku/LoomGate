package stream

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDeltaTargetInvalidatedByRemovalReplacementAndAmbiguity(t *testing.T) {
	for name, e := range map[string]Event{
		"block-delete":    event("STREAM_CHUNK", 3, patch("", 3)),
		"outer-delete":    event("STREAM_CHUNK", 3, `{"message_id":"assistant-fixture","patch_op":[{"patch_object":1,"patch_type":3}]}`),
		"full-empty":      event("FULL_MSG_NOTIFY", 3, `{"message":{"message_id":"assistant-fixture","user_type":2,"content_block":[]}}`),
		"notify-empty":    event("STREAM_MSG_NOTIFY", 3, notify),
		"multiple-ops":    event("STREAM_CHUNK", 3, `{"message_id":"assistant-fixture","patch_op":[{"patch_object":1,"patch_type":1,"patch_value":{"content_block":[{"block_id":"a","block_type":10000,"content":{"text_block":{"text":"A"}}}]}},{"patch_object":50,"patch_type":1}]}`),
		"multiple-blocks": event("STREAM_CHUNK", 3, `{"message_id":"assistant-fixture","patch_op":[{"patch_object":1,"patch_type":1,"patch_value":{"content_block":[{"block_id":"a","block_type":10000,"content":{"text_block":{"text":"A"}}},{"block_id":"b","block_type":10000,"content":{"text_block":{"text":"B"}}}]}}]}`),
		"finished-block":  event("STREAM_CHUNK", 3, `{"message_id":"assistant-fixture","patch_op":[{"patch_object":1,"patch_type":1,"patch_value":{"content_block":[{"block_id":"text-fixture","block_type":10000,"patch_type":2,"is_finish":true,"content":{"text_block":{"text":"done"}}}]}}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			s, c := NewState(Limits{}), &Cursor{}
			for _, e := range []Event{event("STREAM_MSG_NOTIFY", 1, notify), event("STREAM_CHUNK", 2, patch("before", 1)), e} {
				if err := s.Apply(c, e, nil); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := json.Marshal(s.Messages)
			err := s.Apply(c, event("CHUNK_DELTA", 4, `{"text":"must-not-append"}`), nil)
			after, _ := json.Marshal(s.Messages)
			if err != ErrMalformed || string(before) != string(after) {
				t.Fatal("orphan delta mutated state")
			}
			if err := s.Apply(c, event("STREAM_CHUNK", 5, patch("new", 2)), nil); err != nil {
				t.Fatal(err)
			}
			if err := s.Apply(c, event("CHUNK_DELTA", 6, `{"text":" valid"}`), nil); err != nil {
				t.Fatal("valid new delta context lost")
			}
			found := false
			for _, b := range s.Messages["assistant-fixture"].Blocks {
				if b.ID == "text-fixture" {
					var value struct {
						Text string `json:"text"`
					}
					json.Unmarshal(b.Content["text_block"], &value)
					found = value.Text == "new valid"
				}
			}
			if !found {
				t.Fatal("new delta content lost")
			}
		})
	}
}
func TestMetadataBetweenChunkAndDeltaKeepsLiveTarget(t *testing.T) {
	s, c := NewState(Limits{}), &Cursor{}
	for _, e := range []Event{event("STREAM_MSG_NOTIFY", 1, notify), event("STREAM_CHUNK", 2, patch("before", 1)), event("STREAM_CHUNK", 3, `{"message_id":"assistant-fixture","patch_op":[{"patch_object":111,"patch_type":1}]}`), event("CHUNK_DELTA", 4, `{"text":" after"}`)} {
		if err := s.Apply(c, e, nil); err != nil {
			t.Fatal(err)
		}
	}
	if textOf(s) != "before after" {
		t.Fatal("metadata swallowed valid delta")
	}
}
func TestDeltaChecksTargetEvenAcrossSeparateCursors(t *testing.T) {
	s, c := NewState(Limits{}), &Cursor{}
	s.Apply(c, event("STREAM_MSG_NOTIFY", 1, notify), nil)
	s.Apply(c, event("STREAM_CHUNK", 2, patch("before", 1)), nil)
	other := &Cursor{}
	if err := s.Apply(other, event("FULL_MSG_NOTIFY", 1, `{"message":{"message_id":"assistant-fixture","user_type":2,"content_block":[]}}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(c, event("CHUNK_DELTA", 3, `{"text":"orphan"}`), nil); err != ErrMalformed {
		t.Fatal("cross-stream deletion resurrected")
	}
}

func TestTotalBudgetSharedByResumeAsyncAndCRLF(t *testing.T) {
	for _, crlf := range []bool{false, true} {
		for _, async := range []bool{false, true} {
			for _, extra := range []int{-1, 0, 1} {
				first := frame("SSE_ACK", 1, ack) + frame("STREAM_MSG_NOTIFY", 2, notify)
				second := frame("SSE_REPLY_END", 3, `{"end_type":3}`)
				if async {
					first += frame("FETCH_STREAM", 3, `{"fetch_type":2,"fetch_key":"synthetic-task","msg_id":"assistant-fixture","append_scene":4}`) + frame("SSE_REPLY_END", 4, `{"end_type":3}`)
					second = frame("SSE_REPLY_END", 1, `{"end_type":3}`)
				}
				if crlf {
					first = strings.ReplaceAll(first, "\n", "\r\n")
					second = strings.ReplaceAll(second, "\n", "\r\n")
				}
				calls := 0
				r := Runner{Options: Options{Limits: Limits{TotalBytes: len(first) + len(second) + extra}, MaxResumes: 1, Backoff: time.Millisecond}, Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
					calls++
					if calls == 1 {
						return io.NopCloser(strings.NewReader(first)), nil
					}
					return io.NopCloser(strings.NewReader(second)), nil
				})}
				report, err := r.Run(context.Background(), json.RawMessage(`{"option":{"unique_key":"synthetic","recovery_option":{"req_create_time_sec":1}}}`), nil)
				if extra < 0 {
					if err != ErrLimit || report.ProtocolComplete {
						t.Fatalf("budget escaped async=%v crlf=%v", async, crlf)
					}
				} else if err != nil || !report.ProtocolComplete {
					t.Fatalf("exact permitted budget rejected: %v", err)
				}
			}
		}
	}
}
func TestReaderCRLFLastByteAndCommentBudget(t *testing.T) {
	for _, raw := range []string{frame("SSE_REPLY_END", 1, `{"end_type":3}`), ": comment\r\nevent: SSE_REPLY_END\r\nid: 1\r\ndata: {\"end_type\":3}\r\n\r\n"} {
		if _, err := NewReader(strings.NewReader(raw), Limits{TotalBytes: len(raw) - 1}).Next(context.Background()); err != ErrLimit {
			t.Fatal("final newline budget bypass")
		}
		if _, err := NewReader(strings.NewReader(raw), Limits{TotalBytes: len(raw)}).Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
