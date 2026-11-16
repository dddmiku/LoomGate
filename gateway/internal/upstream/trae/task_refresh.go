package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"workbuddy2api/internal/auth"
)

func (c *TaskClient) Refresh(ctx context.Context, a *auth.Auth) (auth.TokenUpdate, error) {
	if a == nil || a.Realm() != Realm || strings.TrimSpace(a.RefreshToken) == "" {
		return auth.TokenUpdate{}, errors.New("credential_refresh_unavailable")
	}
	body, _ := json.Marshal(exchangeRequest{ClientID: TraeClientID, RefreshToken: a.RefreshToken, ClientSecret: "-", UserID: a.UID})
	defer func() {
		for i := range body {
			body[i] = 0
		}
	}()
	origin := c.Origin
	if origin == "" {
		origin = exchangeHost
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(origin, "/")+exchangePath, bytes.NewReader(body))
	if err != nil {
		return auth.TokenUpdate{}, errors.New("credential_refresh_request_invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Transport: chatTransport()}
	if c.HTTP != nil {
		client = *c.HTTP
	}
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
	var data exchangeResponse
	if json.Unmarshal(raw, &data) != nil || strings.TrimSpace(data.Result.Token) == "" {
		return auth.TokenUpdate{}, errors.New("credential_refresh_schema_invalid")
	}
	stamp, err := strconv.ParseInt(strings.TrimSpace(string(data.Result.TokenExpireAt)), 10, 64)
	if err != nil {
		return auth.TokenUpdate{}, errors.New("credential_refresh_expiry_invalid")
	}
	if stamp > 1e12 {
		stamp /= 1000
	}
	if stamp <= time.Now().Unix() || stamp > time.Now().AddDate(1, 0, 1).Unix() {
		return auth.TokenUpdate{}, errors.New("credential_refresh_expiry_invalid")
	}
	return auth.TokenUpdate{AccessToken: data.Result.Token, RefreshToken: data.Result.RefreshToken, ExpiresAt: stamp}, nil
}
