package doubao

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// TestBuildCompletionBodyRewritesOnlyVolatileFields proves the captured template
// keeps its official protocol shape. Every non-volatile key must survive
// byte-identical after JSON round-trip.
func TestBuildCompletionBodyRewritesOnlyVolatileFields(t *testing.T) {
	path := os.Getenv("LOOMGATE_DOUBAO_TEMPLATE")
	if path == "" {
		t.Skip("LOOMGATE_DOUBAO_TEMPLATE not set; private capture required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	var original map[string]any
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatalf("template is not JSON: %v", err)
	}

	built, err := buildCompletionBody(raw, "doubao:1:0", "synthetic prompt", "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(built, &got); err != nil {
		t.Fatalf("built body is not JSON: %v", err)
	}

	// Top-level keys must be exactly the captured set.
	for key := range original {
		if _, ok := got[key]; !ok {
			t.Errorf("top-level key %q disappeared", key)
		}
	}
	for key := range got {
		if _, ok := original[key]; !ok {
			t.Errorf("unexpected top-level key %q", key)
		}
	}

	// client_meta: bot_id and local_permissions must be untouched.
	origMeta := original["client_meta"].(map[string]any)
	gotMeta := got["client_meta"].(map[string]any)
	if gotMeta["bot_id"] != origMeta["bot_id"] {
		t.Errorf("bot_id changed")
	}
	if _, ok := gotMeta["local_permissions"]; !ok {
		t.Errorf("local_permissions disappeared")
	}
	if gotMeta["local_conversation_id"] == origMeta["local_conversation_id"] {
		t.Errorf("local_conversation_id was not rotated")
	}

	// messages: exactly one, same block types, prompt replaced.
	origMsg := original["messages"].([]any)[0].(map[string]any)
	gotMsg := got["messages"].([]any)[0].(map[string]any)
	if gotMsg["local_message_id"] == origMsg["local_message_id"] {
		t.Errorf("local_message_id was not rotated")
	}
	origBlock := origMsg["content_block"].([]any)[0].(map[string]any)
	gotBlock := gotMsg["content_block"].([]any)[0].(map[string]any)
	if gotBlock["block_type"] != origBlock["block_type"] {
		t.Errorf("block_type changed")
	}
	if gotBlock["block_id"] == origBlock["block_id"] {
		t.Errorf("block_id was not rotated")
	}
	text := gotBlock["content"].(map[string]any)["text_block"].(map[string]any)["text"]
	if text != "synthetic prompt" {
		t.Errorf("prompt not applied: %v", text)
	}

	// option: model identity rewritten, everything else preserved.
	origOpt := original["option"].(map[string]any)
	gotOpt := got["option"].(map[string]any)
	if gotOpt["unique_key"] == origOpt["unique_key"] {
		t.Errorf("unique_key was not rotated")
	}
	if _, ok := gotOpt["sse_recv_event_options"]; !ok {
		t.Errorf("sse_recv_event_options disappeared")
	}
	if _, ok := gotOpt["general_task_param"]; !ok {
		t.Errorf("general_task_param disappeared")
	}
	if gotOpt["model_config"].(map[string]any)["model_item_key"] != "0" {
		t.Errorf("model_item_key not applied")
	}
	if gotOpt["aggregate_params"].(map[string]any)["mode_id"] != "1" {
		t.Errorf("mode_id not applied")
	}

	// ext block must be untouched.
	origExt := original["ext"].(map[string]any)
	gotExt := got["ext"].(map[string]any)
	for key, want := range origExt {
		if gotExt[key] != want {
			t.Errorf("ext[%q] changed", key)
		}
	}

	// The built body must still be a legal JSON object under the size cap.
	if len(built) > 1<<20 {
		t.Errorf("built body exceeds capture cap: %d", len(built))
	}
}

func TestBuildCompletionBodyRejectsBadInput(t *testing.T) {
	cases := []struct {
		name     string
		template string
		model    string
		prompt   string
	}{
		{"empty template", "", "doubao:1:0", "hi"},
		{"not json", "{", "doubao:1:0", "hi"},
		{"no client_meta", `{"messages":[]}`, "doubao:1:0", "hi"},
		{"empty prompt", `{"client_meta":{},"messages":[{}],"option":{}}`, "doubao:1:0", ""},
		{"bad model", `{"client_meta":{},"messages":[{}],"option":{}}`, "doubao:", "hi"},
		{"model without mode", `{"client_meta":{},"messages":[{}],"option":{}}`, "doubao:1", "hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildCompletionBody(json.RawMessage(tc.template), tc.model, tc.prompt, ""); err == nil {
				t.Fatalf("expected an error")
			}
		})
	}
}

func TestSplitModel(t *testing.T) {
	mode, key, err := splitModel("doubao:1:0")
	if err != nil || mode != "1" || key != "0" {
		t.Fatalf("got %q %q %v", mode, key, err)
	}
	if _, _, err := splitModel("doubao:default:abc"); err != nil {
		t.Fatalf("default mode should be valid: %v", err)
	}
	if _, _, err := splitModel("trae:1:0"); err == nil {
		t.Fatalf("foreign provider model must be rejected")
	}
}

// TestEffortIsWrittenToAllThreeWireFields guards the exact trap this wiring
// exists for: the official request carries reasoning effort THREE times with
// three different JSON types. Writing only one is silently ignored upstream.
func TestEffortIsWrittenToAllThreeWireFields(t *testing.T) {
	path := os.Getenv("LOOMGATE_DOUBAO_TEMPLATE")
	if path == "" {
		t.Skip("LOOMGATE_DOUBAO_TEMPLATE not set; private capture required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	for _, level := range []int{EffortLow, EffortMedium, EffortHigh, EffortVeryHigh, EffortMax} {
		built, err := buildCompletionBodyWithEffort(raw, "doubao:1:0", "x", "", level, true)
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		var body map[string]any
		if err := json.Unmarshal(built, &body); err != nil {
			t.Fatalf("level %d: invalid json", level)
		}
		opt := body["option"].(map[string]any)

		// conversation_init_ext.reasoning_effort must be a STRING
		cie := opt["conversation_init_ext"].(map[string]any)
		if got, ok := cie["reasoning_effort"].(string); !ok || got != strconv.Itoa(level) {
			t.Errorf("level %d: conversation_init_ext.reasoning_effort=%v (%T), want string %q",
				level, cie["reasoning_effort"], cie["reasoning_effort"], strconv.Itoa(level))
		}
		// model_config.reasoning_effort must be a NUMBER
		mc := opt["model_config"].(map[string]any)
		if got, ok := mc["reasoning_effort"].(float64); !ok || int(got) != level {
			t.Errorf("level %d: model_config.reasoning_effort=%v (%T), want number %d",
				level, mc["reasoning_effort"], mc["reasoning_effort"], level)
		}
		// aggregate_params.reasoning_effort must be a STRING
		ap := opt["aggregate_params"].(map[string]any)
		if got, ok := ap["reasoning_effort"].(string); !ok || got != strconv.Itoa(level) {
			t.Errorf("level %d: aggregate_params.reasoning_effort=%v (%T), want string %q",
				level, ap["reasoning_effort"], ap["reasoning_effort"], strconv.Itoa(level))
		}
	}
}

func TestEffortNotTouchedWhenUnset(t *testing.T) {
	path := os.Getenv("LOOMGATE_DOUBAO_TEMPLATE")
	if path == "" {
		t.Skip("LOOMGATE_DOUBAO_TEMPLATE not set; private capture required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	var original map[string]any
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatalf("template: %v", err)
	}
	built, err := buildCompletionBody(raw, "doubao:1:0", "x", "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(built, &got); err != nil {
		t.Fatalf("built: %v", err)
	}
	origOpt := original["option"].(map[string]any)
	gotOpt := got["option"].(map[string]any)
	for _, path := range [][]string{
		{"conversation_init_ext", "reasoning_effort"},
		{"model_config", "reasoning_effort"},
		{"aggregate_params", "reasoning_effort"},
	} {
		o := origOpt[path[0]].(map[string]any)[path[1]]
		g := gotOpt[path[0]].(map[string]any)[path[1]]
		if o != g {
			t.Errorf("%v changed with no effort requested: %v -> %v", path, o, g)
		}
	}
}

func TestParseEffortAndClamp(t *testing.T) {
	cases := map[string]int{
		"low": EffortLow, "低": EffortLow, "3": EffortLow,
		"medium": EffortMedium, "中": EffortMedium, "4": EffortMedium,
		"high": EffortHigh, "高": EffortHigh, "5": EffortHigh,
		"xhigh": EffortVeryHigh, "极高": EffortVeryHigh, "6": EffortVeryHigh,
		"max": EffortMax, "最高": EffortMax, "7": EffortMax,
	}
	for token, want := range cases {
		if got, ok := ParseEffort(token); !ok || got != want {
			t.Errorf("ParseEffort(%q) = %d,%v want %d", token, got, ok, want)
		}
	}
	if _, ok := ParseEffort("bogus"); ok {
		t.Errorf("unknown token must not map")
	}

	// glm-style ladder is non-contiguous 3/5/7. A missing level must downgrade to
	// the highest published level AT OR BELOW the request, never round up.
	ladder := []int{3, 5, 7}
	if got := ClampEffort(4, ladder); got != 3 {
		t.Errorf("ClampEffort(4, 3/5/7) = %d, want 3 (downgrade, not 5)", got)
	}
	if got := ClampEffort(6, ladder); got != 5 {
		t.Errorf("ClampEffort(6, 3/5/7) = %d, want 5 (downgrade, not 7)", got)
	}
	if got := ClampEffort(7, ladder); got != 7 {
		t.Errorf("ClampEffort(7, 3/5/7) = %d, want 7", got)
	}
	if got := ClampEffort(5, ladder); got != 5 {
		t.Errorf("ClampEffort(5, 3/5/7) = %d, want 5", got)
	}
	// The user's exact case: proxy reports max (7) but the model only has 低/中/高
	// (3/4/5). That must resolve to 5 - the model's own highest - not an error.
	if got := ClampEffort(EffortMax, []int{3, 4, 5}); got != EffortHigh {
		t.Errorf("max on a 3/4/5 model = %d, want 5", got)
	}
	// Every supported level above the request: take the lowest (smallest deviation).
	if got := ClampEffort(EffortLow, []int{5, 7}); got != 5 {
		t.Errorf("ClampEffort(3, 5/7) = %d, want 5", got)
	}
	if got := ClampEffort(EffortMax, nil); got != EffortMax {
		t.Errorf("empty ladder must not clamp: %d", got)
	}
	if got := ClampEffort(EffortHigh, []int{5}); got != 5 {
		t.Errorf("single-level ladder = %d, want 5", got)
	}
}
