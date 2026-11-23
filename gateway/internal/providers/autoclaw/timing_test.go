package autoclaw

import (
	"strings"
	"testing"
)

func TestAutoClawFirstFrameObservationPrecedesCallbacksAndIgnoresHeartbeat(t *testing.T) {
	calls := 0
	input := " : heartbeat\n: heartbeat\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fixture\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	_, _, _, _, e := decodeSSEObserved(strings.NewReader(input), func(string) error {
		if calls != 1 {
			t.Fatal("first valid upstream frame was observed after downstream delivery")
		}
		return nil
	}, nil, nil, func() { calls++ })
	if e != nil || calls != 1 {
		t.Fatal("upstream first-frame observation was missing or repeated")
	}
	calls = 0
	_, _, _, _, _ = decodeSSEObserved(strings.NewReader(": heartbeat\n\n"), nil, nil, nil, func() { calls++ })
	if calls != 0 {
		t.Fatal("heartbeat was treated as a valid model response")
	}
}
