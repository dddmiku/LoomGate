package doubao

import (
	"errors"
	"io"
	"strings"
	"testing"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/doubao/stream"
)

func TestTextProgressAcknowledgementAndAtomicPrefix(t *testing.T) {
	var chunks []string
	p := textProgress{emit: func(s string) error { chunks = append(chunks, s); return nil }}
	for _, s := range []stream.TextSnapshot{{Text: "private before ACK"}, {Text: "你好", Acknowledged: true}, {Text: "你好", Acknowledged: true}, {Text: "你好 世界", Acknowledged: true}} {
		if err := p.apply(s); err != nil {
			t.Fatal(err)
		}
	}
	if len(chunks) != 2 || strings.Join(chunks, "") != "你好 世界" {
		t.Fatal("unacknowledged text, duplicate, or prefix lost")
	}
	for _, text := range []string{"你好", "changed", ""} {
		if err := p.apply(stream.TextSnapshot{Text: text, Acknowledged: true}); !errors.Is(err, inference.ErrStreamRevised) {
			t.Fatal("retracted text was silently appended")
		}
	}
	buffered := textProgress{}
	for _, text := range []string{"old", "new", ""} {
		if err := buffered.apply(stream.TextSnapshot{Text: text, Acknowledged: true}); err != nil {
			t.Fatal(err)
		}
	}
	if buffered.text != "" {
		t.Fatal("buffered completion retained deleted text")
	}
}
func TestTextProgressDownstreamFailureDoesNotAdvance(t *testing.T) {
	p := textProgress{emit: func(string) error { return io.ErrClosedPipe }}
	if err := p.apply(stream.TextSnapshot{Text: "synthetic", Acknowledged: true}); !errors.Is(err, io.ErrClosedPipe) || p.delivered != "" {
		t.Fatal("callback failure was hidden")
	}
}
