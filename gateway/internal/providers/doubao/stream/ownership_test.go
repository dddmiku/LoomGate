package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
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
