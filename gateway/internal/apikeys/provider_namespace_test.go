package apikeys

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderNamespaceBindingsRoundTripExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	store, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	models := []string{"doubao:synthetic-mode:" + strings.Repeat("m", 120), "new-provider:synthetic-model"}
	_, secret, err := store.Create("synthetic", "", models)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	info, ok := reopened.Resolve(secret)
	if !ok || len(info.Models) != 2 || info.Models[0] != models[0] || info.Models[1] != models[1] {
		t.Fatal("provider namespace binding altered")
	}
	for _, bad := range []string{":model", "Bad:model", "provider:", "p:+bad\nvalue", "p:" + strings.Repeat("x", 255)} {
		if _, _, err := store.Create("synthetic", "", []string{bad}); err == nil {
			t.Fatal("invalid binding accepted")
		}
	}
}
