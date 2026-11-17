package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestACKMustMatchPreparedConversationAndMessage(t *testing.T) {
	initial := json.RawMessage(`{"client_meta":{"local_conversation_id":"synthetic-conversation"},"messages":[{"local_message_id":"synthetic-message"}],"option":{}}`)
	for _, foreign := range []bool{false, true} {
		local := "synthetic-message"
		if foreign {
			local = "foreign"
		}
		payload, _ := json.Marshal(map[string]any{"query_list": []any{map[string]string{"local_message_id": local}}, "ack_client_meta": map[string]string{"local_conversation_id": "synthetic-conversation"}})
		calls := 0
		r := Runner{Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
			calls++
			return io.NopCloser(strings.NewReader(frame("SSE_ACK", 1, string(payload)) + frame("SSE_REPLY_END", 2, `{"end_type":3}`))), nil
		}), Options: Options{MaxResumes: 3}}
		report, err := r.Run(context.Background(), initial, nil)
		if foreign {
			if err == nil || report.Acknowledged || report.ProtocolComplete || calls != 1 {
				t.Fatal("foreign ACK accepted or retried")
			}
		} else if err != nil || !report.Acknowledged || !report.ProtocolComplete {
			t.Fatal("matching ACK rejected")
		}
	}
}


func TestACKRejectsDuplicateLocalOrSharedQuestionOwnership(t *testing.T) {
	initial := json.RawMessage(`{"client_meta":{"local_conversation_id":"synthetic-conversation"},"messages":[{"local_message_id":"synthetic-message"}],"option":{}}`)
	cases := []string{
		`{"query_list":[{"local_message_id":"synthetic-message","question_id":"synthetic-q1"},{"local_message_id":"synthetic-message","question_id":"synthetic-q2"}],"ack_client_meta":{"local_conversation_id":"synthetic-conversation"}}`,
		`{"query_list":[{"local_message_id":"synthetic-message","question_id":"synthetic-q"},{"local_message_id":"synthetic-message-2","question_id":"synthetic-q"}],"ack_client_meta":{"local_conversation_id":"synthetic-conversation"}}`,
	}
	for _, payload := range cases {
		r := Runner{Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(frame("SSE_ACK", 1, payload) + frame("SSE_REPLY_END", 2, `{"end_type":3}`))), nil
		}), Options: Options{MaxResumes: 3}}
		report, err := r.Run(context.Background(), initial, nil)
		if err == nil || report.Acknowledged || report.ProtocolComplete || report.MessageID != "" {
			t.Fatalf("ambiguous ACK accepted: %+v", report)
		}
	}
}

func TestRecoveredACKCannotChangeTheSignedMessageID(t *testing.T) {
	initial := json.RawMessage(`{"client_meta":{"local_conversation_id":"synthetic-conversation"},"messages":[{"local_message_id":"synthetic-message"}],"option":{"unique_key":"synthetic-id","recovery_option":{"req_create_time_sec":123}}}`)
	calls := 0
	r := Runner{Options: Options{MaxResumes: 2, Backoff: time.Millisecond}, Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
		calls++
		switch calls {
		case 1:
			return io.NopCloser(strings.NewReader(frame("SSE_ACK", 1, `{"query_list":[{"local_message_id":"synthetic-message","question_id":"first-q"}],"ack_client_meta":{"local_conversation_id":"synthetic-conversation"}}`))), nil
		default:
			return io.NopCloser(strings.NewReader(frame("SSE_ACK", 1, `{"query_list":[{"local_message_id":"synthetic-message","question_id":"changed-q"}],"ack_client_meta":{"local_conversation_id":"synthetic-conversation"}}`) + frame("SSE_REPLY_END", 2, `{"end_type":3}`))), nil
		}
	})}
	report, err := r.Run(context.Background(), initial, nil)
	if err == nil || report.ProtocolComplete || report.MessageID != "first-q" || calls != 2 {
		t.Fatalf("changed ACK id accepted: err=%v calls=%d report=%+v", err, calls, report)
	}
}

type closeFailure struct{ io.Reader }

func (closeFailure) Close() error { return errors.New("synthetic_cleanup_uncertain") }
func TestCleanupFailureCannotProduceSuccessfulReport(t *testing.T) {
	r := Runner{Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
		return closeFailure{strings.NewReader(frame("SSE_ACK", 1, ack) + frame("SSE_REPLY_END", 2, `{"end_type":3}`))}, nil
	})}
	report, err := r.Run(context.Background(), json.RawMessage(`{"option":{}}`), nil)
	if err == nil || report.ProtocolComplete {
		t.Fatal("cleanup failure reported as completion")
	}
}

type permanentReader struct{ io.Reader }

func (permanentReader) Close() error { return nil }
func (r permanentReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		return n, NoResume(errors.New("synthetic_identity_mismatch"))
	}
	return n, err
}
func TestPermanentTransportIdentityFailureNeverResumes(t *testing.T) {
	calls := 0
	r := Runner{Options: Options{MaxResumes: 3}, Opener: openFunc(func(context.Context, Request) (io.ReadCloser, error) {
		calls++
		return permanentReader{strings.NewReader(frame("SSE_ACK", 1, ack))}, nil
	})}
	_, err := r.Run(context.Background(), json.RawMessage(`{"option":{}}`), nil)
	if err == nil || calls != 1 {
		t.Fatal("permanent transport error resumed")
	}
}
