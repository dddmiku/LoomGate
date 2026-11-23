package server

import (
	"net/http/httptest"
	"testing"
	"time"
	"workbuddy2api/internal/inference"
)

type delayedProviderWriter struct {
	*httptest.ResponseRecorder
	entered *time.Time
}

func TestProviderTimingUsesObservedFrameWithoutFabricatingMissingTiming(t *testing.T) {
	start := time.Now().Add(-time.Second)
	observed := start.Add(100 * time.Millisecond)
	stat := newChatStat(start, nil, false)
	recordProviderAttempts(stat, inference.Result{FirstByteAt: observed, Attempted: true}, "autoclaw:fixture", nil)
	if stat.ttfb != 100*time.Millisecond {
		t.Fatal("buffered provider lost the actual upstream first-frame timestamp")
	}
	missing := newChatStat(start, nil, false)
	recordProviderAttempts(missing, inference.Result{}, "autoclaw:fixture", nil)
	if missing.ttfb != 0 {
		t.Fatal("completion time was fabricated as TTFB")
	}
}

func (w delayedProviderWriter) Write(raw []byte) (int, error) {
	if w.entered.IsZero() {
		*w.entered = time.Now()
	}
	time.Sleep(140 * time.Millisecond)
	return w.ResponseRecorder.Write(raw)
}

func TestProviderTTFBIsObservedBeforeSlowDownstreamWrite(t *testing.T) {
	start := time.Now().Add(-20 * time.Millisecond)
	stat := newChatStat(start, []byte(`{"stream":true}`), true)
	var entered time.Time
	w := delayedProviderWriter{httptest.NewRecorder(), &entered}
	sink := newProviderStreamSink(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), "autoclaw:fixture", stat)
	if err := sink.emit("fixture"); err != nil {
		t.Fatal(err)
	}
	if stat.ttfb <= 0 || start.Add(stat.ttfb).After(entered) {
		t.Fatal("upstream TTFB incorrectly included downstream write/adapter time")
	}
}
