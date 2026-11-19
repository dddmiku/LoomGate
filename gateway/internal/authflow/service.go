// Package authflow persists bounded authentication workflows without exposing
// provider credentials. Drivers own protocol semantics and authoritative commit.
package authflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"time"

	"workbuddy2api/internal/accountvault"
)

type Method struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	InputMode string `json:"input_mode,omitempty"`
}
type Provider struct {
	ID      string   `json:"id"`
	Methods []Method `json:"methods"`
}
type Request struct {
	Provider  string `json:"provider"`
	Method    string `json:"method"`
	Realm     string `json:"realm,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}
type Input struct {
	Phone    string `json:"phone,omitempty"`
	Code     string `json:"code,omitempty"`
	Callback string `json:"callback,omitempty"`
	// InviteCode is an optional referral code the operator types while adding
	// an account. Empty means the provider's own default applies.
	InviteCode string `json:"invite_code,omitempty"`
}

func (Input) String() string   { return "authflow.Input[redacted]" }
func (Input) GoString() string { return "authflow.Input[redacted]" }

type View struct {
	ID string `json:"id"`
	Request
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	NextPollAt     time.Time `json:"next_poll_at"`
	RetryAt        time.Time `json:"retry_at"`
	ActionURL      string    `json:"action_url,omitempty"`
	QRURL          string    `json:"qr_url,omitempty"`
	ErrorCode      string    `json:"error_code,omitempty"`
	UpstreamCode   *int64    `json:"upstream_code,omitempty"`
	Notice         string    `json:"notice,omitempty"`
	Revision       uint64    `json:"revision"`
	CanVerify      bool      `json:"can_verify,omitempty"`
	InputMode      string    `json:"input_mode,omitempty"`
	Authentication string    `json:"authentication,omitempty"`
}

// Flow is private storage. Only View may be serialized into management responses.
type Flow struct {
	View
	Secret json.RawMessage `json:"secret,omitempty"`
	Action string          `json:"action,omitempty"`
	Polls  int             `json:"polls,omitempty"`
}

func (Flow) String() string   { return "authflow.Flow[redacted]" }
func (Flow) GoString() string { return "authflow.Flow[redacted]" }

type Result struct {
	Authentication                                         string
	InputMode                                              string
	UpstreamCode                                           *int64
	Status, ActionURL, QRURL, ErrorCode, Notice, AccountID string
	Secret                                                 json.RawMessage
	RetryAfter                                             time.Duration
	ExpiresAt                                              time.Time
}

func (Result) String() string   { return "authflow.Result[redacted]" }
func (Result) GoString() string { return "authflow.Result[redacted]" }

type Driver interface {
	Methods() []Method
	Start(context.Context, Request) (Result, error)
	Step(context.Context, Flow, string, Input) (Result, error)
}

// PreparedStarter persists the provider intent before any external side effect.
// PrepareStart must only inspect local state, never perform network operations.
type PreparedStarter interface {
	PrepareStart(Request) (json.RawMessage, error)
	StartPrepared(context.Context, Flow) (Result, error)
}
type Canceler interface {
	Cancel(context.Context, Flow) error
}

// Verifier permits only an explicit read-only recovery. It must not send codes,
// create QR sessions, commit accounts or otherwise replay an interrupted write.
type Verifier interface {
	CanVerify(Flow) bool
	Verify(context.Context, Flow) (Result, error)
}
type Options struct {
	Vault    *accountvault.Vault
	Drivers  map[string]Driver
	Now      func() time.Time
	Timeout  time.Duration
	Lifetime time.Duration
}
type Service struct {
	mu     sync.Mutex
	opts   Options
	record accountvault.Account
	flows  []Flow
	active map[string]context.CancelFunc
}

var ErrBusy = errors.New("authentication_busy")
var ErrInvalid = errors.New("authentication_request_invalid")
var ErrMissing = errors.New("authentication_not_found")
var ErrStorage = errors.New("authentication_storage_unavailable")

func Open(o Options) (*Service, error) {
	if o.Vault == nil {
		return nil, ErrStorage
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.Lifetime <= 0 {
		o.Lifetime = 10 * time.Minute
	}
	s := &Service{opts: o, active: map[string]context.CancelFunc{}, flows: []Flow{}}
	rows, err := o.Vault.List()
	if err != nil {
		return nil, ErrStorage
	}
	for _, row := range rows {
		if row.ID == "authentication-flows" {
			s.record = row
			_, b, e := o.Vault.Get(row.ID)
			if e != nil {
				return nil, ErrStorage
			}
			e = json.Unmarshal(b, &s.flows)
			for i := range b {
				b[i] = 0
			}
			if e != nil || len(s.flows) > 16 {
				return nil, ErrStorage
			}
		}
	}
	for i := range s.flows {
		if s.flows[i].Status == "working" {
			s.flows[i].Status = "uncertain"
			s.flows[i].Action = ""
			s.flows[i].ErrorCode = "interrupted_operation"
			s.flows[i].Revision++
		}
	}
	if len(s.flows) > 0 {
		if err = s.save(s.flows); err != nil {
			return nil, err
		}
	}
	return s, nil
}
func (s *Service) save(rows []Flow) error {
	b, err := json.Marshal(rows)
	if err != nil {
		return ErrStorage
	}
	defer func() {
		for i := range b {
			b[i] = 0
		}
	}()
	a, err := s.opts.Vault.Put(accountvault.Account{ID: "authentication-flows", Provider: "authentication", Enabled: false}, b, s.record.Revision)
	if err != nil {
		return ErrStorage
	}
	s.record = a
	s.flows = rows
	return nil
}
func terminal(status string) bool {
	return status == "succeeded" || status == "canceled" || status == "expired" || status == "failed"
}

// inProgress reports whether a flow still owns the driver's login session.
// Only these may block a second start for the same provider.
//
// "verified" deliberately does not count: identity has already been proven and
// the only remaining work is an idempotent commit. "uncertain" and
// "manual_action" are likewise finished attempts that need the operator to
// look, not a live upstream dialog. Treating any of them as "in progress" made
// the first successful login permanently block every later one, because such a
// flow only clears when its lifetime expires.
func inProgress(status string) bool {
	switch status {
	case "working", "waiting_phone", "waiting_code", "waiting_scan", "scanned", "verifying", "waiting_authorization":
		return true
	}
	return false
}
func (s *Service) Providers() []Provider {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Provider{}
	for id, d := range s.opts.Drivers {
		out = append(out, Provider{ID: id, Methods: d.Methods()})
	}
	return out
}
func (s *Service) List() ([]View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := append([]Flow(nil), s.flows...)
	changed := false
	for i := range rows {
		if !terminal(rows[i].Status) && !s.opts.Now().Before(rows[i].ExpiresAt) {
			if cancel := s.active[rows[i].ID]; cancel != nil {
				cancel()
			}
			rows[i].Status = "expired"
			rows[i].Secret = nil
			rows[i].QRURL = ""
			rows[i].ActionURL = ""
			rows[i].Revision++
			changed = true
		}
	}
	if changed {
		if err := s.save(rows); err != nil {
			return nil, err
		}
	}
	out := []View{}
	for _, f := range s.flows {
		view := f.View
		if verifier, ok := s.opts.Drivers[f.Provider].(Verifier); ok {
			view.CanVerify = !terminal(f.Status) && f.Status != "working" && s.active[f.ID] == nil && verifier.CanVerify(f)
		}
		out = append(out, view)
	}
	return out, nil
}
func (s *Service) Start(ctx context.Context, r Request) (View, error) {
	if r.Provider == "workbuddy" && r.Realm == "" {
		r.Realm = "cn"
	}
	// Validate the provider/realm pair before consulting a driver or touching
	// persisted flows. A foreign realm must never fall through to a CN request.
	if !validRealm(r) {
		return View{}, ErrInvalid
	}
	d := s.opts.Drivers[r.Provider]
	if d == nil {
		return View{}, ErrInvalid
	}
	allowed := false
	inputMode := ""
	for _, m := range d.Methods() {
		if m.ID == r.Method && m.Available {
			allowed = true
			inputMode = m.InputMode
		}
	}
	if !allowed {
		return View{}, ErrInvalid
	}
	if r.AccountID != "" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(r.AccountID) {
		return View{}, ErrInvalid
	}
	if _, err := s.List(); err != nil {
		return View{}, err
	}
	s.mu.Lock()
	for _, f := range s.flows {
		// Only a live upstream session blocks a new one. A finished login that
		// is merely waiting to be saved must not stop the operator from adding
		// a different account.
		if f.Provider == r.Provider && f.Realm == r.Realm && inProgress(f.Status) {
			s.mu.Unlock()
			return f.View, ErrBusy
		}
	}
	rows := append([]Flow(nil), s.flows...)
	if len(rows) >= 16 {
		idx := -1
		for i, f := range rows {
			if terminal(f.Status) && !s.opts.Now().Before(f.RetryAt) {
				idx = i
				break
			}
		}
		if idx < 0 {
			s.mu.Unlock()
			return View{}, ErrBusy
		}
		rows = append(rows[:idx], rows[idx+1:]...)
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		s.mu.Unlock()
		return View{}, ErrStorage
	}
	now := s.opts.Now()
	f := Flow{View: View{ID: hex.EncodeToString(b), Request: r, Status: "working", CreatedAt: now, ExpiresAt: now.Add(s.opts.Lifetime), Revision: 1}, Action: "start"}
	f.InputMode = inputMode
	prepared, hasPrepared := d.(PreparedStarter)
	if hasPrepared {
		secret, err := prepared.PrepareStart(r)
		if err != nil {
			s.mu.Unlock()
			return View{}, ErrInvalid
		}
		f.Secret = secret
	}
	rows = append(rows, f)
	if err := s.save(rows); err != nil {
		s.mu.Unlock()
		return View{}, err
	}
	bounded, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	s.active[f.ID] = cancel
	s.mu.Unlock()
	if hasPrepared {
		return s.call(bounded, cancel, f, func() (Result, error) { return prepared.StartPrepared(bounded, f) })
	}
	return s.call(bounded, cancel, f, func() (Result, error) { return d.Start(bounded, r) })
}
func validRealm(r Request) bool {
	if r.Provider == "workbuddy" {
		return r.Realm == "cn" || r.Realm == "global"
	}
	return r.Realm == ""
}
func allowed(status, action string) bool {
	switch action {
	case "resume":
		return status == "uncertain" || status == "manual_action" || status == "waiting_authorization"
	case "poll":
		return status == "waiting_scan" || status == "scanned" || status == "verifying" || status == "waiting_authorization" || status == "waiting_phone" || status == "waiting_code"
	case "send_code":
		return status == "waiting_phone" || status == "waiting_code"
	case "submit_code":
		return status == "waiting_code"
	case "callback":
		return status == "waiting_authorization"
	case "commit":
		return status == "verified"
	}
	return false
}
func (s *Service) Step(ctx context.Context, id, action string, input Input) (View, error) {
	if len(input.Phone) > 64 || len(input.Code) > 32 || len(input.Callback) > 16384 || len(input.InviteCode) > 64 {
		return View{}, ErrInvalid
	}
	if _, err := s.List(); err != nil {
		return View{}, err
	}
	s.mu.Lock()
	rows := append([]Flow(nil), s.flows...)
	idx := -1
	for i := range rows {
		if rows[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return View{}, ErrMissing
	}
	f := rows[idx]
	if f.InputMode == "official" && (input.Phone != "" || input.Code != "" || input.Callback != "" || action == "send_code" || action == "submit_code") {
		s.mu.Unlock()
		return f.View, ErrInvalid
	}
	if action == "cancel" {
		original := f
		if cancel := s.active[id]; cancel != nil {
			cancel()
		}
		if terminal(f.Status) {
			s.mu.Unlock()
			return f.View, nil
		}
		if canceler, ok := s.opts.Drivers[f.Provider].(Canceler); ok && original.Status != "working" && s.active[id] == nil {
			f.Status = "uncertain"
			f.ErrorCode = "cancellation_unconfirmed"
			f.Notice = "正在关闭官方登录窗口；关闭结果核验前请勿重新开始。"
			f.Revision++
			rows[idx] = f
			if err := s.save(rows); err != nil {
				s.mu.Unlock()
				return View{}, err
			}
			bounded, cancel := context.WithTimeout(ctx, 6*time.Second)
			s.active[id] = cancel
			s.mu.Unlock()
			_, cancelErr := safeCall(func() (Result, error) { return Result{}, canceler.Cancel(bounded, original) })
			cancel()
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.active, id)
			rows = append([]Flow(nil), s.flows...)
			idx = -1
			for i := range rows {
				if rows[i].ID == id {
					idx = i
					break
				}
			}
			if idx < 0 {
				return View{}, ErrMissing
			}
			if rows[idx].Revision != f.Revision {
				return rows[idx].View, nil
			}
			if cancelErr == nil {
				f.Status = "canceled"
				f.ErrorCode = ""
				f.Secret = nil
				f.Notice = "此次登录已取消。"
			} else {
				f.Notice = "官方窗口关闭未确认，请先核对官方状态；不会自动重发登录。"
			}
			f.QRURL = ""
			f.ActionURL = ""
			f.Revision++
			rows[idx] = f
			return f.View, s.save(rows)
		}
		f.Status = "canceled"
		if rows[idx].Status == "working" || s.active[id] != nil {
			f.Status = "uncertain"
			f.Notice = "取消请求已发出，进行中的外部操作可能已经执行，请先核验官方状态。"
			f.ErrorCode = "cancellation_unconfirmed"
		}
		f.Secret = nil
		if f.Status == "uncertain" {
			f.Secret = original.Secret
		}
		f.QRURL = ""
		f.ActionURL = ""
		f.Revision++
		rows[idx] = f
		err := s.save(rows)
		s.mu.Unlock()
		return f.View, err
	}
	// A canceled operation may still be returning from its driver. Do not let
	// recovery replace its cancellation handle or overlap another operation.
	if s.active[id] != nil {
		s.mu.Unlock()
		return f.View, ErrBusy
	}
	verifier, canVerify := s.opts.Drivers[f.Provider].(Verifier)
	readVerify := action == "verify" && canVerify && !terminal(f.Status) && f.Status != "working" && verifier.CanVerify(f)
	if readVerify && (input.Phone != "" || input.Code != "" || input.Callback != "") {
		s.mu.Unlock()
		return f.View, ErrInvalid
	}
	if !allowed(f.Status, action) && !readVerify {
		s.mu.Unlock()
		return f.View, ErrInvalid
	}
	if !validRealm(f.Request) {
		s.mu.Unlock()
		return f.View, ErrInvalid
	}
	now := s.opts.Now()
	if action == "send_code" {
		for _, prior := range rows {
			if prior.Provider == f.Provider && now.Before(prior.RetryAt) {
				s.mu.Unlock()
				return f.View, ErrBusy
			}
		}
	}
	if (action == "poll" || readVerify) && now.Before(f.NextPollAt) || action == "send_code" && now.Before(f.RetryAt) {
		s.mu.Unlock()
		return f.View, ErrBusy
	}
	d := s.opts.Drivers[f.Provider]
	if d == nil {
		s.mu.Unlock()
		return View{}, ErrInvalid
	}
	f.Status = "working"
	f.Action = action
	if action == "send_code" {
		f.RetryAt = now.Add(time.Minute)
	}
	f.Revision++
	rows[idx] = f
	if err := s.save(rows); err != nil {
		s.mu.Unlock()
		return View{}, err
	}
	bounded, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	s.active[id] = cancel
	s.mu.Unlock()
	if readVerify {
		return s.call(bounded, cancel, f, func() (Result, error) { return verifier.Verify(bounded, f) })
	}
	return s.call(bounded, cancel, f, func() (Result, error) { return d.Step(bounded, f, action, input) })
}
func (s *Service) call(ctx context.Context, cancel context.CancelFunc, f Flow, fn func() (Result, error)) (View, error) {
	defer cancel()
	result, err := safeCall(fn)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, f.ID)
	rows := append([]Flow(nil), s.flows...)
	idx := -1
	for i := range rows {
		if rows[i].ID == f.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return View{}, ErrMissing
	}
	if rows[idx].Revision != f.Revision {
		return rows[idx].View, nil
	}
	f = rows[idx]
	if result.Status == "succeeded" && f.Action != "commit" {
		err = errors.New("authentication_commit_required")
	}
	f.Revision++
	if f.Action == "poll" {
		f.Polls++
	}
	f.Action = ""
	f.ErrorCode = ""
	f.UpstreamCode = nil
	delay := 2 * time.Second
	for i := 0; i < f.Polls && delay < 8*time.Second; i++ {
		delay *= 2
	}
	f.NextPollAt = s.opts.Now().Add(delay)
	if err != nil || ctx.Err() != nil {
		f.Status = "uncertain"
		f.ErrorCode = "operation_unconfirmed"
		f.Notice = "操作结果未确认，请核验官方界面后取消或重新接入。"
	} else {
		switch result.Status {
		case "waiting_phone", "waiting_code", "waiting_scan", "scanned", "waiting_authorization", "verifying", "verified", "manual_action", "uncertain", "succeeded", "failed", "expired":
			f.Status = result.Status
		default:
			f.Status = "uncertain"
			result.ErrorCode = "invalid_provider_state"
		}
		f.ErrorCode = result.ErrorCode
		f.UpstreamCode = result.UpstreamCode
		f.Notice = result.Notice
		f.InputMode = result.InputMode
		if result.Authentication != "" {
			f.Authentication = result.Authentication
		}
		f.ActionURL = result.ActionURL
		f.QRURL = result.QRURL
		if result.AccountID != "" {
			f.AccountID = result.AccountID
		}
		if result.Secret != nil {
			f.Secret = append(json.RawMessage(nil), result.Secret...)
		}
		if result.RetryAfter > 0 {
			f.RetryAt = s.opts.Now().Add(result.RetryAfter)
		}
		if !result.ExpiresAt.IsZero() && result.ExpiresAt.Before(f.ExpiresAt) {
			f.ExpiresAt = result.ExpiresAt
		}
	}
	if terminal(f.Status) {
		f.Secret = nil
		f.QRURL = ""
		f.ActionURL = ""
	}
	rows[idx] = f
	if err := s.save(rows); err != nil {
		return View{}, err
	}
	view := f.View
	if verifier, ok := s.opts.Drivers[f.Provider].(Verifier); ok {
		view.CanVerify = !terminal(f.Status) && verifier.CanVerify(f)
	}
	return view, nil
}
func safeCall(fn func() (Result, error)) (out Result, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("authentication_driver_failed")
		}
	}()
	return fn()
}
