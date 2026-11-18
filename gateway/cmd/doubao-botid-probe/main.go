// doubao-botid-probe diagnoses why the gateway cannot resolve an account's
// default bot id. It resumes the newest authenticated flow from the encrypted
// vault, issues the read-only startup calls, and reports only structural facts:
// endpoint, HTTP status, top-level keys and how many bot-id-shaped values were
// seen. It never prints cookies, tokens, ids or message content.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
	"workbuddy2api/internal/providers/doubao"
)

func run() error {
	vaultDir := flag.String("vault", "", "provider-accounts vault directory")
	flag.Parse()
	if *vaultDir == "" {
		return fmt.Errorf("vault_required")
	}
	vault, err := accountvault.Open(*vaultDir)
	if err != nil {
		return fmt.Errorf("vault_open_failed")
	}
	defer vault.Close()

	driver := &doubao.Passport{
		Save:  func(context.Context, string, doubao.StoredProfile) error { return nil },
		Trace: summarize,
	}
	service, err := authflow.Open(authflow.Options{Vault: vault, Drivers: map[string]authflow.Driver{"doubao": driver}})
	if err != nil {
		return fmt.Errorf("authentication_service_unavailable")
	}
	rows, err := service.List()
	if err != nil {
		return fmt.Errorf("authentication_state_unavailable")
	}
	var view authflow.View
	found := false
	for _, row := range rows {
		if row.Status == "verified" || row.Status == "manual_action" || row.Status == "uncertain" {
			view, found = row, true
		}
	}
	if !found {
		return fmt.Errorf("resume_flow_not_found")
	}

	// Read-only identity re-check first: proves the session is still valid.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	view, err = service.Step(ctx, view.ID, "resume", authflow.Input{})
	if err != nil {
		return fmt.Errorf("resume_failed: %w", err)
	}
	fmt.Printf("session=%s code=%s\n", view.Status, view.ErrorCode)
	if view.Status != "verified" {
		return nil
	}
	// commit performs the bot-id discovery internally; surface only its outcome.
	view, err = service.Step(ctx, view.ID, "commit", authflow.Input{})
	if err != nil {
		return fmt.Errorf("commit_failed: %w", err)
	}
	fmt.Printf("commit=%s code=%s notice=%s\n", view.Status, view.ErrorCode, view.Notice)
	return nil
}

// summarize prints structural facts about a decoded JSON payload.
func summarize(label string, value any) {
	ids := map[string]struct{}{}
	walk(value, ids)
	keys := make([]string, 0)
	if node, ok := value.(map[string]any); ok {
		for key := range node {
			keys = append(keys, key)
		}
		sort.Strings(keys)
	}
	like := make([]string, 0, len(ids))
	for id := range ids {
		like = append(like, fmt.Sprintf("%d", len(id)))
	}
	sort.Strings(like)
	fmt.Printf("%s keys=%v id_shaped=%d lengths=%v\n", label, keys, len(ids), like)
}

func walk(value any, ids map[string]struct{}) {
	switch node := value.(type) {
	case map[string]any:
		for _, child := range node {
			walk(child, ids)
		}
	case []any:
		for _, child := range node {
			walk(child, ids)
		}
	case string:
		if len(node) >= 15 && len(node) <= 20 {
			numeric := true
			for _, r := range node {
				if r < '0' || r > '9' {
					numeric = false
					break
				}
			}
			if numeric {
				ids[node] = struct{}{}
			}
		}
	case json.Number:
		text := node.String()
		if len(text) >= 15 && len(text) <= 20 {
			ids[text] = struct{}{}
		}
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
