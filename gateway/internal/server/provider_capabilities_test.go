package server

import (
	"context"
	"testing"

	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/autoclaw"
	"workbuddy2api/internal/providers/doubao"
)

// Client feature selection uses these public catalog fields. Reporting false
// for the real AutoClaw ToolProvider disabled tool use before a call was sent.
func TestProviderCatalogReportsItsImplementedCapabilities(t *testing.T) {
	h := &Handler{cfg: Config{Inference: map[string]inference.Provider{
		"autoclaw": &autoclaw.Provider{},
		"doubao":   &doubao.Provider{},
	}}}
	models, issues := h.providerModels(context.Background())
	if len(issues) != 0 || len(models) != len(autoclaw.Models)+len(doubao.Models) {
		t.Fatal("provider capability catalog was incomplete")
	}
	for _, model := range models {
		tools := model["supports_tool_call"]
		reasoning := model["supports_reasoning"]
		if model["owned_by"] == "autoclaw" {
			if tools != true || reasoning != true {
				t.Fatalf("AutoClaw's tool/reasoning interfaces were hidden: tools=%v reasoning=%v", tools, reasoning)
			}
		} else if tools != false {
			t.Fatal("a provider without ToolProvider advertised tool support")
		}
	}
}
