package panel

import (
	"encoding/json"
	"testing"
)

// TestModelsPayloadKeepsZeroCreditsAndProviderErrors 锁定 P1-07：
// 0 倍率（免费）必须保留而不是按假值丢弃；provider_errors 只透传 {provider,code}。
func TestModelsPayloadKeepsZeroCreditsAndProviderErrors(t *testing.T) {
	payload := map[string]any{
		"data": []any{
			map[string]any{"id": "cn:free-model", "credits": float64(0)},
			map[string]any{"id": "cn:paid-model", "credits": "x0.11 credits", "name": "Paid"},
			map[string]any{"id": "cn:unknown-model"},
		},
		"provider_errors": []any{map[string]any{"provider": "doubao", "code": "catalog_unavailable"}},
	}
	out := modelsPayload(payload)
	items, _ := out["items"].([]map[string]any)
	if len(items) != 3 {
		t.Fatalf("items=%d", len(items))
	}
	byID := map[string]map[string]any{}
	for _, item := range items {
		byID[item["id"].(string)] = item
	}
	free, ok := byID["cn:free-model"]["credits"]
	if !ok {
		t.Fatal("0 倍率（免费模型）被丢弃")
	}
	if number, isNum := free.(float64); !isNum || number != 0 {
		t.Fatalf("free credits=%v (%T)", free, free)
	}
	if byID["cn:paid-model"]["credits"] != "x0.11 credits" {
		t.Fatalf("paid credits=%v", byID["cn:paid-model"]["credits"])
	}
	if _, present := byID["cn:unknown-model"]["credits"]; present {
		t.Fatal("未知倍率不能被编造")
	}
	raw, _ := json.Marshal(out)
	if string(raw) == "" {
		t.Fatal("marshal failed")
	}
	errors, _ := out["provider_errors"].([]any)
	if len(errors) != 1 {
		t.Fatalf("provider_errors lost: %v", out["provider_errors"])
	}
}
