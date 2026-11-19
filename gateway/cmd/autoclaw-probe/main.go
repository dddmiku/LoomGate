// Command autoclaw-probe measures the AutoClaw upstream's time-to-first-byte
// and separates the thinking phase from the answer phase.
//
// It is read-only: it opens the local provider vault, sends one chat request,
// and prints ONLY timings and frame kinds — never a token, phone number, or
// response text.
//
// Why this exists: the gateway reported TTFB 121s while the vendor's own
// desktop client answers in ~9s. Before changing behaviour we need to know
// whether the upstream really is silent for two minutes, or whether it streams
// reasoning immediately and our provider simply emits nothing until the answer
// starts.
//
// Usage:
//
//	autoclaw-probe -vault /app/data/provider-accounts -model zai_glm-5.3-flash
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/autoclaw"
)

func main() {
	vaultDir := flag.String("vault", "/app/data/provider-accounts", "provider vault directory")
	model := flag.String("model", "zai_glm-5.3-flash", "upstream model id")
	prompt := flag.String("prompt", "你好", "user prompt")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall timeout")
	flag.Parse()

	vault, err := accountvault.Open(*vaultDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open vault:", err)
		os.Exit(1)
	}
	defer vault.Close()
	rows, err := vault.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, "list:", err)
		os.Exit(1)
	}
	var account autoclaw.ProbeAccount
	for _, a := range rows {
		if a.Provider != "autoclaw" || !a.Enabled {
			continue
		}
		token, perr := autoclaw.ProbeAccessToken(vault, a.ID)
		if perr != nil {
			continue
		}
		account = autoclaw.ProbeAccount{ID: a.ID, AccessToken: token}
		break
	}
	if account.AccessToken == "" {
		fmt.Fprintln(os.Stderr, "no usable autoclaw account in vault")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	report, err := autoclaw.Probe(ctx, account, *model, *prompt)
	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe error:", err)
		os.Exit(1)
	}
}
