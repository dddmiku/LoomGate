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
}

func (Input) String() string   { return "authflow.Input[redacted]" }
func (Input) GoString() string { return "authflow.Input[redacted]" }

type View struct {
	ID string `json:"id"`
	Request
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	NextPollAt time.Time `json:"next_poll_at"`
	RetryAt    time.Time `json:"retry_at"`
	ActionURL  string    `json:"action_url,omitempty"`
	QRURL      string    `json:"qr_url,omitempty"`
	ErrorCode  string    `json:"error_code,omitempty"`
	Notice     string    `json:"notice,omitempty"`
	Revision   uint64    `json:"revision"`
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
		out = append(out, f.View)
	}
	return out, nil
}
func (s *Service) Start(ctx context.Context, r Request) (View, error) {
	if r.Provider == "workbuddy" && r.Realm == "" {
		r.Realm = "cn"
	}
	d := s.opts.Drivers[r.Provider]
	if d == nil {
		return View{}, ErrInvalid
	}
	allowed := false
	for _, m := range d.Methods() {
		if m.ID == r.Method && m.Available {
			allowed = true
		}
	}
	if !allowed {
		return View{}, ErrInvalid
	}
	if r.Realm != "" && r.Realm != "cn" && r.Realm != "global" && r.Realm != "trae" {
		return View{}, ErrInvalid
	}
	if r.Provider != "workbuddy" && r.Realm != "" {
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
		if f.Provider == r.Provider && f.Realm == r.Realm && !terminal(f.Status) {
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
	rows = append(rows, f)
	if err := s.save(rows); err != nil {
		s.mu.Unlock()
		return View{}, err
	}
	bounded, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	s.active[f.ID] = cancel
	s.mu.Unlock()
	return s.call(bounded, cancel, f, func() (Result, error) { return d.Start(bounded, r) })
}
func allowed(status, action string) bool {
	switch action {
	case "poll":
		return status == "waiting_scan" || status == "scanned" || status == "verifying" || status == "waiting_authorization"
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
	if len(input.Phone) > 64 || len(input.Code) > 32 || len(input.Callback) > 16384 {
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
	if action == "cancel" {
		if cancel := s.active[id]; cancel != nil {
			cancel()
		}
		if terminal(f.Status) {
			s.mu.Unlock()
			return f.View, nil
		}
		f.Status = "canceled"
		if rows[idx].Status == "working" {
			f.Status = "uncertain"
			f.Notice = "取消请求已发出，进行中的外部操作可能已经执行，请先核验官方状态。"
			f.ErrorCode = "cancellation_unconfirmed"
		}
		f.Secret = nil
		f.QRURL = ""
		f.ActionURL = ""
		f.Revision++
		rows[idx] = f
		err := s.save(rows)
		s.mu.Unlock()
		return f.View, err
	}
	if !allowed(f.Status, action) {
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
	if action == "poll" && now.Before(f.NextPollAt) || action == "send_code" && now.Before(f.RetryAt) {
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
		case "waiting_phone", "waiting_code", "waiting_scan", "scanned", "waiting_authorization", "verifying", "verified", "manual_action", "succeeded", "failed", "expired":
			f.Status = result.Status
		default:
			f.Status = "uncertain"
			result.ErrorCode = "invalid_provider_state"
		}
		f.ErrorCode = result.ErrorCode
		f.Notice = result.Notice
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
	return f.View, nil
}
func safeCall(fn func() (Result, error)) (out Result, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("authentication_driver_failed")
		}
	}()
	return fn()
}
