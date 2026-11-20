package upstream

import (
	"context"
	"io"
	"log"
	"net/http"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream/trae"
)

const traeAuthenticationRequired = `{"error":{"code":"trae_authentication_required","message":"Trae rejected the stored session after one refresh attempt; sign in again."}}`

func IsTraeAuthenticationRequired(status int, body []byte) bool {
	return status == http.StatusUnauthorized && string(body) == traeAuthenticationRequired
}

func (c *Client) traeChatStreamContext(ctx context.Context, account *auth.Auth, body []byte, clientIP string, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	attempt := func(snapshot *auth.Auth) (io.ReadCloser, int, []byte, error) {
		observeChatAttempt(ctx, "start", 0)
		rc, status, raw, err := trae.ChatStream(ctx, snapshot, body, clientIP, trae.SessionIdentity{CallerID: meta.CallerID, ConversationID: meta.ConversationID})
		if err == nil {
			observeChatAttempt(ctx, "headers", status)
		}
		return rc, status, raw, err
	}
	rejected := account.Snapshot()
	rc, status, raw, err := attempt(rejected)
	if err != nil || rc != nil || !trae.ChatAuthRejected(status, raw) {
		return rc, status, raw, err
	}
	if ctx.Err() != nil {
		return nil, 0, nil, ctx.Err()
	}
	observeChatRetry(ctx, raw)
	// Merge refreshes through the original pool Auth. A response produced by
	// the old token can arrive after another request has already replaced it;
	// in that case reuse the new token without rotating it a second time.
	err = account.RefreshOnceContext(ctx, func(current *auth.Auth) (auth.TokenUpdate, error) {
		if current.AccessToken != rejected.AccessToken {
			return auth.TokenUpdate{AccessToken: current.AccessToken, RefreshToken: current.RefreshToken, ExpiresAt: current.ExpiresAt}, nil
		}
		access, refresh, expiry, err := trae.RefreshContext(ctx, current)
		if err != nil {
			return auth.TokenUpdate{}, err
		}
		return auth.TokenUpdate{AccessToken: access, RefreshToken: refresh, ExpiresAt: expiry}, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, nil, ctx.Err()
		}
		return nil, http.StatusUnauthorized, []byte(traeAuthenticationRequired), nil
	}
	if account.Snapshot().FilePath != "" {
		if err := account.SaveAtomic(); err != nil {
			log.Print("WARN: [trae] refreshed chat credentials could not be persisted")
		}
	}
	trae.InvalidateCatalog(account.Snapshot().UID)
	// Only an HTTP rejection with no returned stream reaches this point. The
	// exact original model and body are reused once; visible output is never
	// replayed and a second auth rejection is terminal.
	rc, status, raw, err = attempt(account.Snapshot())
	if err == nil && rc == nil && trae.ChatAuthRejected(status, raw) {
		return nil, http.StatusUnauthorized, []byte(traeAuthenticationRequired), nil
	}
	return rc, status, raw, err
}
