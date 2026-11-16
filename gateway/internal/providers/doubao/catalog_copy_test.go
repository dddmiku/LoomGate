package doubao

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCatalogCopyRetainsPrivateMetadataWithoutAliasing(t *testing.T) {
	source := Catalog{RuntimeBound: true, FetchedAt: time.Now().UTC(), Models: []Model{{ID: "doubao:synthetic:text", Efforts: []Effort{{Level: 1, Name: "Synthetic"}}, Tiers: []string{"synthetic-tier"}}}}
	copy := copyCatalog(source)
	if !copy.RuntimeBound || copy.FetchedAt != source.FetchedAt {
		t.Fatal("internal metadata lost")
	}
	copy.Models[0].ID = "changed"
	copy.Models[0].Efforts[0].Name = "changed"
	copy.Models[0].Tiers[0] = "changed"
	if source.Models[0].ID != "doubao:synthetic:text" || source.Models[0].Efforts[0].Name != "Synthetic" || source.Models[0].Tiers[0] != "synthetic-tier" {
		t.Fatal("returned models alias cache")
	}
	raw, err := json.Marshal(copy)
	if err != nil || strings.Contains(strings.ToLower(string(raw)), "runtimebound") || strings.Contains(string(raw), "runtime_bound") {
		t.Fatal("private provenance exposed in JSON")
	}
	if copyCatalog(Catalog{}).Models != nil || copyCatalog(Catalog{Models: []Model{}}).Models == nil {
		t.Fatal("nil/empty distinction changed")
	}
}
