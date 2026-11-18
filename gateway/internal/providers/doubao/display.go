package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"workbuddy2api/internal/accountvault"
)

// RefreshDisplayFacts re-reads the account's own display facts (masked phone
// number and display name) with a single read-only account-info request and
// stores them on the profile.
//
// It exists because accounts logged in before display facts were captured would
// otherwise keep showing only an internal hash forever. It never sends a login,
// never touches cookies, and never changes the bot id or any session material.
func RefreshDisplayFacts(ctx context.Context, vault *accountvault.Vault, id string) (bool, error) {
	if vault == nil || id == "" {
		return false, errors.New("doubao_account_unavailable")
	}
	account, plain, err := vault.Get(id)
	if err != nil {
		return false, err
	}
	defer func() {
		for i := range plain {
			plain[i] = 0
		}
	}()
	if account.Provider != "doubao" || !account.Enabled {
		return false, errors.New("doubao_account_unavailable")
	}
	var profile StoredProfile
	if json.Unmarshal(plain, &profile) != nil {
		return false, errors.New("doubao_session_invalid")
	}
	if profile.Headers.Get("Cookie") == "" {
		return false, errors.New("doubao_session_invalid")
	}

	query := url.Values{}
	for k, v := range profile.Query {
		query[k] = v
	}
	query.Del("real_aid")
	query.Del("a_bogus")
	query.Del("msToken")
	target := url.URL{Scheme: "https", Host: "accounts.doubao.com", Path: "/passport/account/info/v2/", RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return false, errors.New("doubao_request_invalid")
	}
	req.Header = profile.Headers.Clone()
	req.Header.Set("Cookie", profile.Headers.Get("Cookie"))
	client := http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return false, errors.New("doubao_transport_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, errors.New("doubao_request_rejected")
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if dec.Decode(&envelope) != nil || envelope.Data == nil {
		return false, errors.New("doubao_response_invalid")
	}
	mobile := passportString(envelope.Data, "mobile")
	name := passportString(envelope.Data, "name")
	if name == "" {
		name = passportString(envelope.Data, "screen_name")
	}
	if mobile == profile.Mobile && name == profile.DisplayName {
		return false, nil
	}
	// Only accept a value that is actually masked; a full number would be a
	// credential and must never be persisted or displayed.
	if mobile != "" && !strings.Contains(mobile, "*") {
		mobile = ""
	}
	profile.Mobile = mobile
	profile.DisplayName = name
	encoded, err := json.Marshal(profile)
	if err != nil {
		return false, errors.New("doubao_profile_invalid")
	}
	defer func() {
		for i := range encoded {
			encoded[i] = 0
		}
	}()
	if _, err := vault.Put(accountvault.Account{ID: id, Provider: "doubao", Enabled: true}, encoded, account.Revision); err != nil {
		return false, err
	}
	return true, nil
}
