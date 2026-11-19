package autoclaw

import (
	"encoding/json"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providercatalog"
)

// Source adapts the AutoClaw provider to the console's provider registry.
// AutoClaw publishes a fixed model list, so Catalog is nil: the catalog layer
// answers from StaticModels without touching the network or the vault.
type Source struct {
	Vault *accountvault.Vault
}

func (Source) ID() string           { return "autoclaw" }
func (Source) Name() string         { return "AutoClaw" }
func (Source) CatalogScope() string { return "account" }
func (Source) Inference() bool      { return true }
func (Source) Login() bool          { return false }

func (Source) StaticModels() []providercatalog.Model {
	out := make([]providercatalog.Model, 0, len(Models))
	for _, m := range Models {
		out = append(out, providercatalog.Model{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.ContextWindow,
		})
	}
	return out
}

// Display exposes the account's phone number only when the stored profile has
// one. It never returns tokens and never decrypts anything the profile does not
// already carry as a display fact.
func (s Source) Display(id string) (mobile, nickname, botID string, verifiedAt time.Time, ok bool) {
	if s.Vault == nil {
		return "", "", "", time.Time{}, false
	}
	_, plain, err := s.Vault.Get(id)
	if err != nil {
		return "", "", "", time.Time{}, false
	}
	defer wipe(plain)
	var p struct {
		Phone string `json:"phone"`
	}
	if json.Unmarshal(plain, &p) != nil || p.Phone == "" {
		return "", "", "", time.Time{}, false
	}
	return p.Phone, "", "", time.Time{}, true
}
