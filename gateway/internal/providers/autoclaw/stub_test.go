package autoclaw

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubHTTP returns a client whose transport answers the send-code endpoint with
// a success envelope, and records which path was called.
func stubHTTP(t *testing.T, calledPath *string) *http.Client {
	t.Helper()
	return &http.Client{Transport: stubTransport{t: t, calledPath: calledPath}}
}

type stubTransport struct {
	t          *testing.T
	calledPath *string
}

func (s stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.t.Helper()
	if s.calledPath != nil {
		*s.calledPath = r.URL.Path
	}
	body := `{"code":0,"msg":"success"}`
	switch {
	case strings.Contains(r.URL.Path, "agent-send-code"):
		body = `{"code":0,"msg":"success"}`
	case strings.Contains(r.URL.Path, "agent-login"):
		body = `{"code":0,"msg":"success","data":{"access_token":"synthetic.access.token","refresh_token":"synthetic-refresh","user_id":"u1"}}`
	case strings.Contains(r.URL.Path, "refresh"):
		body = `{"code":0,"data":{"access_token":"swapped.access.token","refresh_token":"swapped-refresh"}}`
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}
