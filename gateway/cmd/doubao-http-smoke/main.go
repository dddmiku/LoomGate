// doubao-http-smoke exercises the REAL HTTPTextProvider path end to end against
// a private vault imported from an official-client capture.
//
// It sends exactly one short prompt. Use only when a real generation is
// authorized; the default mode (-dry-run) performs catalog + body build only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/doubao"
)

func run() error {
	vaultDir := flag.String("vault", "", "provider-accounts vault directory")
	account := flag.String("account", "", "account id inside the vault")
	model := flag.String("model", "", "model id, e.g. doubao:1:0")
	prompt := flag.String("prompt", "只回复两个字：收到", "prompt to send")
	effort := flag.String("effort", "", "reasoning effort: low|medium|high|xhigh|max or 3..7")
	live := flag.Bool("live", false, "actually send one generation (consumes quota)")
	flag.Parse()
	if *vaultDir == "" || *account == "" || *model == "" {
		return fmt.Errorf("vault_account_model_required")
	}

	vault, err := accountvault.Open(*vaultDir)
	if err != nil {
		return fmt.Errorf("vault_open_failed: %w", err)
	}
	defer vault.Close()

	client := doubao.New(doubao.VaultSessions{Vault: vault}, nil)
	provider := &doubao.HTTPTextProvider{Vault: vault, Client: client}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	models, err := provider.Models(ctx)
	if err != nil {
		return fmt.Errorf("models_failed: %w", err)
	}
	found := false
	for _, m := range models {
		if m.ID == *model {
			found = true
		}
	}
	out := map[string]any{
		"model_count": len(models),
		"model_known": found,
	}

	if !*live {
		out["mode"] = "dry_run"
		out["sent"] = false
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		return enc.Encode(out)
	}

	delivered := 0
	var result inference.Result
	var err2 error
	if *effort != "" {
		result, err2 = provider.StreamWithEffort(ctx, *model, *prompt, *effort, func(delta string) error {
			delivered += len(delta)
			return nil
		})
	} else {
		result, err2 = provider.Stream(ctx, *model, *prompt, func(delta string) error {
			delivered += len(delta)
			return nil
		})
	}
	out["mode"] = "live"
	out["effort"] = *effort
	out["sent"] = true
	out["attempted"] = result.Attempted
	out["acknowledged"] = result.Acknowledged
	out["complete"] = result.Complete
	out["text_chars"] = len(result.Text)
	out["streamed_chars"] = delivered
	out["events"] = result.Events
	out["attempts"] = result.Attempts
	if err2 != nil {
		out["error"] = err2.Error()
	} else {
		out["error"] = nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	return enc.Encode(out)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
