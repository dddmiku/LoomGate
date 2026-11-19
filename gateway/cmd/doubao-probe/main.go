// Command doubao-probe is a read-only diagnostic: it loads one stored Doubao
// profile, sends the same work-mode body the provider builds, and prints only
// the response envelope (status, content-type, event names, event count). It
// never prints a Cookie, a token, or any message text.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
)

func main() {
	vaultDir := flag.String("vault", "/app/data/provider-accounts", "provider vault directory")
	prompt := flag.String("prompt", "只回复两个字：收到", "prompt to send")
	fresh := flag.Bool("fresh", false, "start a new conversation instead of reusing the stored one")
	flag.Parse()

	vault, err := accountvault.Open(*vaultDir)
	if err != nil {
		fail("open vault: %v", err)
	}
	defer vault.Close()
	rows, err := vault.List()
	if err != nil {
		fail("list vault: %v", err)
	}
	accountID := ""
	for _, a := range rows {
		if a.Provider == "doubao" && a.Enabled {
			accountID = a.ID
			break
		}
	}
	if accountID == "" {
		fail("no enabled doubao account")
	}

	report, err := doubao.ProbeWith(context.Background(), vault, accountID, *prompt, doubao.ProbeSession{Fresh: *fresh})
	if err != nil {
		fail("probe: %v", err)
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
