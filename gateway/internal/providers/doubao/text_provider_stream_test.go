package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
)

func TestTextProviderRealChildStreamAcknowledgementRevisionAndCancellation(t *testing.T) {
	for _, mode := range []string{"protocol", "protocol-rewrite", "protocol-noack", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			v, err := accountvault.Open(filepath.Join(t.TempDir(), "accounts"))
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			raw, _ := json.Marshal(StoredProfile{Runtime: &RuntimeBinding{Identity: strings.Repeat("a", 64)}, BotID: "synthetic-bot"})
			account, err := v.Put(accountvault.Account{ID: "synthetic-account", Provider: "doubao", Enabled: true}, raw, 0)
			if err != nil {
				t.Fatal(err)
			}
			childMode := mode
			if mode == "disabled" {
				childMode = "protocol"
			}
			runtime := textFixture(t, childMode)
			p := &TextProvider{Vault: v, Runtime: runtime}
			var chunks []string
			result, err := p.Stream(context.Background(), "doubao:synthetic:model", "synthetic", func(text string) error {
				chunks = append(chunks, text)
				if mode == "disabled" {
					_, failure := v.SetEnabled(account.ID, false, account.Revision)
					if failure != nil {
						t.Fatal(failure)
					}
				}
				return nil
			})
			if mode == "protocol" {
				if err != nil || len(chunks) != 2 || strings.Join(chunks, "") != result.Text || !result.Complete || !result.Acknowledged {
					t.Fatal("real child pipeline lost incremental text")
				}
			}
			if mode == "protocol-rewrite" && (!errors.Is(err, inference.ErrStreamRevised) || len(chunks) != 1) {
				t.Fatal("rewrite of delivered text did not fail")
			}
			if mode == "protocol-noack" && (err == nil || len(chunks) != 0) {
				t.Fatal("unacknowledged text escaped to caller")
			}
			if mode == "disabled" && (err == nil || len(chunks) != 1) {
				t.Fatal("account disable did not stop later chunks")
			}
			if !result.Attempted || len(result.AttemptRecords) != 1 || len(runtime.slot) != 0 {
				t.Fatal("attempt audit or runtime slot cleanup failed")
			}
		})
	}
}
