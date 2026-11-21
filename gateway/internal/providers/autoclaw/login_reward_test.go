package autoclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
)

const rewardUserInfoPath = "/userapi/v1/user-info"
const rewardAccess = "fixture-autoclaw-access"

// This transport has no permissive fallback. Only user-info changes the
// synthetic weekly batch; successful authentication by itself does not.
type rewardLoginTransport struct {
	t                *testing.T
	mu               sync.Mutex
	events           []string
	device           string
	savedDevice      string
	userInfoCalls    int
	weeklyBatch      bool
	userInfoFails    bool
	userInfoExchange roundTripFunc
	readSaved        func() (stored, bool)
}

func (f *rewardLoginTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Scheme != "https" || r.URL.Host != apiBase {
		f.t.Error("unexpected host at the offline login boundary")
		return nil, errors.New("fixture_unexpected_host")
	}
	path := r.URL.RequestURI()
	if path == walletPath || path == expiringPath {
		if r.Method != http.MethodGet {
			f.t.Error("wallet fixture only permits GET")
			return nil, errors.New("fixture_unexpected_method")
		}
		f.events = append(f.events, "wallet_read")
		if path == walletPath {
			amount := 10000
			if f.weeklyBatch {
				amount += 10000
			}
			body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"total_balance": amount}})
			return creditResponse(r, http.StatusOK, string(body))
		}
		return creditResponse(r, http.StatusOK, `{"code":0,"data":{"expiring_points":0}}`)
	}
	if r.Method != http.MethodPost {
		f.t.Error("login fixture only permits the declared POST methods")
		return nil, errors.New("fixture_unexpected_method")
	}
	if path != sendCodePath && path != agentLoginPt && path != refreshPt && path != rewardUserInfoPath {
		f.t.Error("undeclared endpoint at the offline login boundary")
		return nil, errors.New("fixture_unexpected_endpoint")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	defer wipe(raw)
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return nil, errors.New("fixture_invalid_request")
	}
	device, _ := body["device_id"].(string)
	if body["source_id"] != "autoclaw" || device == "" {
		f.t.Error("request lost its original login device or source")
	}
	switch path {
	case sendCodePath:
		f.device = device
		f.events = append(f.events, "send_code")
		return creditResponse(r, http.StatusOK, `{"code":0}`)
	case agentLoginPt:
		if device != f.device {
			f.t.Error("login changed the per-flow device")
		}
		f.events = append(f.events, "login")
		return creditResponse(r, http.StatusOK, `{"code":0,"data":{"access_token":"fixture-agent-access","refresh_token":"fixture-agent-refresh","user_id":"fixture-upstream-user"}}`)
	case refreshPt:
		if device != f.device {
			f.t.Error("audience exchange changed the login device")
		}
		f.events = append(f.events, "audience_exchange")
		return creditResponse(r, http.StatusOK, `{"code":0,"data":{"access_token":"fixture-autoclaw-access","refresh_token":"fixture-autoclaw-refresh"}}`)
	case rewardUserInfoPath:
		f.userInfoCalls++
		f.events = append(f.events, "user_info")
		if len(body) != 2 || body["source_id"] != "autoclaw" || device != f.savedDevice {
			f.t.Error("user-info did not use exactly the persisted profile device and source")
		}
		if r.Header.Get("Authorization") != "Bearer "+rewardAccess || r.Header.Get("x-auth-sign") == "" || r.Header.Get("x-auth-timestamp") == "" || r.Header.Get("x-auth-appid") == "" {
			f.t.Error("user-info omitted the normal authorization or signature")
		}
		profile, saved := f.readSaved()
		if !saved || profile.DeviceID != device || profile.AccessToken != rewardAccess {
			f.t.Error("user-info ran before the authenticated profile was durably saved")
		}
		if f.userInfoExchange != nil {
			return f.userInfoExchange(r)
		}
		if f.userInfoFails {
			return nil, errors.New("PRIVATE_INITIALIZATION_REPLY_MUST_NOT_LEAK")
		}
		f.weeklyBatch = true
		return creditResponse(r, http.StatusOK, `{"code":0,"data":{"user_id":"fixture-upstream-user"}}`)
	}
	return nil, errors.New("fixture_unreachable_endpoint")
}

func (f *rewardLoginTransport) counts() (int, bool, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.userInfoCalls, f.weeklyBatch, append([]string(nil), f.events...)
}

type rewardLoginHarness struct {
	service   *authflow.Service
	vault     *accountvault.Vault
	transport *rewardLoginTransport
	client    *http.Client
	driver    *LoginDriver
	flowVault *accountvault.Vault
	flowPath  string
	now       time.Time
	saveFails bool
	saveCalls int
	savedIDs  []string
}

func newRewardLoginHarness(t *testing.T) *rewardLoginHarness {
	t.Helper()
	h := &rewardLoginHarness{now: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}
	h.vault = mustProviderVault(t)
	h.transport = &rewardLoginTransport{t: t}
	h.client = &http.Client{Transport: h.transport}
	h.transport.readSaved = func() (stored, bool) {
		if len(h.savedIDs) == 0 {
			return stored{}, false
		}
		_, raw, err := h.vault.Get(h.savedIDs[len(h.savedIDs)-1])
		if err != nil {
			return stored{}, false
		}
		defer wipe(raw)
		var profile stored
		err = json.Unmarshal(raw, &profile)
		return profile, err == nil
	}
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("login bypassed the injected offline transport")
		return nil, errors.New("fixture_default_transport_forbidden")
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	h.flowPath = filepath.Join(t.TempDir(), "reward-flows")
	flowVault, err := accountvault.Open(h.flowPath)
	if err != nil {
		t.Fatal(err)
	}
	h.flowVault = flowVault
	t.Cleanup(func() { h.flowVault.Close() })
	h.driver = &LoginDriver{HTTP: h.client, InviteCode: "-", Save: func(ctx context.Context, raw []byte) (string, error) {
		h.saveCalls++
		if h.saveFails {
			return "", errors.New("fixture_save_failed")
		}
		id, err := SaveProfile(h.vault)(ctx, raw)
		if err != nil {
			return "", err
		}
		h.savedIDs = append(h.savedIDs, id)
		var profile stored
		if json.Unmarshal(raw, &profile) != nil {
			return "", errors.New("fixture_saved_profile_invalid")
		}
		h.transport.mu.Lock()
		h.transport.savedDevice = profile.DeviceID
		h.transport.events = append(h.transport.events, "save")
		h.transport.mu.Unlock()
		return id, nil
	}}
	h.service, err = authflow.Open(authflow.Options{Vault: flowVault, Drivers: map[string]authflow.Driver{"autoclaw": h.driver}, Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *rewardLoginHarness) verify(t *testing.T) authflow.View {
	t.Helper()
	flow, err := h.service.Start(t.Context(), authflow.Request{Provider: "autoclaw", Method: "sms"})
	if err != nil || flow.Status != "waiting_phone" {
		t.Fatal("login did not start")
	}
	flow, err = h.service.Step(t.Context(), flow.ID, "send_code", authflow.Input{Phone: "1" + strings.Repeat("0", 10)})
	if err != nil || flow.Status != "waiting_code" {
		t.Fatal("synthetic SMS step failed")
	}
	flow, err = h.service.Step(t.Context(), flow.ID, "submit_code", authflow.Input{Code: strings.Repeat("1", 6)})
	if err != nil || flow.Status != "verified" {
		t.Fatal("synthetic verification failed")
	}
	return flow
}

func (h *rewardLoginHarness) commit(t *testing.T, flow authflow.View) authflow.View {
	t.Helper()
	view, err := h.service.Step(t.Context(), flow.ID, "commit", authflow.Input{})
	if err != nil {
		t.Fatalf("commit returned an unexpected error: %v", err)
	}
	return view
}

func TestLoginRewardCommitInitializesSavedAccountAndExposesWeeklyBatch(t *testing.T) {
	resetCreditCacheForTest(t)
	h := newRewardLoginHarness(t)
	verified := h.verify(t)
	if calls, weekly, _ := h.transport.counts(); calls != 0 || weekly || h.saveCalls != 0 {
		t.Fatal("verification initialized or saved an account before commit")
	}
	complete := h.commit(t, verified)
	if complete.Status != "succeeded" {
		t.Fatal("normal login did not complete")
	}
	calls, weekly, events := h.transport.counts()
	if calls != 1 || !weekly {
		t.Fatalf("saved login missed initialization: calls=%d weekly_batch=%t", calls, weekly)
	}
	if strings.Join(events, ",") != "send_code,login,audience_exchange,save,user_info" {
		t.Fatalf("unexpected safe call order: %v", events)
	}
	source := Source{Vault: h.vault, Client: h.client}
	for range 2 {
		view, err := source.Credits(t.Context(), complete.AccountID, true)
		if err != nil || view.Remain == nil || *view.Remain != 20000 {
			t.Fatal("wallet reader did not observe the fixture's initialized weekly batch")
		}
	}
	if after, _, _ := h.transport.counts(); after != 1 {
		t.Fatal("balance GET unexpectedly initialized the account again")
	}
}

func TestLoginRewardRepeatedCommitAndPollDoNotReplayInitialization(t *testing.T) {
	h := newRewardLoginHarness(t)
	complete := h.commit(t, h.verify(t))
	for range 2 {
		_, _ = h.service.Step(t.Context(), complete.ID, "commit", authflow.Input{})
		_, _ = h.service.Step(t.Context(), complete.ID, "poll", authflow.Input{})
	}
	if calls, _, _ := h.transport.counts(); calls != 1 || h.saveCalls != 1 {
		t.Fatalf("terminal flow replayed initialization calls=%d saves=%d", calls, h.saveCalls)
	}
}

func TestLoginRewardSaveFailureNeverInitializes(t *testing.T) {
	h := newRewardLoginHarness(t)
	h.saveFails = true
	complete := h.commit(t, h.verify(t))
	if complete.Status == "succeeded" {
		t.Fatal("failed save became a successful login")
	}
	if calls, _, _ := h.transport.counts(); calls != 0 {
		t.Fatal("user-info ran after the save failed")
	}
	rows, err := h.vault.List()
	if err != nil || len(rows) != 0 {
		t.Fatal("failed save left a persisted account")
	}
}

func TestLoginRewardSameAccountNewNormalLoginInitializesAgain(t *testing.T) {
	h := newRewardLoginHarness(t)
	first := h.commit(t, h.verify(t))
	h.now = h.now.Add(2 * time.Minute)
	second := h.commit(t, h.verify(t))
	if first.Status != "succeeded" || second.Status != "succeeded" || first.AccountID != second.AccountID {
		t.Fatal("re-login did not preserve the same account identity")
	}
	if calls, weekly, _ := h.transport.counts(); calls != 2 || !weekly {
		t.Fatalf("new normal login was blocked by a permanent once flag: calls=%d", calls)
	}
	rows, err := h.vault.List()
	if err != nil || len(rows) != 1 {
		t.Fatal("re-login duplicated the account")
	}
}

func TestLoginRewardInitializationFailureKeepsSavedAccountAndRedactsNotice(t *testing.T) {
	h := newRewardLoginHarness(t)
	h.transport.userInfoFails = true
	complete := h.commit(t, h.verify(t))
	if complete.Status != "succeeded" || complete.AccountID == "" {
		t.Fatal("initialization failure discarded the saved login")
	}
	if calls, _, _ := h.transport.counts(); calls != 1 {
		t.Fatalf("initialization network failure retried or was skipped: calls=%d", calls)
	}
	if strings.Contains(complete.Notice, "PRIVATE_INITIALIZATION_REPLY") || !strings.Contains(complete.Notice, "初始化") {
		t.Fatal("initialization failure notice is missing or contains a raw response")
	}
	rows, err := h.vault.List()
	if err != nil || len(rows) != 1 {
		t.Fatal("saved account was lost after initialization failed")
	}
}

const rewardPrivateReply = "PRIVATE_INITIALIZATION_REPLY_MUST_NOT_LEAK"

func rewardInitializeProfile(t *testing.T) []byte {
	t.Helper()
	return mustJSON(t, stored{
		AccessToken: rewardAccess, RefreshToken: "fixture-autoclaw-refresh",
		DeviceID: "acg-fixture-original-device", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
}

// Each direct API test also guards the process default: an implementation that
// ignores its supplied client must fail offline instead of contacting upstream.
func rewardInitializeClient(t *testing.T, exchange roundTripFunc) *http.Client {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("initialization bypassed the injected offline transport")
		return nil, errors.New("fixture_default_transport_forbidden")
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != apiBase || r.URL.RequestURI() != rewardUserInfoPath {
			t.Error("initialization reached an undeclared method, host or endpoint")
			return nil, errors.New("fixture_unexpected_request")
		}
		return exchange(r)
	})}
}

func assertRewardInitializeRequest(t *testing.T, r *http.Request) {
	t.Helper()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		t.Fatal("initialization request body was unreadable")
	}
	defer wipe(raw)
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil || len(payload) != 2 || payload["source_id"] != "autoclaw" || payload["device_id"] != "acg-fixture-original-device" {
		t.Fatal("initialization invented a device, campaign or extra payload field")
	}
	if r.Header.Get("Authorization") != "Bearer "+rewardAccess || r.Header.Get("x-auth-sign") == "" || r.Header.Get("x-auth-timestamp") == "" || r.Header.Get("x-auth-appid") == "" {
		t.Fatal("initialization omitted the ordinary identity authentication")
	}
	if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
		t.Fatal("initialization lost its JSON identity protocol")
	}
	for _, name := range []string{"X-Authorization", "X-Agent-Id", "X-Request-Model", "X-Query-Id", "X-Session-Id", "x_trace_id"} {
		if r.Header.Get(name) != "" {
			t.Error("initialization carried inference-specific context")
		}
	}
}

func assertRewardInitializeErrorPrivate(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("initialization unexpectedly accepted an invalid result")
	}
	for _, private := range []string{rewardPrivateReply, rewardAccess, "fixture-autoclaw-refresh", "acg-fixture-original-device"} {
		if strings.Contains(err.Error(), private) {
			t.Fatal("initialization error exposed private request or response material")
		}
	}
}

type rewardInitializeBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (b *rewardInitializeBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *rewardInitializeBody) Close() error { b.closed = true; return nil }

func TestInitializeLoginUsesOnlySavedIdentityAndExplicitAcknowledgement(t *testing.T) {
	const responseLimit = 64 << 10
	for _, tc := range []struct{ name, response string }{
		{"explicit_zero", `{"code":0}`},
		// An acknowledgment need not contain a user or wallet result. Extra data
		// must not turn this API into a reward-receipt parser.
		{"uninterpreted_data", `{"code":0,"data":{"msg":"` + rewardPrivateReply + `"}}`},
		{"exactly_64k", `{"code":0}` + strings.Repeat(" ", responseLimit-len(`{"code":0}`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := rewardInitializeProfile(t)
			before := append([]byte(nil), profile...)
			calls := 0
			body := &rewardInitializeBody{reader: strings.NewReader(tc.response)}
			client := rewardInitializeClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				assertRewardInitializeRequest(t, r)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: r}, nil
			})
			if InitializeLogin(t.Context(), profile, client) != nil {
				t.Fatal("explicit initialization acknowledgment was rejected")
			}
			if calls != 1 || !body.closed || body.read != len(tc.response) {
				t.Fatal("initialization retried, leaked its response, or missed the response boundary")
			}
			if !bytes.Equal(profile, before) {
				t.Fatal("initialization modified the caller's saved credential profile")
			}
		})
	}
}

func TestInitializeLoginRejectsInvalidRepliesWithoutRetry(t *testing.T) {
	const responseLimit = 64 << 10
	for _, tc := range []struct {
		name, response string
		status         int
	}{
		{"http_400", rewardPrivateReply, http.StatusBadRequest},
		{"http_401", rewardPrivateReply, http.StatusUnauthorized},
		{"http_429", rewardPrivateReply, http.StatusTooManyRequests},
		{"http_503", rewardPrivateReply, http.StatusServiceUnavailable},
		{"malformed_json", rewardPrivateReply, http.StatusOK},
		{"missing_code", `{"msg":"` + rewardPrivateReply + `"}`, http.StatusOK},
		{"null_code", `{"code":null,"msg":"` + rewardPrivateReply + `"}`, http.StatusOK},
		{"string_code", `{"code":"0","msg":"` + rewardPrivateReply + `"}`, http.StatusOK},
		{"business_failure", `{"code":9,"msg":"` + rewardPrivateReply + `"}`, http.StatusOK},
		{"null_response", `null`, http.StatusOK},
		{"oversized_valid_prefix", `{"code":0}` + strings.Repeat(" ", responseLimit), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			body := &rewardInitializeBody{reader: strings.NewReader(tc.response)}
			client := rewardInitializeClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				assertRewardInitializeRequest(t, r)
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body, Request: r}, nil
			})
			assertRewardInitializeErrorPrivate(t, InitializeLogin(t.Context(), rewardInitializeProfile(t), client))
			if calls != 1 || !body.closed || body.read > responseLimit+1 {
				t.Fatal("invalid response retried, leaked its body, or exceeded the bounded read")
			}
		})
	}
}

type rewardInitializeBrokenReader struct{}

func (rewardInitializeBrokenReader) Read([]byte) (int, error) {
	return 0, errors.New(rewardPrivateReply)
}

func TestInitializeLoginNetworkAndReadFailuresStayPrivateWithoutRetry(t *testing.T) {
	for _, networkFailure := range []bool{true, false} {
		name := "body_read_failure"
		if networkFailure {
			name = "network_failure"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			body := &rewardInitializeBody{reader: rewardInitializeBrokenReader{}}
			client := rewardInitializeClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				assertRewardInitializeRequest(t, r)
				if networkFailure {
					return nil, errors.New(rewardPrivateReply)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: r}, nil
			})
			assertRewardInitializeErrorPrivate(t, InitializeLogin(t.Context(), rewardInitializeProfile(t), client))
			if calls != 1 || (!networkFailure && !body.closed) {
				t.Fatal("failed initialization retried or leaked its response")
			}
		})
	}
}

func TestInitializeLoginRejectsInvalidProfilesWithoutHTTP(t *testing.T) {
	valid := stored{AccessToken: rewardAccess, RefreshToken: "fixture-autoclaw-refresh", DeviceID: "acg-fixture-original-device"}
	for _, tc := range []struct {
		name  string
		alter func(*stored)
		raw   string
	}{
		{name: "malformed_profile", raw: rewardPrivateReply},
		{name: "null_profile", raw: "null"},
		{name: "missing_access", alter: func(s *stored) { s.AccessToken = "" }},
		{name: "blank_access", alter: func(s *stored) { s.AccessToken = " \t" }},
		{name: "missing_device", alter: func(s *stored) { s.DeviceID = "" }},
		{name: "invalid_device", alter: func(s *stored) { s.DeviceID += "/" }},
		{name: "oversized_device", alter: func(s *stored) { s.DeviceID = strings.Repeat("x", 129) }},
		{name: "refresh_pending", alter: func(s *stored) { s.RefreshPending = true }},
		{name: "expired", alter: func(s *stored) { s.ExpiresAt = time.Now().Add(-time.Hour).Unix() }},
		{name: "expires_now", alter: func(s *stored) { s.ExpiresAt = time.Now().Unix() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := valid
			if tc.alter != nil {
				tc.alter(&profile)
			}
			raw := mustJSON(t, profile)
			if tc.raw != "" {
				raw = []byte(tc.raw)
			}
			calls := 0
			client := rewardInitializeClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				t.Error("invalid local profile reached an HTTP boundary")
				return nil, errors.New("fixture_request_forbidden")
			})
			assertRewardInitializeErrorPrivate(t, InitializeLogin(t.Context(), raw, client))
			if calls != 0 {
				t.Fatal("invalid profile triggered initialization or renewal")
			}
		})
	}
}

func TestInitializeLoginCancellationAndBoundedDeadline(t *testing.T) {
	t.Run("already_canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		calls := 0
		client := rewardInitializeClient(t, func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("fixture_request_forbidden")
		})
		assertRewardInitializeErrorPrivate(t, InitializeLogin(ctx, rewardInitializeProfile(t), client))
		if calls != 0 {
			t.Fatal("canceled initialization made an HTTP request")
		}
	})
	t.Run("in_flight_cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		client := rewardInitializeClient(t, func(r *http.Request) (*http.Response, error) {
			calls++
			cancel()
			<-r.Context().Done()
			return nil, errors.New(rewardPrivateReply)
		})
		assertRewardInitializeErrorPrivate(t, InitializeLogin(ctx, rewardInitializeProfile(t), client))
		if calls != 1 {
			t.Fatal("canceled initialization retried")
		}
	})
	for _, parentBounded := range []bool{false, true} {
		name := "eight_second_cap"
		if parentBounded {
			name = "shorter_parent_preserved"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			if parentBounded {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			}
			client := rewardInitializeClient(t, func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				maximum := time.Now().Add(8 * time.Second)
				if parentBounded {
					maximum, _ = ctx.Deadline()
					maximum = maximum.Add(-250 * time.Millisecond)
				}
				if !ok || !deadline.After(time.Now()) || deadline.After(maximum) {
					t.Error("initialization lost its bounded or parent deadline")
				}
				return creditResponse(r, http.StatusOK, `{"code":0}`)
			})
			if InitializeLogin(ctx, rewardInitializeProfile(t), client) != nil {
				t.Fatal("deadline-aware initialization failed")
			}
		})
	}
}

func TestInitializeLoginNeverFollowsRedirectOrMutatesCallerClient(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls, redirects := 0, 0
			client := rewardInitializeClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://fixture.invalid/never-follow"}}, Body: io.NopCloser(strings.NewReader(rewardPrivateReply)), Request: r}, nil
			})
			client.Timeout = 42 * time.Second
			client.CheckRedirect = func(*http.Request, []*http.Request) error {
				redirects++
				return errors.New("fixture_original_redirect_policy")
			}
			assertRewardInitializeErrorPrivate(t, InitializeLogin(t.Context(), rewardInitializeProfile(t), client))
			if calls != 1 || redirects != 0 {
				t.Fatal("initialization followed a redirect or invoked a shared redirect policy")
			}
			if client.Timeout != 42*time.Second || client.CheckRedirect == nil {
				t.Fatal("initialization changed its caller's shared client")
			}
			_ = client.CheckRedirect(nil, nil)
			if redirects != 1 {
				t.Fatal("initialization replaced the caller's redirect policy")
			}
		})
	}
}

func TestLoginRewardNormalRefreshDoesNotInitialize(t *testing.T) {
	v, account := refreshFixture(t)
	calls := 0
	p := offlineRefreshProvider(t, v, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != refreshPt {
			t.Error("normal renewal crossed into login initialization")
			return nil, errors.New("fixture_renewal_endpoint_only")
		}
		return refreshSuccess(r), nil
	})
	if _, err := p.refreshAccount(t.Context(), account.ID, "fixture-old-refresh"); err != nil {
		t.Fatal("ordinary renewal failed in its offline fixture")
	}
	if calls != 1 {
		t.Fatal("ordinary renewal unexpectedly initialized or retried")
	}
}

func TestLoginRewardInitializationBudgetLeavesParentForSuccessfulCommit(t *testing.T) {
	t.Run("optional_request_reaches_its_child_deadline", func(t *testing.T) {
		h := newRewardLoginHarness(t)
		verified := h.verify(t)
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Millisecond)
		defer cancel()
		h.transport.userInfoExchange = func(r *http.Request) (*http.Response, error) {
			parentDeadline, _ := ctx.Deadline()
			deadline, ok := r.Context().Deadline()
			if !ok || deadline.After(parentDeadline.Add(-250*time.Millisecond)) {
				t.Error("optional initialization did not reserve commit time")
			}
			<-r.Context().Done()
			if ctx.Err() != nil {
				t.Error("optional initialization consumed the parent deadline")
			}
			return nil, errors.New(rewardPrivateReply)
		}
		complete, err := h.service.Step(ctx, verified.ID, "commit", authflow.Input{})
		if err != nil || complete.Status != "succeeded" || complete.AccountID == "" || ctx.Err() != nil {
			t.Fatal("optional initialization timeout consumed or invalidated the saved login")
		}
		if calls, _, _ := h.transport.counts(); calls != 1 || h.saveCalls != 1 {
			t.Fatal("timed-out optional initialization was skipped or replayed")
		}
		if !strings.Contains(complete.Notice, "初始化") || strings.Contains(complete.Notice, rewardPrivateReply) {
			t.Fatal("initialization timeout lost its private, non-fatal notice")
		}
	})
	t.Run("insufficient_budget_never_posts", func(t *testing.T) {
		h := newRewardLoginHarness(t)
		verified := h.verify(t)
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		complete, err := h.service.Step(ctx, verified.ID, "commit", authflow.Input{})
		if err != nil || complete.Status != "succeeded" || ctx.Err() != nil {
			t.Fatal("skipped optional initialization prevented the saved login from completing")
		}
		if calls, _, _ := h.transport.counts(); calls != 0 || h.saveCalls != 1 {
			t.Fatal("initialization posted without enough budget for its commit")
		}
	})
}

func TestLoginRewardParentCancellationPersistsUncertainAndCannotReplayCommit(t *testing.T) {
	h := newRewardLoginHarness(t)
	verified := h.verify(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.transport.userInfoExchange = func(r *http.Request) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, errors.New(rewardPrivateReply)
	}
	interrupted, err := h.service.Step(ctx, verified.ID, "commit", authflow.Input{})
	if err != nil || interrupted.Status != "uncertain" {
		t.Fatal("canceled in-flight commit did not remain uncertain")
	}
	if calls, _, _ := h.transport.counts(); calls != 1 || h.saveCalls != 1 {
		t.Fatal("canceled initialization was retried or ran before save")
	}
	rows, err := h.vault.List()
	if err != nil || len(rows) != 1 {
		t.Fatal("canceled optional initialization lost the saved account")
	}
	if err := h.flowVault.Close(); err != nil {
		t.Fatal("could not close the encrypted flow fixture")
	}
	h.flowVault, err = accountvault.Open(h.flowPath)
	if err != nil {
		t.Fatal("could not reopen the same encrypted flow fixture")
	}
	h.service, err = authflow.Open(authflow.Options{Vault: h.flowVault, Drivers: map[string]authflow.Driver{"autoclaw": h.driver}, Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal("could not restore the interrupted flow service")
	}
	views, err := h.service.List()
	if err != nil || len(views) != 1 || views[0].ID != verified.ID || views[0].Status != "uncertain" {
		t.Fatal("uncertain commit was not durable across the encrypted flow reopen")
	}
	if replay, err := h.service.Step(t.Context(), verified.ID, "commit", authflow.Input{}); err == nil || replay.Status != "uncertain" {
		t.Fatal("reopened uncertain commit became replayable")
	}
	if calls, _, _ := h.transport.counts(); calls != 1 || h.saveCalls != 1 {
		t.Fatal("reopened interrupted flow replayed save or initialization")
	}
}

func TestLoginRewardReferralAlsoReservesTheParentCommitBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 900*time.Millisecond)
	defer cancel()
	calls, saves := 0, 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		switch r.URL.Path {
		case loginInitializePath:
			assertRewardInitializeRequest(t, r)
			return creditResponse(r, http.StatusOK, ` {"code":0}`)
		case fissionBindPt:
			parentDeadline, _ := ctx.Deadline()
			deadline, ok := r.Context().Deadline()
			if !ok || deadline.After(parentDeadline.Add(-250*time.Millisecond)) {
				t.Error("referral consumed the parent commit budget")
			}
			<-r.Context().Done()
			return nil, errors.New("fixture_optional_timeout")
		default:
			t.Error("unexpected optional login request")
			return nil, errors.New("fixture_request_forbidden")
		}
	})}
	driver := &LoginDriver{HTTP: client, InviteCode: "FIXTURE1", Save: func(context.Context, []byte) (string, error) {
		saves++
		return "autoclaw-fixture", nil
	}}
	result, err := driver.commit(ctx, &loginState{Profile: rewardInitializeProfile(t)})
	if err != nil || result.Status != "succeeded" || ctx.Err() != nil || saves != 1 || calls != 2 {
		t.Fatal("optional referral timeout invalidated or replayed the saved login")
	}
}
