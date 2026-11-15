// doubao-desktop-inspect verifies a current official desktop identity and model
// catalog. Optional import creates an encrypted binding without exporting login
// cookies. It never sends messages, starts authentication or executes tasks.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
)

type sessions struct{ value doubao.Session }

func (s sessions) Session(context.Context, string) (doubao.Session, error) { return s.value, nil }

func run() error {
	python := flag.String("python", "", "absolute Python executable")
	helper := flag.String("helper", "", "absolute path to tools/doubao-runtime/bridge.py")
	port := flag.Int("port", 0, "official desktop loopback CDP port")
	profile := flag.String("bot-profile", "", "private captured catalog request; only bot_id is read")
	vaultPath := flag.String("import-vault", "", "optional isolated encrypted vault for the new binding")
	flag.Parse()
	runtime, err := doubao.NewDesktopRuntime(*python, *helper, *port)
	if err != nil {
		return err
	}
	f, err := os.Open(*profile)
	if err != nil {
		return errors.New("bot_profile_unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return errors.New("bot_profile_invalid")
	}
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	var captured struct {
		Body string `json:"post_data"`
	}
	var body struct {
		BotID string `json:"bot_id"`
	}
	if json.Unmarshal(raw, &captured) != nil || json.Unmarshal([]byte(captured.Body), &body) != nil || body.BotID == "" {
		return errors.New("bot_profile_invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	binding, err := runtime.Bind(ctx)
	if err != nil {
		return err
	}
	client := doubao.NewWithRuntime(sessions{doubao.Session{Runtime: &binding, BotID: body.BotID, Revision: "desktop-verified"}}, runtime)
	catalog, err := client.Catalog(ctx, "current-desktop", true)
	if err != nil {
		return err
	}
	if *vaultPath != "" {
		vault, err := accountvault.Open(*vaultPath)
		if err != nil {
			return err
		}
		defer vault.Close()
		secret, err := json.Marshal(doubao.StoredProfile{BotID: body.BotID, Runtime: &binding})
		if err != nil {
			return errors.New("binding_encode_failed")
		}
		defer func() {
			for i := range secret {
				secret[i] = 0
			}
		}()
		if _, err = vault.Put(accountvault.Account{ID: "doubao-" + binding.Identity[:20], Provider: "doubao", Enabled: true}, secret, 0); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"identity_verified": true, "fresh_signed_catalog": true, "model_count": len(catalog.Models), "catalog_stale": catalog.Stale, "binding_imported": *vaultPath != "", "mutations": 0})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
