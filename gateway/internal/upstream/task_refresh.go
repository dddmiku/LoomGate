package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"workbuddy2api/internal/auth"
)

type TaskRefreshRejected struct{ Code int }

func (e *TaskRefreshRejected) Error() string               { return "credential_refresh_rejected" }
func (e *TaskRefreshRejected) TaskFailure() (string, *int) { return "authentication_required", &e.Code }

// TaskRefresh uses the official refresh contract, a cancelable single request,
// and an explicit code. It does not update memory, disk, or account state.
func (c *Client) TaskRefresh(ctx context.Context, a *auth.Auth) (auth.TokenUpdate, error) {
	if c == nil || c.HTTP == nil || a == nil || !a.IsWorkBuddy() || strings.TrimSpace(a.RefreshToken) == "" {
		return auth.TokenUpdate{}, errors.New("credential_refresh_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return auth.TokenUpdate{}, errors.New("credential_refresh_request_invalid")
	}
	c.RefreshHeaders(req, a)
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return auth.TokenUpdate{}, ctx.Err()
		}
		return auth.TokenUpdate{}, errors.New("credential_refresh_transport_failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	if err != nil || len(raw) > 1<<20 || response.StatusCode != 200 {
		return auth.TokenUpdate{}, errors.New("credential_refresh_response_invalid")
	}
	var envelope struct {
		Code *int `json:"code"`
		Data struct {
			Access  string `json:"accessToken"`
			Refresh string `json:"refreshToken"`
			Expires *int64 `json:"expiresIn"`
			Domain  string `json:"domain"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil {
		return auth.TokenUpdate{}, errors.New("credential_refresh_schema_invalid")
	}
	if *envelope.Code == 12153 {
		return auth.TokenUpdate{}, &TaskRefreshRejected{Code: 12153}
	}
	if *envelope.Code != 0 {
		return auth.TokenUpdate{}, errors.New("credential_refresh_business_failed")
	}
	value := envelope.Data
	if strings.TrimSpace(value.Access) == "" || value.Expires == nil || *value.Expires <= 0 || *value.Expires > 366*24*3600 {
		return auth.TokenUpdate{}, errors.New("credential_refresh_schema_invalid")
	}
	if value.Domain != "" && auth.ResolveRealm("", value.Domain) != auth.ResolveRealm("", a.Domain) {
		return auth.TokenUpdate{}, errors.New("credential_refresh_realm_changed")
	}
	return auth.TokenUpdate{AccessToken: value.Access, RefreshToken: value.Refresh, ExpiresAt: time.Now().Add(time.Duration(*value.Expires) * time.Second).Unix(), Domain: value.Domain}, nil
}
