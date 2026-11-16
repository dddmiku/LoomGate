// Package doubao implements the official Doubao desktop/work service protocol.
// Catalogs and campaign identifiers are fetched at runtime, never compiled here.
package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Session is supplied by the secure account store. Do not serialize or log it.
// Query contains the current official client's version/device parameters.
type Session struct {
	Query    url.Values
	Headers  http.Header
	BotID    string
	Revision string
	Runtime  *RuntimeBinding
}

func (Session) String() string   { return "doubao.Session[redacted]" }
func (Session) GoString() string { return "doubao.Session[redacted]" }

type Sessions interface {
	Session(context.Context, string) (Session, error)
}
type Client struct {
	sessions Sessions
	http     *http.Client
	mu       sync.Mutex
	cache    map[string]*catalogCache
	runtime  CatalogRuntime
}
type APIError struct {
	HTTPStatus int
	Code       int
	Kind       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("doubao %s (http=%d, code=%d)", e.Kind, e.HTTPStatus, e.Code)
}
func (e *APIError) PublicCode() string { return e.Kind }
func (e *APIError) TaskFailure() (string, *int) {
	switch e.Kind {
	case "desktop_upstream_rejected":
		code := e.Code
		return "provider_rejected", &code
	case "desktop_checkin_unconfirmed", "desktop_submission_uncertain", "desktop_cleanup_uncertain":
		return "operation_unconfirmed", nil
	case "desktop_identity_mismatch", "desktop_binding_invalid":
		return "account_identity_changed", nil
	case "login_required", "desktop_login_required":
		return "authentication_required", nil
	case "desktop_task_expired":
		return "task_expired", nil
	case "desktop_task_not_claimable", "desktop_task_instance_missing", "desktop_task_manual":
		return "operation_not_eligible", nil
	case "desktop_busy":
		return "provider_busy", nil
	default:
		return "upstream_failed", nil
	}
}
func New(s Sessions, transport http.RoundTripper) *Client {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{sessions: s, http: &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, cache: map[string]*catalogCache{}}
}
func (c *Client) call(ctx context.Context, account, path string, body any) (json.RawMessage, error) {
	s, err := c.sessions.Session(ctx, account)
	if err != nil {
		if canceled := contextFailure(ctx, err); canceled != nil {
			return nil, canceled
		}
		return nil, &APIError{Kind: "login_required"}
	}
	return c.callSession(ctx, s, path, body)
}
func (c *Client) callSession(ctx context.Context, s Session, path string, body any) (json.RawMessage, error) {
	if s.Runtime != nil {
		if path != catalogPath {
			return nil, &APIError{Kind: "desktop_operation_unsupported"}
		}
		if c.runtime == nil {
			return nil, &APIError{Kind: "desktop_unavailable"}
		}
		return c.runtime.Catalog(ctx, *s.Runtime, s.BotID)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, &APIError{Kind: "invalid_request"}
	}
	u := url.URL{Scheme: "https", Host: "www.doubao.com", Path: path, RawQuery: s.Query.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(raw))
	if err != nil {
		return nil, &APIError{Kind: "invalid_request"}
	}
	req.Header = s.Headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	for _, k := range []string{"Content-Length", "Host", "Connection", "Accept-Encoding"} {
		req.Header.Del(k)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if canceled := contextFailure(ctx, err); canceled != nil {
			return nil, canceled
		}
		return nil, &APIError{Kind: "transport_failed"}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil {
		if canceled := contextFailure(ctx, err); canceled != nil {
			return nil, canceled
		}
		return nil, &APIError{HTTPStatus: resp.StatusCode, Kind: "response_read_failed"}
	}
	if len(data) > 8<<20 {
		return nil, &APIError{HTTPStatus: resp.StatusCode, Kind: "response_limit"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{HTTPStatus: resp.StatusCode, Kind: "http_error"}
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Code == nil {
		return nil, &APIError{HTTPStatus: resp.StatusCode, Kind: "invalid_envelope"}
	}
	if *envelope.Code != 0 {
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: *envelope.Code, Kind: "upstream_rejected"}
	}
	if len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) {
		return nil, errors.New("doubao empty response data")
	}
	return envelope.Data, nil
}

// PrepareCompletion proves that the current official desktop can construct a
// signed request without sending it. It never exposes cookies or signed values.
func (c *Client) PrepareCompletion(ctx context.Context, account, prompt string) (json.RawMessage, error) {
	s, err := c.sessions.Session(ctx, account)
	if err != nil {
		return nil, &APIError{Kind: "login_required"}
	}
	if s.Runtime == nil || c.runtime == nil {
		return nil, &APIError{Kind: "desktop_unavailable"}
	}
	preparer, ok := c.runtime.(CompletionPreparer)
	if !ok {
		return nil, &APIError{Kind: "desktop_unavailable"}
	}
	return preparer.PrepareCompletion(ctx, *s.Runtime, s.BotID, prompt)
}
func contextFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}
