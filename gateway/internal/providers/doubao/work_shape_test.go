package doubao

import (
	"encoding/json"
	"testing"
)

func buildForTest(t *testing.T, conversation string) map[string]any {
	t.Helper()
	raw, err := buildCompletionBodyFor(embeddedCompletionTemplate, defaultAssistantBotID,
		"doubao:3:person-deepseek-v4-1-flash-261017", "hello", conversation, 5, true,
		BodyProfile{ContextWindow: 300000, MemoryProfile: "300k_230"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// The reverse proxy must never create a conversation per request. A pinned
// conversation id turns creation off, which is what the official client sends on
// every turn after the first.
func TestPinnedConversationDisablesCreation(t *testing.T) {
	body := buildForTest(t, "38454141791525378")
	meta := body["client_meta"].(map[string]any)
	if meta["conversation_id"] != "38454141791525378" {
		t.Fatalf("conversation_id = %v", meta["conversation_id"])
	}
	if _, ok := meta["local_conversation_id"]; ok {
		t.Fatal("a continuing turn must not send a fresh local_conversation_id")
	}
	option := body["option"].(map[string]any)
	if option["need_create_conversation"] != false {
		t.Fatalf("need_create_conversation = %v, want false", option["need_create_conversation"])
	}
}

// A work-mode model must use the work request shape. Sending the chat shape with
// a work-only model is what made the service fall back to the free default model.
func TestWorkModeUsesWorkRequestShape(t *testing.T) {
	body := buildForTest(t, "38454141791525378")
	option := body["option"].(map[string]any)
	if option["conversation_mode"] != json.Number("2") && option["conversation_mode"] != float64(2) {
		t.Fatalf("conversation_mode = %v, want 2", option["conversation_mode"])
	}
	if _, ok := option["agent_mode"]; ok {
		t.Fatal("work mode must not carry the chat agent_mode")
	}
	if _, ok := option["conversation_init_option"]; ok {
		t.Fatal("work mode must not carry conversation_init_option")
	}
	aggregate := option["aggregate_params"].(map[string]any)
	if aggregate["conversation_mode"] != "2" {
		t.Fatalf("aggregate conversation_mode = %v", aggregate["conversation_mode"])
	}
	if aggregate["agent_mode"] != "" {
		t.Fatalf("aggregate agent_mode = %v, want empty", aggregate["agent_mode"])
	}
	gtp := option["general_task_param"].(map[string]any)
	if gtp["runtime_type"] != json.Number("1") && gtp["runtime_type"] != float64(1) {
		t.Fatalf("general_task_param.runtime_type = %v", gtp["runtime_type"])
	}
	if len(gtp) != 1 {
		t.Fatalf("CloudVM general_task_param must be exactly {runtime_type:1}; got %v", gtp)
	}
	ext := body["ext"].(map[string]any)
	if len(ext) != 3 {
		t.Fatalf("ext has %d keys, want exactly the 3 work keys: %v", len(ext), ext)
	}
	if _, ok := ext["general_task_param"]; !ok {
		t.Fatal("ext.general_task_param missing")
	}
	config := option["model_config"].(map[string]any)
	extra := config["model_extra_params"].(map[string]any)
	if extra["total_window_size"] != "300000" {
		t.Fatalf("total_window_size = %v", extra["total_window_size"])
	}
	if extra["memory_profile"] != "300k_230" {
		t.Fatalf("memory_profile = %v", extra["memory_profile"])
	}
}

// Mode "1" is 对话 and must keep the captured chat shape untouched.
func TestChatModeKeepsChatShape(t *testing.T) {
	raw, err := buildCompletionBodyFor(embeddedCompletionTemplate, defaultAssistantBotID,
		"doubao:1:0", "hello", "38454141791525378", 3, true)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	option := body["option"].(map[string]any)
	if _, ok := option["conversation_init_option"]; !ok {
		t.Fatal("chat mode lost conversation_init_option")
	}
	if _, ok := option["agent_mode"]; !ok {
		t.Fatal("chat mode lost agent_mode")
	}
	if _, ok := body["ext"].(map[string]any)["agent_mode"]; !ok {
		t.Fatal("chat mode lost ext.agent_mode")
	}
}
