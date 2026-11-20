package autoclaw

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providercatalog"
)

// Source adapts the AutoClaw provider to the console's provider registry.
// AutoClaw publishes a fixed model list, so Catalog is nil: the catalog layer
// answers from StaticModels without touching the network or the vault.
type Source struct {
	Vault *accountvault.Vault
	// Client and BaseURL are optional seams for isolated provider tests. Empty
	// values preserve the production transport and AutoClaw endpoint.
	Client  *http.Client
	BaseURL string
}

func (Source) ID() string           { return "autoclaw" }
func (Source) Name() string         { return "AutoClaw" }
func (Source) CatalogScope() string { return "account" }
func (Source) Inference() bool      { return true }
func (Source) Login() bool          { return true }

func (Source) StaticModels() []providercatalog.Model {
	out := make([]providercatalog.Model, 0, len(Models))
	for _, m := range Models {
		out = append(out, providercatalog.Model{
			ID:                m.ID,
			Name:              m.Name,
			ContextWindow:     m.ContextWindow,
			SupportsMultiTurn: m.MultiTurn,
			Credits:           "1x",
		})
	}
	return out
}

// PublicAccountID projects legacy phone-bearing IDs without rewriting vault
// files or invalidating saved account revisions.
func (s Source) PublicAccountID(id string) string {
	return publicAccountID(s.Vault, id)
}

func publicAccountID(vault *accountvault.Vault, id string) string {
	if strings.HasPrefix(id, "autoclaw-") {
		phone := strings.TrimPrefix(id, "autoclaw-")
		if isPhone(phone) {
			if vault == nil {
				return ""
			}
			return vault.OpaqueID("autoclaw", phone)
		}
	}
	return id
}

// Display exposes the account's phone number only when the stored profile has
// one. It never returns tokens and never decrypts anything the profile does not
// already carry as a display fact.
//
// The number is masked: the account list is a persistent, shoulder-surfable
// page, and the full number is only needed while typing it into the login
// form (where it stays visible for checking). maskPhone keeps enough digits to
// tell two accounts apart without printing a contactable number on screen.
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
	return maskPhone(p.Phone), "", "", time.Time{}, true
}
