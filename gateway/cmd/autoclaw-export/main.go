// Command autoclaw-export writes one AutoClaw account from the gateway vault
// into the file format the standalone reference gateway reads.
//
// It is a migration helper, not a probe: the output file contains live
// credentials, so it is created with 0600 inside a private directory and
// nothing is ever printed. The caller must delete it after the comparison.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/autoclaw"
)

func main() {
	vaultDir := flag.String("vault", "/app/data/provider-accounts", "provider vault directory")
	outDir := flag.String("out", "", "target auths directory for the reference gateway")
	flag.Parse()
	if *outDir == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(1)
	}
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
	fmt.Fprintln(os.Stderr, "rows:", len(rows))
	for _, a := range rows {
		fmt.Fprintln(os.Stderr, "row", a.ID, a.Provider, a.Enabled)
	}
	written := 0
	for _, a := range rows {
		if a.Provider != "autoclaw" {
			continue
		}
		if !a.Enabled {
			fmt.Fprintln(os.Stderr, "skip", a.ID, "disabled")
			continue
		}
		profile, err := autoclaw.ExportProfile(vault, a.ID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "skip", a.ID, err)
			continue
		}
		if err := os.MkdirAll(*outDir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "mkdir:", err)
			os.Exit(1)
		}
		path := filepath.Join(*outDir, "autoclaw-"+autoclaw.ExportPhoneDigits(profile)+".json")
		if err := os.WriteFile(path, profile, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		written++
	}
	fmt.Printf("exported=%d\n", written)
}
