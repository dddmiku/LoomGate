package stream

// Real-capture replay: a normal official /chat/completion stream must yield the
// answer text. This locks the parser's happy path to a captured response so a
// "thousands of events, zero characters" symptom can never be blamed on the
// decoder when the upstream actually answered.
//
// The capture is private operator data kept outside the repository. When it is
// absent the test skips; the synthetic event tests still cover the same contract.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureFile(name string) string {
	if dir := os.Getenv("LOOMGATE_DOUBAO_CAPTURE_DIR"); dir != "" {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	for _, candidate := range []string{
		filepath.Join("D:/LoomGate-work/private/major02-doubao", name),
		filepath.Join("..", "..", "..", "..", "..", "..", "LoomGate-work", "private", "major02-doubao", name),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func TestCapturedAnswerStreamYieldsText(t *testing.T) {
	path := captureFile("native_arithmetic-response.sse")
	if path == "" {
		t.Skip("真实捕获不可用；合成事件测试仍覆盖同一契约")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skip("真实捕获不可读")
	}
	// The capture is a complete event-stream file: parse it directly instead of
	// going through the HTTP opener.
	reader := NewReader(strings.NewReader(string(raw)), Limits{})
	state := NewState(Limits{})
	cursor := &Cursor{}
	var text strings.Builder
	emit := func(update Update) error {
		if update.Kind == "text" {
			text.WriteString(update.Text)
		}
		return nil
	}
	for {
		event, err := reader.Next(context.Background())
		if err != nil {
			break
		}
		if err := state.Apply(cursor, event, emit); err != nil {
			t.Fatalf("apply %s: %v", event.Name, err)
		}
	}
	got := strings.TrimSpace(text.String())
	if got == "" {
		t.Fatal("captured answer stream produced no text; the decoder happy path is broken")
	}
	if !strings.Contains(got, "1785") {
		t.Fatalf("captured answer text missing the expected result: %q", got)
	}
	if state.Events == 0 {
		t.Fatal("no events were applied")
	}
	if !cursor.Acknowledged {
		t.Fatal("captured stream carried an ACK that was not recognized")
	}
}
