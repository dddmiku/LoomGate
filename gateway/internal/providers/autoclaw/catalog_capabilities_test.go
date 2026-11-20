package autoclaw

import "testing"

func TestAccountCatalogKeepsThePublishedMultiTurnCapability(t *testing.T) {
	models := (Source{}).StaticModels()
	if len(models) != len(Models) {
		t.Fatal("account catalog omitted published models")
	}
	for i, model := range models {
		if model.ID != Models[i].ID || model.SupportsMultiTurn != Models[i].MultiTurn {
			t.Fatal("account catalog changed the provider's conversation capability")
		}
	}
}
