package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func event(name string, id int64, data string) Event {
	return Event{Name: name, ID: id, HasID: true, Data: json.RawMessage(data)}
}
func frame(name string, id int64, data string) string {
	return fmt.Sprintf("event: %s\nid: %d\ndata: %s\n\n", name, id, data)
}

const notify = `{"meta":{"message_id":"assistant-fixture","user_type":2},"content":{"content_block":[]}}`
const ack = `{"query_list":[{"question_id":"synthetic-question"}],"ack_client_meta":{"conversation_id":"synthetic-conversation"}}`

func patch(text string, operation int) string {
	b, _ := json.Marshal(map[string]any{"message_id": "assistant-fixture", "patch_op": []any{map[string]any{"patch_object": 1, "patch_type": 1, "patch_value": map[string]any{"content_block": []any{map[string]any{"block_id": "text-fixture", "block_type": 10000, "patch_type": operation, "content": map[string]any{"text_block": map[string]any{"text": text}}}}}}}})
	return string(b)
}
func textOf(s *State) string {
	m := s.Messages["assistant-fixture"]
	if len(m.Blocks) == 0 {
		return ""
	}
	var v struct {
		Text string `json:"text"`
	}
	json.Unmarshal(m.Blocks[0].Content["text_block"], &v)
	return v.Text
}
func TestFramingBoundariesAndPartialFrameNeverDispatches(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		r := NewReader(strings.NewReader("\ufeff:comment"+ending+"event: CHUNK_DELTA"+ending+"id: 8"+ending+"data: {"+ending+"data: \"text\":\"合成\"}"+ending+ending), Limits{})
		e, err := r.Next(context.Background())
		if err != nil || e.ID != 8 || e.Name != "CHUNK_DELTA" || !json.Valid(e.Data) {
			t.Fatal("SSE line framing failed")
		}
		if _, err = r.Next(context.Background()); err != io.EOF {
			t.Fatal("missing EOF")
		}
	}
	for _, input := range []string{"event: X\ndata: {}", "event: X\ndata: {}\n"} {
		if _, err := NewReader(strings.NewReader(input), Limits{}).Next(context.Background()); err != ErrIncomplete {
			t.Fatal("unterminated event dispatched")
		}
	}
	if _, err := NewReader(strings.NewReader("data: "+strings.Repeat("x", 40)), Limits{EventBytes: 16}).Next(context.Background()); err != ErrLimit {
		t.Fatal("unbounded line")
	}
	if _, err := NewReader(strings.NewReader("event: X\nid: opaque\ndata: {}\n\n"), Limits{}).Next(context.Background()); err != ErrMalformed {
		t.Fatal("non-numeric Doubao sequence accepted")
	}
}
func TestPatchReplaceDeleteDeltaReplayAndHumanACK(t *testing.T) {
	s, c := NewState(Limits{}), &Cursor{}
	updates := []Update{}
	emit := func(u Update) error { updates = append(updates, u); return nil }
	steps := []Event{event("FULL_MSG_NOTIFY", 1, `{"message":{"message_id":"human-fixture","user_type":1,"content_block":[{"block_id":"human-text","block_type":10000,"content":{"text_block":{"text":"PRIVATE_SYNTHETIC_INPUT"}}}]}}`), event("STREAM_MSG_NOTIFY", 2, notify), event("STREAM_CHUNK", 3, patch("A", 1)), event("CHUNK_DELTA", 4, `{"text":"B"}`), event("CHUNK_DELTA", 4, `{"text":"B"}`)}
	for _, e := range steps {
		if err := s.Apply(c, e, emit); err != nil {
			t.Fatal(err)
		}
	}
	if textOf(s) != "AB" || s.Replayed != 1 || len(updates) != 2 {
		t.Fatal("delta/replay/human handling failed")
	}
	if err := s.Apply(c, event("STREAM_CHUNK", 5, patch("replacement", 2)), emit); err != nil || textOf(s) != "replacement" {
		t.Fatal("replace appended instead")
	}
	if err := s.Apply(c, event("STREAM_CHUNK", 6, patch("", 3)), emit); err != nil || textOf(s) != "" {
		t.Fatal("delete did not clear")
	}
	for _, u := range updates {
		if strings.Contains(u.Text, "PRIVATE_SYNTHETIC_INPUT") {
			t.Fatal("human ACK leaked as assistant content")
		}
	}
}
func TestTerminalAndNativeSemantics(t *testing.T) {
	s, c := NewState(Limits{}), &Cursor{}
	if err := s.Apply(c, event("CHUNK_DELTA", 1, `{"text":"x"}`), nil); err != ErrMalformed {
		t.Fatal("orphan delta accepted")
	}
	for i := 1; i <= 2; i++ {
		if err := s.Apply(c, event("SSE_REPLY_END", int64(i), fmt.Sprintf(`{"end_type":%d}`, i)), nil); err != nil || c.Terminal {
			t.Fatal("partial end became terminal")
		}
	}
	if err := s.Apply(c, event("FETCH_STREAM", 3, `{"fetch_type":2,"fetch_key":"synthetic-task","msg_id":"assistant-fixture","append_scene":4}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(c, event("SSE_REPLY_END", 4, `{"end_type":3}`), nil); err != nil || !c.Terminal || len(s.Handoffs) != 1 {
		t.Fatal("async handoff lost")
	}
	// A control frame carries no answer text. It must not abort the turn: the
	// official client receives these during ordinary chat as well. It is recorded
	// as native content and the stream continues.
	{
		state := NewState(Limits{})
		if err := state.Apply(&Cursor{}, event("DOWNLINK_CMD", 1, `{"command":"synthetic-native-operation"}`), nil); err != nil {
			t.Fatalf("control frame aborted the stream: %v", err)
		}
		if !state.NativeContent {
			t.Fatal("control frame was not recorded as native content")
		}
	}
	if err := NewState(Limits{}).Apply(&Cursor{}, event("STREAM_ERROR", 1, `{"error_msg":"PRIVATE_SYNTHETIC_ERROR"}`), nil); err != ErrUpstream || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("upstream error leaked")
	}
	if err := NewState(Limits{}).Apply(&Cursor{}, event("UNKNOWN_EVENT", 1, `{}`), nil); err != ErrUnsupported {
		t.Fatal("unknown event ignored")
	}
}
func TestSnapshotReplacesAndLimitsAreEnforced(t *testing.T) {
	s, c := NewState(Limits{StateBytes: 32}), &Cursor{}
	if err := s.Apply(c, event("STREAM_MSG_NOTIFY", 1, notify), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(c, event("STREAM_CHUNK", 2, patch(strings.Repeat("x", 50), 1)), nil); err != ErrLimit {
		t.Fatal("unbounded content")
	}
	s = NewState(Limits{})
	c = &Cursor{}
	snapshot := `{"snapshot_seq":10,"next_seq":11,"message_snapshots":[{"meta":{"message_id":"assistant-fixture","user_type":2},"content":{"content_block":[{"block_id":"text-fixture","block_type":10000,"content":{"text_block":{"text":"snapshot"}}}]}}]}`
	if err := s.Apply(c, event("ASYNC_CHUNK_SNAPSHOT", 11, snapshot), nil); err != nil || textOf(s) != "snapshot" {
		t.Fatal("snapshot not applied")
	}
	if err := s.Apply(c, event("ASYNC_CHUNK_SNAPSHOT", 12, `{"snapshot_seq":20,"next_seq":21,"message_snapshots":[]}`), nil); err != nil || len(s.Messages) != 0 || c.LastID != 21 {
		t.Fatal("snapshot failed to clear its stream or advance next_seq")
	}
	if err := s.Apply(c, event("STREAM_MSG_NOTIFY", 22, notify), nil); err != nil {
		t.Fatal(err)
	}
	consumerErr := errors.New("synthetic consumer closed")
	if err := s.Apply(c, event("STREAM_CHUNK", 23, patch("next", 1)), func(Update) error { return consumerErr }); !errors.Is(err, consumerErr) {
		t.Fatal("consumer cancellation lost")
	}
}
