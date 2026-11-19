// Command doubao-state prints only whether a stored Doubao profile has a pinned
// conversation id, and the id's length. It never prints the id, the Cookie, or
// any token.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
)

func main() {
	vaultDir := flag.String("vault", "/app/data/provider-accounts", "provider vault directory")
	flag.Parse()
	vault, err := accountvault.Open(*vaultDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer vault.Close()
	rows, err := vault.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, "list:", err)
		os.Exit(1)
	}
	for _, a := range rows {
		if a.Provider != "doubao" {
			continue
		}
		_, plain, err := vault.Get(a.ID)
		if err != nil {
			fmt.Printf("account=%s unreadable\n", a.ID)
			continue
		}
		var s doubao.Stored
		_ = json.Unmarshal(plain, &s)
		for i := range plain {
			plain[i] = 0
		}
		fmt.Printf("account=%s enabled=%v conversation_len=%d bot_id_set=%v query_params=%d stored_headers=%d\n",
			a.ID, a.Enabled, len(s.Conversation), s.BotID != "", len(s.Query), len(s.Headers))
	}
}
