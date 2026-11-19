package doubao

// The Doubao work-mode envelope is the whole reason this provider works: an
// incomplete body is accepted by the service and then never answers. These
// tests pin the fields that were missing before, so a refactor cannot quietly
// drop them again.

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/inference"
)

func decodeBody(t *testing.T, s Stored, m Model, prompt string) map[string]any {
	t.Helper()
	raw, err := buildBody(s, m, []inference.Message{{Role: "user", Content: prompt}})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		t.Fatal("body is not valid JSON")
	}
	return body
}

// TestWorkModeEnvelopeIsComplete locks every field the service needs before it
// will answer instead of waiting for a runtime.
func TestWorkModeEnvelopeIsComplete(t *testing.T) {
	m := resolve("doubao:deepseek-v4-flash")
	body := decodeBody(t, Stored{Cookie: "synthetic=1", Device: "dev-1"}, m, "hi")
	option := body["option"].(map[string]any)

	// The two values that were wrong before: agent_mode must be 1 (not 2) and
	// the runtime plane must be 2 with a full agent_task_param block.
	if got := numText(option["agent_mode"]); got != "1" {
		t.Fatalf("agent_mode=%v want 1", option["agent_mode"])
	}
	if got := numText(option["conversation_mode"]); got != "2" {
		t.Fatalf("conversation_mode=%v want 2", option["conversation_mode"])
	}
	gtp, ok := option["general_task_param"].(map[string]any)
	if !ok {
		t.Fatal("general_task_param missing")
	}
	if got := numText(gtp["runtime_type"]); got != "2" {
		t.Fatalf("general_task_param.runtime_type=%v want 2", gtp["runtime_type"])
	}
	for _, key := range []string{"action", "thread_local_message_id", "client_option", "agent_task_param", "task_input_json"} {
		if _, ok := gtp[key]; !ok {
			t.Fatalf("general_task_param missing %q", key)
		}
	}
	if _, ok := gtp["client_option"].(map[string]any)["sandbox_id"]; !ok {
		t.Fatal("client_option.sandbox_id missing")
	}
	atp, ok := gtp["agent_task_param"].(map[string]any)
	if !ok {
		t.Fatal("agent_task_param missing")
	}
	for _, key := range []string{"runtime_type", "sandbox_auth_type", "device_name", "local_app_id", "local_device_id"} {
		if _, ok := atp[key]; !ok {
			t.Fatalf("agent_task_param missing %q", key)
		}
	}

	// The work ext block carries the fields the client sends for this mode.
	ext := option["ext"].(map[string]any)
	for _, key := range []string{"general_task_param", "use_deep_think", "agent_mode", "is_finish", "conversation_init_option", "commerce_credit_config_enable"} {
		if _, ok := ext[key]; !ok {
			t.Fatalf("ext missing %q", key)
		}
	}
	if ext["agent_mode"] != "1" {
		t.Fatalf("ext.agent_mode=%v want \"1\"", ext["agent_mode"])
	}

	// Model identity appears in all four places the service reads.
	if option["conversation_init_ext"].(map[string]any)["model_item_key"] != m.Upstream {
		t.Fatal("conversation_init_ext model mismatch")
	}
	if option["model_config"].(map[string]any)["model_item_key"] != m.Upstream {
		t.Fatal("model_config model mismatch")
	}
	if option["aggregate_params"].(map[string]any)["model_item_key"] != m.Upstream {
		t.Fatal("aggregate_params model mismatch")
	}
	if option["aggregate_params"].(map[string]any)["mode_id"] != modeID {
		t.Fatal("aggregate_params mode_id mismatch")
	}
}

// TestFirstTurnCreatesThenPinsConversation is the documented boundary: the
// protocol always creates one conversation, and every later turn reuses it.
func TestFirstTurnCreatesThenPinsConversation(t *testing.T) {
	m := resolve("doubao:deepseek-v4-flash")
	first := decodeBody(t, Stored{Cookie: "synthetic=1"}, m, "hi")
	if first["option"].(map[string]any)["need_create_conversation"] != true {
		t.Fatal("first turn must ask to create the one conversation")
	}
	second := decodeBody(t, Stored{Cookie: "synthetic=1", Conversation: "38454286937084162"}, m, "again")
	opt := second["option"].(map[string]any)
	if opt["need_create_conversation"] != false {
		t.Fatal("later turns must not create another conversation")
	}
	if got := second["client_meta"].(map[string]any)["conversation_id"]; got != "38454286937084162" {
		t.Fatalf("conversation not pinned: %v", got)
	}
	if _, ok := second["client_meta"].(map[string]any)["local_conversation_id"]; !ok {
		t.Fatal("local_conversation_id must stay present in this protocol")
	}
}

// TestSSEExtractionCoversBothDeltaShapes covers the two ways the service
// delivers text, and proves CHUNK_DELTA suppresses the duplicated snapshot.
// Frames are written as raw strings because a frame is `event:` followed
// immediately by `data:` in the same block; joining those two lines as
// separate frames would test a stream shape the service never sends.
func TestSSEExtractionCoversBothDeltaShapes(t *testing.T) {
	frames := []string{
		"event: SSE_ACK\ndata: {\"ack_client_meta\":{\"conversation_id\":\"38454286937084162\"}}",
		"event: STREAM_CHUNK\ndata: {\"patch_op\":[{\"patch_value\":{\"content_block\":[{\"block_type\":10000,\"content\":{\"text_block\":{\"text\":\"7\"}}}]}}]}",
		"event: CHUNK_DELTA\ndata: {\"text\":\"8\"}",
		"event: CHUNK_DELTA\ndata: {\"text\":\"5\"}",
	}
	stream := strings.Join(frames, "\n\n") + "\n\n"
	var got []string
	text, conv, events, err := pumpStream(strings.NewReader(stream), func(s string) error {
		got = append(got, s)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "785" || strings.Join(got, "") != "785" {
		t.Fatalf("text=%q deltas=%v", text, got)
	}
	if conv != "38454286937084162" {
		t.Fatalf("conversation not captured: %q", conv)
	}
	if events != 3 {
		t.Fatalf("events=%d want 3", events)
	}
}

// TestUnknownModelFallsBackToDefault keeps a typo from becoming a hard failure.
func TestUnknownModelFallsBackToDefault(t *testing.T) {
	if got := resolve("doubao:not-a-model").ID; got != defaultModel {
		t.Fatalf("fallback=%q want %q", got, defaultModel)
	}
	if got := resolve("deepseek-v4-pro").ID; got != "deepseek-v4-pro" {
		t.Fatalf("exact model not matched: %q", got)
	}
}

// TestModelIDsAreNamespaced keeps the public catalog consistent with routing.
func TestModelIDsAreNamespaced(t *testing.T) {
	p := &Provider{}
	models, err := p.Models(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != len(Models) {
		t.Fatalf("got %d models want %d", len(models), len(Models))
	}
	for _, m := range models {
		if !strings.HasPrefix(m.ID, "doubao:") {
			t.Fatalf("model id missing namespace: %q", m.ID)
		}
	}
}

func numText(v any) string {
	switch n := v.(type) {
	case float64:
		b, _ := json.Marshal(n)
		return strings.TrimSuffix(string(b), ".0")
	case int:
		b, _ := json.Marshal(n)
		return string(b)
	case string:
		return n
	}
	return ""
}
