package doubao

// The Doubao work-mode envelope is the whole reason this provider works: an
// incomplete body is accepted by the service and then never answers. These
// tests pin the fields that were missing before, so a refactor cannot quietly
// drop them again.

import (
	"encoding/json"
	"errors"
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

// TestEveryTurnStartsItsOwnConversation pins the detached-session policy.
//
// The protocol always creates a conversation for a chat turn; there is no
// storage-off switch and no delete endpoint. Pinning one id per account only
// meant every caller's prompt went into a single shared conversation in the
// user's Doubao app. Each request must therefore ask to create a fresh one,
// even when the stored profile still carries an old id.
func TestEveryTurnStartsItsOwnConversation(t *testing.T) {
	m := resolve("doubao:deepseek-v4-flash")
	first := decodeBody(t, Stored{Cookie: "synthetic=1"}, m, "hi")
	if first["option"].(map[string]any)["need_create_conversation"] != true {
		t.Fatal("first turn must ask to create a conversation")
	}
	// A profile carrying a conversation id from an older build must not cause
	// reuse: the field is inert now.
	withOldID := decodeBody(t, Stored{Cookie: "synthetic=1", Conversation: "38454286937084162"}, m, "again")
	opt := withOldID["option"].(map[string]any)
	if opt["need_create_conversation"] != true {
		t.Fatal("every turn must create its own conversation, not reuse a stored id")
	}
	if got := withOldID["client_meta"].(map[string]any)["conversation_id"]; got != "" {
		t.Fatalf("stored conversation id was reused: %v", got)
	}
	// Two consecutive requests must not share a local id either.
	second := decodeBody(t, Stored{Cookie: "synthetic=1"}, m, "again")
	a := first["client_meta"].(map[string]any)["local_conversation_id"]
	b := second["client_meta"].(map[string]any)["local_conversation_id"]
	if a == "" || b == "" || a == b {
		t.Fatalf("local_conversation_id must be fresh per request: %v vs %v", a, b)
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
		// The service marks the reply terminal with end_type=3. Without this a
		// closed connection is indistinguishable from a finished answer.
		`event: SSE_REPLY_END` + "\n" + `data: {"end_type":3}`,
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

// The catalog is not a matter of taste: every window and display name here
// comes from the service's own model list, captured in
// D:/LoomGate-work/evidence/doubao-model-catalog-safe.json. Writing a window
// from memory once put 300000 where a model had 256000, which silently changes
// the total_window_size sent upstream and can truncate a long conversation.
func TestCatalogWindowsMatchTheServiceCatalog(t *testing.T) {
	want := map[string]struct {
		window int64
		name   string
	}{
		"auto":                {256000, "自动"},
		"doubao-2.1-lite":     {256000, "豆包 2.1 Lite"},
		"doubao-2.1-pro":      {256000, "豆包 2.1 Pro"},
		"deepseek-v4.1-flash": {300000, "DeepSeek V4.1 Flash"},
		"deepseek-v4-pro":     {300000, "DeepSeek V4 Pro"},
		"kimi-k3":             {300000, "Kimi K3"},
		"kimi-k3.1-pro":       {300000, "Kimi K3.1 Pro"},
		"kimi-k3.1-pro-fast":  {300000, "Kimi K3.1 Pro 极速"},
		"glm-5.3":             {300000, "GLM-5.3"},
		"glm-5.3-flash":       {300000, "GLM-5.3 Flash"},
	}
	if len(Models) != len(want) {
		t.Fatalf("catalog has %d models, evidence records %d", len(Models), len(want))
	}
	for _, m := range Models {
		expected, ok := want[m.ID]
		if !ok {
			t.Errorf("model %q is not in the captured catalog", m.ID)
			continue
		}
		if m.Window != expected.window {
			t.Errorf("%s: window=%d, catalog says %d", m.ID, m.Window, expected.window)
		}
		if m.Name != expected.name {
			t.Errorf("%s: name=%q, catalog says %q", m.ID, m.Name, expected.name)
		}
	}
}

// Pro is plain V4 upstream while Flash is V4.1. Writing the same version for
// both invents a model name the service does not publish.
func TestDeepSeekProIsNotV41(t *testing.T) {
	var flash, pro Model
	for _, m := range Models {
		switch m.ID {
		case "deepseek-v4.1-flash":
			flash = m
		case "deepseek-v4-pro":
			pro = m
		}
	}
	if flash.Name != "DeepSeek V4.1 Flash" {
		t.Errorf("flash name=%q", flash.Name)
	}
	if pro.Name != "DeepSeek V4 Pro" {
		t.Errorf("pro name=%q; the service publishes it as V4 Pro, not V4.1 Pro", pro.Name)
	}
	if flash.Upstream != "person-deepseek-v4-1-flash-261017" || pro.Upstream != "person-deepseek-v4-pro-ga-260813" {
		t.Errorf("upstream ids drifted: flash=%q pro=%q", flash.Upstream, pro.Upstream)
	}
}

// A client still sending the pre-correction id must reach the same model, not
// the default.
func TestLegacyDeepSeekIdStillResolves(t *testing.T) {
	got := resolve("doubao:deepseek-v4-flash")
	if got.ID != "deepseek-v4.1-flash" {
		t.Fatalf("legacy id resolved to %q; it must map onto the corrected model", got.ID)
	}
	if got.Upstream != "person-deepseek-v4-1-flash-261017" {
		t.Fatalf("legacy id reached upstream %q", got.Upstream)
	}
}

// A closed connection is not a finished answer. The desktop client's own code
// proves only SSE_REPLY_END with end_type=3 marks the reply terminal; 1 closes
// one message and 2 closes the answer stage. Reporting a bare EOF as success
// let a mid-reply disconnect look like a complete turn.
func TestReplyEndTypeIsRequiredForSuccess(t *testing.T) {
	cases := []struct {
		name    string
		end     string
		wantErr bool
	}{
		{"terminal_reply_finish", `event: SSE_REPLY_END` + "\n" + `data: {"end_type":3}`, false},
		{"message_finish_is_not_terminal", `event: SSE_REPLY_END` + "\n" + `data: {"end_type":1}`, true},
		{"answer_finish_is_not_terminal", `event: SSE_REPLY_END` + "\n" + `data: {"end_type":2}`, true},
		{"no_reply_end_at_all", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := []string{
				`event: CHUNK_DELTA` + "\n" + `data: {"text":"部分回答"}`,
			}
			if tc.end != "" {
				frames = append(frames, tc.end)
			}
			stream := strings.Join(frames, "\n\n") + "\n\n"
			text, _, _, err := pumpStream(strings.NewReader(stream), func(string) error { return nil })
			if tc.wantErr {
				if !errors.Is(err, ErrStreamTruncated) {
					t.Fatalf("err=%v; a stream without end_type=3 must report truncation", err)
				}
				// The partial text is still returned so the caller can decide
				// what to do with it; it must never be marked complete.
				if text != "部分回答" {
					t.Fatalf("partial text=%q", text)
				}
				return
			}
			if err != nil {
				t.Fatalf("a completed reply reported an error: %v", err)
			}
		})
	}
}
