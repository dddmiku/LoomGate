package panel

// The key model picker reads /api/models -> modelsPayload -> items[].credits.
// The gateway already returns credits on each model entry; this test proves the
// panel projection preserves it for the exact shape the gateway emits, so a
// "倍率未知" badge can only mean the gateway did not send a value.

import "testing"

func TestModelsPayloadPreservesGatewayCreditsShape(t *testing.T) {
	// Verbatim shape of one /v1/models row as the gateway emits it today.
	payload := map[string]any{
		"data": []any{
			map[string]any{
				"id":                    "doubao:deepseek-v4-flash",
				"object":                "model",
				"created":               float64(0),
				"owned_by":              "doubao",
				"name":                  "DeepSeek V4.1 Flash",
				"context_length":        float64(300000),
				"credits":               "0.01x",
				"streaming_mode":        "incremental",
				"supports_multi_turn":   true,
				"single_turn_text_only": false,
			},
		},
	}
	out := modelsPayload(payload)
	items, _ := out["items"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("items=%d", len(items))
	}
	if got := items[0]["credits"]; got != "0.01x" {
		t.Fatalf("credits=%v (%T), want \"0.01x\"", got, got)
	}
	if got := items[0]["name"]; got != "DeepSeek V4.1 Flash" {
		t.Fatalf("name=%v", got)
	}
}
