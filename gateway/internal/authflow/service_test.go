package authflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/accountvault"
)

type driverStub struct {
	start func(context.Context, Request) (Result, error)
	step  func(context.Context, Flow, string, Input) (Result, error)
}

func (*driverStub) Methods() []Method {
	return []Method{{ID: "sms", Available: true}, {ID: "qr", Available: true}}
}
func (d *driverStub) Start(c context.Context, r Request) (Result, error) {
	if d.start != nil {
		return d.start(c, r)
	}
	return Result{Status: "waiting_phone", Secret: json.RawMessage(`{"credential":"synthetic-session-private"}`)}, nil
}
func (d *driverStub) Step(c context.Context, f Flow, a string, i Input) (Result, error) {
	return d.step(c, f, a, i)
}
func testService(t *testing.T, d Driver) (*Service, *accountvault.Vault, string, *time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vault")
	v, err := accountvault.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	s, err := Open(Options{Vault: v, Drivers: map[string]Driver{"synthetic": d}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return s, v, path, &now
}
func TestEncryptedRecoveryCooldownAndNoSecretProjection(t *testing.T) {
	var sends atomic.Int32
	d := &driverStub{step: func(c context.Context, f Flow, a string, i Input) (Result, error) {
		sends.Add(1)
		return Result{Status: "waiting_code", RetryAfter: 90 * time.Second, Secret: json.RawMessage(`{"credential":"synthetic-session-private"}`)}, nil
	}}
	s, v, path, now := testService(t, d)
	f, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "sms"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(context.Background(), Request{Provider: "synthetic", Method: "sms"}); err != ErrBusy {
		t.Fatal("duplicate start allowed")
	}
	f, err = s.Step(context.Background(), f.ID, "send_code", Input{Phone: "synthetic-phone"})
	if err != nil || f.Status != "waiting_code" {
		t.Fatal("send transition failed")
	}
	if _, err = s.Step(context.Background(), f.ID, "send_code", Input{}); err != ErrBusy || sends.Load() != 1 {
		t.Fatal("cooldown bypassed")
	}
	rows, _ := s.List()
	public, _ := json.Marshal(rows)
	if strings.Contains(string(public), "synthetic-session-private") || strings.Contains(string(public), "secret") {
		t.Fatal("private state exposed")
	}
	b, _ := os.ReadFile(filepath.Join(path, "authentication-flows.account"))
	if strings.Contains(string(b), "synthetic-session-private") {
		t.Fatal("private state stored plaintext")
	}
	if err = v.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := accountvault.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	resume, err := Open(Options{Vault: restored, Drivers: s.opts.Drivers, Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	rows, err = resume.List()
	if err != nil || rows[0].Status != "waiting_code" || rows[0].RetryAt != f.RetryAt {
		t.Fatal("restart lost waiting state or cooldown")
	}
	*now = now.Add(11 * time.Minute)
	rows, err = resume.List()
	if err != nil || rows[0].Status != "expired" {
		t.Fatal("expiry missing")
	}
	if resume.flows[0].Secret != nil {
		t.Fatal("expired flow retained credential")
	}
}
func TestCancellationWinsLateCompletionAndDriverPanicIsolated(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	d := &driverStub{start: func(context.Context, Request) (Result, error) {
		close(started)
		<-release
		return Result{Status: "succeeded"}, nil
	}}
	s, _, _, _ := testService(t, d)
	done := make(chan struct{})
	go func() { defer close(done); s.Start(context.Background(), Request{Provider: "synthetic", Method: "qr"}) }()
	<-started
	rows, _ := s.List()
	canceled, err := s.Step(context.Background(), rows[0].ID, "cancel", Input{})
	if err != nil || canceled.Status != "uncertain" {
		t.Fatal("cancel failed")
	}
	close(release)
	<-done
	rows, _ = s.List()
	if rows[0].Status != "uncertain" {
		t.Fatal("late completion overwrote cancel")
	}
	if _, err = s.Step(context.Background(), rows[0].ID, "cancel", Input{}); err != nil {
		t.Fatal(err)
	}
	d.start = func(context.Context, Request) (Result, error) { panic("synthetic-private-error") }
	f, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "qr"})
	if err != nil || f.Status != "uncertain" || strings.Contains(f.ErrorCode, "private") {
		t.Fatal("panic not isolated")
	}
}
func TestInterruptedMutationRequiresReconciliation(t *testing.T) {
	d := &driverStub{}
	s, v, _, _ := testService(t, d)
	s.mu.Lock()
	err := s.save([]Flow{{View: View{ID: "synthetic-flow", Request: Request{Provider: "synthetic", Method: "sms"}, Status: "working", ExpiresAt: time.Now().Add(time.Hour), Revision: 1}, Action: "submit_code", Secret: json.RawMessage(`{"synthetic":true}`)}})
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Vault: v, Drivers: s.opts.Drivers})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := reopened.List()
	if rows[0].Status != "uncertain" {
		t.Fatal("interrupted write was replayable")
	}
	if _, err = reopened.Step(context.Background(), rows[0].ID, "submit_code", Input{Code: "000000"}); err != ErrInvalid {
		t.Fatal("replayed uncertain mutation")
	}
}
func TestDriverFailureAndHTTPStrictBoundary(t *testing.T) {
	d := &driverStub{start: func(context.Context, Request) (Result, error) {
		return Result{}, errors.New("synthetic-sensitive-value")
	}}
	s, _, _, _ := testService(t, d)
	for _, body := range []string{`{"provider":"synthetic","method":"qr","extra":1}`, `{"provider":"synthetic","method":"qr"} {}`} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/connections/start", strings.NewReader(body))
		s.Route(w, r)
		if w.Code != 400 {
			t.Fatal("invalid request accepted")
		}
	}
	w := httptest.NewRecorder()
	s.Route(w, httptest.NewRequest(http.MethodPost, "/api/connections/start", strings.NewReader(`{"provider":"synthetic","method":"qr"}`)))
	if w.Code != 200 || strings.Contains(w.Body.String(), "synthetic-sensitive-value") || !strings.Contains(w.Body.String(), "uncertain") {
		t.Fatal("failure projection invalid")
	}
}

func TestCancellationDoesNotResetSMSCooldown(t *testing.T) {
	var count atomic.Int32
	d := &driverStub{step: func(context.Context, Flow, string, Input) (Result, error) {
		count.Add(1)
		return Result{Status: "waiting_code", RetryAfter: 90 * time.Second}, nil
	}}
	s, _, _, _ := testService(t, d)
	f, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "sms"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Step(context.Background(), f.ID, "send_code", Input{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Step(context.Background(), f.ID, "cancel", Input{}); err != nil {
		t.Fatal(err)
	}
	next, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "sms"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Step(context.Background(), next.ID, "send_code", Input{}); err != ErrBusy || count.Load() != 1 {
		t.Fatal("cancel/new-flow bypassed provider SMS cooldown")
	}
}
func TestBoundedFlowRetentionAndNoOptimisticSuccess(t *testing.T) {
	d := &driverStub{start: func(context.Context, Request) (Result, error) { return Result{Status: "failed"}, nil }}
	s, _, _, _ := testService(t, d)
	for i := 0; i < 30; i++ {
		if _, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "qr"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := s.List()
	if len(rows) != 16 {
		t.Fatal("flow history is unbounded")
	}
	d.start = func(context.Context, Request) (Result, error) { return Result{Status: "succeeded"}, nil }
	f, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "qr"})
	if err != nil || f.Status != "uncertain" {
		t.Fatal("start result bypassed verified commit")
	}
}

func TestBoundedProviderBusinessCodeSurvivesWorkflowProjection(t *testing.T) {
	code := int64(9999)
	d := &driverStub{start: func(context.Context, Request) (Result, error) {
		return Result{Status: "manual_action", ErrorCode: "official_request_rejected", UpstreamCode: &code}, nil
	}}
	s, _, _, _ := testService(t, d)
	f, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "qr"})
	if err != nil || f.UpstreamCode == nil || *f.UpstreamCode != code {
		t.Fatal("business code lost")
	}
	rows, err := s.List()
	if err != nil || rows[0].UpstreamCode == nil || *rows[0].UpstreamCode != code {
		t.Fatal("persisted business code lost")
	}
}

// A completed-but-unsaved login must not block the next one. The operator
// adds several accounts in a row; each earlier flow sits at "verified" waiting
// for its idempotent commit, or at "uncertain" needing a look. Only a live
// upstream dialog may hold the provider.
func TestFinishedLoginDoesNotBlockTheNextAccount(t *testing.T) {
	d := &driverStub{step: func(_ context.Context, _ Flow, _ string, _ Input) (Result, error) {
		return Result{Status: "verified", Secret: json.RawMessage(`{}`)}, nil
	}}
	s, _, _, _ := testService(t, d)

	first, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "sms"})
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	verified, err := s.Step(context.Background(), first.ID, "send_code", Input{Phone: "13800000000"})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Status != "verified" {
		t.Fatalf("setup: status=%q", verified.Status)
	}

	// The first flow is still non-terminal (awaiting commit), yet a second
	// account must be addable.
	second, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "sms"})
	if err != nil {
		t.Fatalf("a verified flow blocked the next login: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("the second start reused the first flow")
	}
}

// A flow that needs human attention must not block the provider either.
func TestUncertainLoginDoesNotBlockTheNextAccount(t *testing.T) {
	d := &driverStub{step: func(_ context.Context, _ Flow, _ string, _ Input) (Result, error) {
		return Result{Status: "uncertain"}, nil
	}}
	s, _, _, _ := testService(t, d)
	first, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "sms"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Step(context.Background(), first.ID, "send_code", Input{Phone: "13800000000"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "sms"}); err != nil {
		t.Fatalf("an uncertain flow blocked the next login: %v", err)
	}
}

// A live dialog still wins: two SMS sends racing the same provider would
// confuse the account being created.
func TestLiveDialogStillBlocksAConcurrentStart(t *testing.T) {
	d := &driverStub{step: func(_ context.Context, _ Flow, _ string, _ Input) (Result, error) {
		return Result{Status: "waiting_code"}, nil
	}}
	s, _, _, _ := testService(t, d)
	first, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "sms"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Step(context.Background(), first.ID, "send_code", Input{Phone: "13800000000"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "sms"}); err != ErrBusy {
		t.Fatalf("a waiting_code flow did not block a concurrent start: %v", err)
	}
}
