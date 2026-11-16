package providercatalog

import (
	"context"
	"testing"
	"workbuddy2api/internal/providers/doubao"
)

func TestInferenceRequiresRuntimeBoundFreshPlainTextModel(t *testing.T) {
	for _, bound := range []bool{false, true} {
		for _, stale := range []bool{false, true} {
			s := New(Options{DoubaoInference: true, Doubao: fetchFunc(func(context.Context, string, bool) (doubao.Catalog, error) {
				return doubao.Catalog{RuntimeBound: bound, Stale: stale, Models: []doubao.Model{{ID: "doubao:synthetic:text"}, {ID: "doubao:synthetic:tools", NativeTools: true}}}, nil
			})})
			row := s.fetch(context.Background(), acct("synthetic", true), false)
			if row.Models[0].InferenceAvailable != (bound && !stale) || row.Models[1].InferenceAvailable {
				t.Fatal("catalog overclaimed inference capability")
			}
		}
	}
}
