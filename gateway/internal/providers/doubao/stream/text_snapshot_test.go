package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestObservedReplacementIsAtomicAndReplayIsNotDelivered(t *testing.T) {
	content := frame("SSE_ACK", 1, ack) + frame("STREAM_MSG_NOTIFY", 2, notify) + frame("STREAM_CHUNK", 3, patch("hello", 1)) + frame("STREAM_CHUNK", 3, patch("hello", 1))
	content += frame("FULL_MSG_NOTIFY", 4, `{"message":{"message_id":"assistant-fixture","user_type":2,"content_block":[{"block_id":"text-fixture","block_type":10000,"content":{"text_block":{"text":"hello again"}}}]}}`) + frame("SSE_REPLY_END", 5, `{"end_type":3}`)
	r := Runner{Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content)), nil
	})}
	var last string
	updates := 0
	report, err := r.RunObserved(context.Background(), json.RawMessage(`{}`), func(s TextSnapshot) error {
		if !s.Acknowledged {
			t.Fatal("ACK did not precede snapshots")
		}
		if !strings.HasPrefix(s.Text, last) {
			t.Fatal("replacement exposed intermediate deletion")
		}
		if s.Text != last {
			updates++
		}
		last = s.Text
		return nil
	})
	if err != nil || !report.ProtocolComplete || report.Replayed != 1 || last != "hello again" || updates != 2 {
		t.Fatal("atomic event observation failed")
	}
}
func TestSnapshotOmitsHumanAndRemovedTextAndKeepsBlockOrder(t *testing.T) {
	s := NewState(Limits{})
	cursor := &Cursor{}
	for i, payload := range []string{
		`{"message":{"message_id":"human","user_type":1,"content_block":[{"block_id":"human-text","block_type":10000,"content":{"text_block":{"text":"PRIVATE_SYNTHETIC_INPUT"}}}]}}`,
		`{"message":{"message_id":"assistant","user_type":2,"content_block":[{"block_id":"one","block_type":10000,"content":{"text_block":{"text":"one"}}},{"block_id":"two","block_type":10008,"content":{"code_block":{"code":"two"}}}]}}`,
	} {
		if err := s.Apply(cursor, Event{Name: "FULL_MSG_NOTIFY", ID: int64(i + 1), HasID: true, Data: json.RawMessage(payload)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	text, err := s.textSnapshot(true)
	if err != nil || text.Text != "onetwo" {
		t.Fatal("human content leaked or block order lost")
	}
	patch := `{"message_id":"assistant","patch_op":[{"patch_object":1,"patch_type":1,"patch_value":{"content_block":[{"block_id":"one","block_type":10019,"patch_type":2,"content":{}}]}}]}`
	if err = s.Apply(cursor, Event{Name: "STREAM_CHUNK", ID: 3, HasID: true, Data: json.RawMessage(patch)}, nil); err != nil {
		t.Fatal(err)
	}
	text, err = s.textSnapshot(true)
	if err != nil || text.Text != "two" {
		t.Fatal("nontext replacement retained stale content")
	}
}
func TestObservedFailureCannotTriggerResumeOrSuccessfulCompletion(t *testing.T) {
	calls := 0
	expected := errors.New("synthetic callback stopped")
	r := Runner{Options: Options{MaxResumes: 3}, Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
		calls++
		return io.NopCloser(strings.NewReader(frame("SSE_ACK", 1, ack) + frame("STREAM_MSG_NOTIFY", 2, notify) + frame("STREAM_CHUNK", 3, patch("hello", 1)) + frame("SSE_REPLY_END", 4, `{"end_type":3}`))), nil
	})}
	report, err := r.RunObserved(context.Background(), json.RawMessage(`{}`), func(s TextSnapshot) error {
		if s.Text != "" {
			return expected
		}
		return nil
	})
	if !errors.Is(err, expected) || calls != 1 || report.ProtocolComplete {
		t.Fatal("callback failure resumed or completed")
	}
}
