package autoclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const loginInitializePath = "/userapi/v1/user-info"

// Optional login work must leave time for authflow to persist its result. Both
// initialization and referral binding use this budget instead of consuming the
// entire parent deadline after the usable account has already been saved.
func loginActivityContext(parent context.Context) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(8 * time.Second)
	if limit, ok := parent.Deadline(); ok && limit.Add(-250*time.Millisecond).Before(deadline) {
		deadline = limit.Add(-250 * time.Millisecond)
	}
	return context.WithDeadline(parent, deadline)
}

// InitializeLogin completes the authenticated initialization used by the
// official client. The provider decides whether this login qualifies for any
// current rewards. Success means only that initialization was acknowledged;
// it is not a reward receipt or a promise of a particular wallet balance.
//
// Call only after saving a verified login, or for an explicit, journaled
// backfill. This deliberately does not refresh, retry, create a device, or run
// from balance reads. The authflow commit intent protects its normal caller
// against replay of an interrupted operation. A new normal login may initialize
// again, as in the official client; there is no invented campaign or once flag.
func InitializeLogin(ctx context.Context, profile []byte, client *http.Client) error {
	if ctx.Err() != nil {
		return errors.New("autoclaw_login_initialize_canceled")
	}
	var session stored
	if json.Unmarshal(profile, &session) != nil || strings.TrimSpace(session.AccessToken) == "" || !isDeviceID(session.DeviceID) || session.RefreshPending {
		return errors.New("autoclaw_login_initialize_profile_invalid")
	}
	if session.ExpiresAt > 0 && session.ExpiresAt <= time.Now().Unix() {
		return errors.New("autoclaw_login_initialize_authentication_required")
	}
	// Reserve most of authflow's overall deadline for completing the saved
	// login and its original referral step. Parent cancellation still applies.
	ctx, cancel := loginActivityContext(ctx)
	defer cancel()
	if ctx.Err() != nil {
		return errors.New("autoclaw_login_initialize_canceled")
	}
	payload, _ := json.Marshal(map[string]string{
		"source_id": "autoclaw",
		"device_id": session.DeviceID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiBase+loginInitializePath, bytes.NewReader(payload))
	if err != nil {
		return errors.New("autoclaw_login_initialize_request_invalid")
	}
	setHeaders(req, session.AccessToken, "")
	// Identity services use the official distribution channel. The shared
	// cakegrowth channel belongs to the existing inference/wallet path.
	req.Header.Set("X-Channel", "official")
	// Identity initialization carries the ordinary identity headers, without
	// inference-specific model, agent or conversation context.
	for _, name := range []string{"X-Authorization", "X-Agent-Id", "X-Request-Model", "X-Query-Id", "X-Session-Id", "x_trace_id"} {
		req.Header.Del(name)
	}
	if client == nil {
		client = &http.Client{}
	}
	// Do not mutate a shared client or follow a redirect with this login body.
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := bounded.Do(req)
	if err != nil {
		return errors.New("autoclaw_login_initialize_unconfirmed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("autoclaw_login_initialize_http")
	}
	const limit = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(raw) > limit {
		return errors.New("autoclaw_login_initialize_response_invalid")
	}
	var reply struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.Code == nil {
		return errors.New("autoclaw_login_initialize_response_invalid")
	}
	if *reply.Code != 0 {
		return errors.New("autoclaw_login_initialize_rejected")
	}
	return nil
}
