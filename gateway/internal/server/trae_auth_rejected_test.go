package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/requestlog"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

func TestTraeRejectedSessionReturnsReauthenticationWithoutInvalidParamsOrAccountPenalty(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			testTraeRejectedSession(t, status)
		})
	}
}

func testTraeRejectedSession(t *testing.T, upstreamStatus int) {
	t.Helper()
	t.Setenv("WB2API_TRAE_REMOTE", "0")
	var chats, exchanges atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == trae.BatchCatalogPath {
			io.WriteString(w, `{"function_configs":[]}`)
			return
		}
		if r.URL.Path == trae.ChatPath {
			chats.Add(1)
			w.WriteHeader(upstreamStatus)
			io.WriteString(w, `{"code":1001,"message":"fixture revoked session"}`)
			return
		}
		w.WriteHeader(404)
	}))
	defer origin.Close()
	oldGateway := trae.GatewayBaseVar
	trae.GatewayBaseVar = origin.URL
	trae.InvalidateCatalog("")
	defer func() { trae.GatewayBaseVar = oldGateway; trae.InvalidateCatalog("") }()
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		exchanges.Add(1)
		return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"private":"fixture must not escape"}`)), Request: r}, nil
	})}
	defer func() { http.DefaultClient = oldClient }()
	account, err := auth.Parse([]byte(`{"uid":"fixture-trae-reauth","realm":"trae","accessToken":"fixture-old","refreshToken":"fixture-refresh","expiresAt":4102444800}`))
	if err != nil {
		t.Fatal(err)
	}
	pool := testPoolWith(account)
	before, _ := pool.Status(account.UID)
	history, err := requestlog.Open(filepath.Join(t.TempDir(), "requests.jsonl"), requestlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	h := NewHandler(Config{Pool: pool, Upstream: upstream.New(), APIKey: "fixture-caller", Requests: history})
	defer h.cfg.KeyLimits.Close()
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"trae:DeepSeek-V4-Flash","messages":[{"role":"user","content":"fixture"}],"max_tokens":64}`)).WithContext(context.Background())
	request.Header.Set("Authorization", "Bearer fixture-caller")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 401 || !strings.Contains(response.Body.String(), "trae_authentication_required") || strings.Contains(response.Body.String(), "invalid_request") || strings.Contains(response.Body.String(), "fixture must not escape") {
		t.Fatal("Trae authentication rejection was misclassified or exposed private upstream detail")
	}
	if chats.Load() != 1 || exchanges.Load() != 1 {
		t.Fatal("failed refresh retried a generation or refreshed more than once")
	}
	after, _ := pool.Status(account.UID)
	if after.Disabled != before.Disabled || after.InFlight != 0 {
		t.Fatal("reauthentication retained a lease or disabled the account")
	}
	page, err := history.List(requestlog.Query{})
	if err != nil || len(page.Items) != 1 || page.Items[0].HTTPStatus != 401 {
		t.Fatal("request history did not retain the actual 401 status")
	}
}
