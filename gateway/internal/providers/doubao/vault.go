package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
	"workbuddy2api/internal/accountvault"
)

// StoredProfile is serialized only immediately before encryption or after
// decryption. HTTP response types never embed this private representation.
type StoredProfile struct {
	Query       url.Values      `json:"query"`
	Headers     http.Header     `json:"headers"`
	BotID       string          `json:"bot_id"`
	Runtime     *RuntimeBinding `json:"runtime,omitempty"`
	BindingFlow string          `json:"binding_flow,omitempty"`
	VerifiedAt  time.Time       `json:"verified_at,omitempty"`
}

func (StoredProfile) String() string   { return "doubao.StoredProfile[redacted]" }
func (StoredProfile) GoString() string { return "doubao.StoredProfile[redacted]" }

type VaultSessions struct{ Vault *accountvault.Vault }

func (v VaultSessions) Session(ctx context.Context, id string) (Session, error) {
	if ctx.Err() != nil {
		return Session{}, ctx.Err()
	}
	account, plain, err := v.Vault.Get(id)
	if err != nil {
		return Session{}, err
	}
	defer func() {
		for i := range plain {
			plain[i] = 0
		}
	}()
	if account.Provider != "doubao" || !account.Enabled {
		return Session{}, errors.New("doubao_account_unavailable")
	}
	var p StoredProfile
	if json.Unmarshal(plain, &p) != nil {
		return Session{}, errors.New("doubao_session_invalid")
	}
	return Session{Query: p.Query, Headers: p.Headers, BotID: p.BotID, Runtime: p.Runtime, Revision: fmt.Sprint(account.Revision)}, nil
}
