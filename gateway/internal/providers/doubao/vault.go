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
	// CompletionTemplate is the official client's own /chat/completion body,
	// captured once and stored encrypted. Only volatile identity fields are
	// rewritten per request, so the protocol shape stays exactly what the
	// official client sends.
	CompletionTemplate json.RawMessage `json:"completion_template,omitempty"`
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
	// A gateway-side login stores only cookies and identity; the protocol body
	// shape comes from the embedded official-client template. A profile that
	// carries its own captured template (legacy capture import) keeps using it.
	completion := p.CompletionTemplate
	if len(completion) == 0 {
		completion = embeddedCompletionTemplate
	}
	return Session{Query: p.Query, Headers: p.Headers, BotID: p.BotID, Runtime: p.Runtime, Revision: fmt.Sprint(account.Revision), Completion: completion}, nil
}
