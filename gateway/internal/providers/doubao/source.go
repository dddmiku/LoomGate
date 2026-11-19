package doubao

import (
	"encoding/json"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providercatalog"
)

// Source adapts the Doubao provider to the console's provider registry. The
// catalog is a fixed published list, so no per-account fetch is needed.
type Source struct {
	Vault *accountvault.Vault
}

func (Source) ID() string           { return "doubao" }
func (Source) Name() string         { return "豆包工作" }
func (Source) CatalogScope() string { return "account" }
func (Source) Inference() bool      { return true }
func (Source) Login() bool          { return false }

func (Source) StaticModels() []providercatalog.Model {
	out := make([]providercatalog.Model, 0, len(Models))
	for _, m := range Models {
		out = append(out, providercatalog.Model{
			ID:            "doubao:" + m.ID,
			Name:          m.Name,
			ContextWindow: m.Window,
			Credits:       m.Credits,
		})
	}
	return out
}

// Display reports only whether the account has a stored conversation binding;
// no cookie material and no bot identity are exposed.
// Display reports whether the stored profile can actually make a call. It used
// to require a pinned conversation id, which no longer exists now that every
// request opens its own conversation; a stored session Cookie is what matters.
func (s Source) Display(id string) (mobile, nickname, botID string, verifiedAt time.Time, ok bool) {
	if s.Vault == nil {
		return "", "", "", time.Time{}, false
	}
	_, plain, err := s.Vault.Get(id)
	if err != nil {
		return "", "", "", time.Time{}, false
	}
	defer wipe(plain)
	var p Stored
	if json.Unmarshal(plain, &p) != nil {
		return "", "", "", time.Time{}, false
	}
	if sessionCookie(p) == "" {
		return "", "", "", time.Time{}, false
	}
	return "", "", "", time.Time{}, true
}
