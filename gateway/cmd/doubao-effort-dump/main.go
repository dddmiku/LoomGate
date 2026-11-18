// doubao-effort-dump prints the official reasoning-effort option list per model
// from the cached catalog. Read-only; no generation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
)

func main() {
	vaultDir := flag.String("vault", "", "provider-accounts vault directory")
	account := flag.String("account", "", "account id inside the vault")
	flag.Parse()

	vault, err := accountvault.Open(*vaultDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vault_open_failed")
		os.Exit(1)
	}
	defer vault.Close()

	client := doubao.New(doubao.VaultSessions{Vault: vault}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	catalog, err := client.Catalog(ctx, *account, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "catalog_failed:", err)
		os.Exit(1)
	}

	type row struct {
		ID            string        `json:"id"`
		Name          string        `json:"name"`
		DefaultEffort int           `json:"default_effort"`
		Efforts       []doubao.Effort `json:"efforts"`
	}
	out := make([]row, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		out = append(out, row{m.ID, m.Name, m.DefaultEffort, m.Efforts})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	_ = enc.Encode(out)
}
