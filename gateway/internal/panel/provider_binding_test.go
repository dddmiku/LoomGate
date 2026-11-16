package panel

import (
	"strings"
	"testing"
)

func TestProviderBindingsAcceptDynamicNamespaceAndBoundLength(t *testing.T) {
	for _, id := range []string{"doubao:synthetic-mode:synthetic-model", "future-provider:some-model", "doubao:" + strings.Repeat("x", 200)} {
		if modelBindingError([]any{id}) != "" {
			t.Fatal("dynamic model rejected")
		}
	}
	for _, id := range []string{"model", "p:", ":model", "Bad:model", "p:" + strings.Repeat("x", 255), "p:x\ny"} {
		if modelBindingError([]any{id}) == "" {
			t.Fatal("invalid model accepted")
		}
	}
}
