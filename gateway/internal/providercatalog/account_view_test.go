package providercatalog

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
)

type staticAccounts []accountvault.Account

func (s staticAccounts) List() ([]accountvault.Account, error) { return s, nil }

// displaySource is a ProviderSource that reports one verified binding and one
// masked phone, so the view layer's "only non-secret facts" contract can be
// tested without importing a concrete provider package.
type displaySource struct{ verified time.Time }

func (displaySource) ID() string           { return "fixture" }
func (displaySource) Name() string         { return "Fixture" }
func (displaySource) CatalogScope() string { return "account" }
func (displaySource) Inference() bool      { return true }
func (displaySource) Login() bool          { return false }
func (displaySource) Catalog(context.Context, string, bool) (Catalog, error) {
	return Catalog{}, nil
}
func (displaySource) StaticModels() []Model { return nil }
func (d displaySource) Display(id string) (string, string, string, time.Time, bool) {
	if id == "bot-account-a" {
		return "138****8000", "", "bot_abc123", d.verified, true
	}
	return "", "", "", time.Time{}, false
}

func TestAccountViewsExposeOnlyVerifiedNonSecretDisplayFacts(t *testing.T) {
	verified := time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)
	s := New(Options{
		Accounts: staticAccounts{
			{ID: "bot-account-a", Provider: "fixture", Revision: 3, Enabled: true},
			{ID: "bot-account-b", Provider: "fixture", Revision: 1, Enabled: false},
		},
		Sources: []ProviderSource{displaySource{verified: verified}},
	})
	rows, err := s.accounts()
	if err != nil {
		t.Fatal(err)
	}
	views := s.accountViews(rows)
	raw, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, leak := range []string{"ciphertext", "nonce", "headers", "authorization", "cookie", "accesstoken", "refreshtoken"} {
		if strings.Contains(strings.ToLower(text), leak) {
			t.Fatalf("account view leaked %q: %s", leak, text)
		}
	}
	if len(views) != 2 || views[0].ID != "bot-account-a" {
		t.Fatalf("account view shape/order wrong: %+v", views)
	}
	if views[0].BotID != "bot_abc123" || views[0].VerifiedAt == nil || !views[0].VerifiedAt.Equal(verified) {
		t.Fatalf("verified binding facts missing: %+v", views[0])
	}
	if views[0].Mobile != "138****8000" {
		t.Fatalf("masked mobile lost: %+v", views[0])
	}
	if views[1].BotID != "" || views[1].VerifiedAt != nil || views[1].Mobile != "" {
		t.Fatalf("unbound account must not report invented facts: %+v", views[1])
	}
}
