package authflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type heldCancelDriver struct {
	readVerifierStub
	entered, release chan struct{}
}

func (d *heldCancelDriver) Cancel(context.Context, Flow) error {
	close(d.entered)
	<-d.release
	return nil
}
func TestCancelKeepsIdentityWhenAnotherStartPrunesOldFlows(t *testing.T) {
	d := &heldCancelDriver{entered: make(chan struct{}), release: make(chan struct{})}
	s, _, _, now := testService(t, d)
	rows := make([]Flow, 16)
	for i := range rows {
		rows[i] = Flow{View: View{ID: fmt.Sprintf("synthetic-%d", i), Request: Request{Provider: "synthetic", Method: "qr"}, Status: "succeeded", ExpiresAt: now.Add(time.Hour), Revision: 1}}
	}
	rows[15].Status = "waiting_scan"
	rows[15].Secret = json.RawMessage(`{"synthetic":true}`)
	if err := s.save(rows); err != nil {
		t.Fatal(err)
	}
	s.opts.Drivers["other"] = &driverStub{}
	done := make(chan error, 1)
	go func() { _, err := s.Step(context.Background(), "synthetic-15", "cancel", Input{}); done <- err }()
	<-d.entered
	newFlow, err := s.Start(context.Background(), Request{Provider: "other", Method: "qr"})
	if err != nil {
		close(d.release)
		t.Fatal(err)
	}
	close(d.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	views, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range views {
		if f.ID == newFlow.ID && f.Status != newFlow.Status {
			t.Fatal("cancellation overwrote new flow")
		}
		if f.ID == "synthetic-15" {
			found = f.Status == "canceled"
		}
	}
	if !found {
		t.Fatal("cancellation lost its original flow")
	}
}

type readVerifierStub struct {
	driverStub
	reads atomic.Int32
}

func (d *readVerifierStub) CanVerify(f Flow) bool { return f.Method == "qr" }
func (d *readVerifierStub) Verify(context.Context, Flow) (Result, error) {
	d.reads.Add(1)
	return Result{Status: "verified"}, nil
}

func TestExplicitVerificationGuards(t *testing.T) {
	for _, status := range []string{"waiting_authorization", "manual_action", "uncertain", "verified", "working", "succeeded", "failed", "expired", "canceled"} {
		t.Run(status, func(t *testing.T) {
			d := &readVerifierStub{}
			s, _, _, now := testService(t, d)
			flow := Flow{View: View{ID: "synthetic-verify", Request: Request{Provider: "synthetic", Method: "qr"}, Status: status, ExpiresAt: now.Add(time.Hour), Revision: 1}}
			if err := s.save([]Flow{flow}); err != nil {
				t.Fatal(err)
			}
			want := !terminal(status) && status != "working"
			rows, err := s.List()
			if err != nil || rows[0].CanVerify != want {
				t.Fatal("verification availability disagrees with state")
			}
			_, err = s.Step(context.Background(), flow.ID, "verify", Input{})
			if want && (err != nil || d.reads.Load() != 1) || !want && (err != ErrInvalid || d.reads.Load() != 0) {
				t.Fatal("verification crossed state boundary")
			}
		})
	}
}

func TestVerificationRejectsSensitiveInputAndRespectsCooldown(t *testing.T) {
	d := &readVerifierStub{}
	s, _, _, now := testService(t, d)
	f, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "qr"})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []Input{{Phone: "synthetic-phone"}, {Code: "000000"}, {Callback: "https://example.invalid/callback"}} {
		if _, err := s.Step(context.Background(), f.ID, "verify", input); err != ErrInvalid {
			t.Fatal("verification accepted sensitive input")
		}
	}
	if _, err := s.Step(context.Background(), f.ID, "verify", Input{}); err != ErrBusy {
		t.Fatal("verification bypassed cooldown")
	}
	if d.reads.Load() != 0 {
		t.Fatal("rejected verification called driver")
	}
	*now = now.Add(3 * time.Second)
	result, err := s.Step(context.Background(), f.ID, "verify", Input{})
	if err != nil || result.Status != "verified" || d.reads.Load() != 1 {
		t.Fatal("read-only verification did not resume")
	}
}

func TestCanceledCommitCannotOverlapVerificationOrClaimNoSave(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var writes atomic.Int32
	d := &readVerifierStub{driverStub: driverStub{
		start: func(context.Context, Request) (Result, error) {
			return Result{Status: "verified", Secret: json.RawMessage(`{"synthetic":true}`)}, nil
		},
		step: func(context.Context, Flow, string, Input) (Result, error) {
			close(started)
			<-release
			writes.Add(1)
			return Result{Status: "succeeded", AccountID: "synthetic-account"}, nil
		},
	}}
	s, _, _, now := testService(t, d)
	f, err := s.Start(context.Background(), Request{Provider: "synthetic", Method: "qr"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.Step(context.Background(), f.ID, "commit", Input{}) }()
	<-started
	result, err := s.Step(context.Background(), f.ID, "cancel", Input{})
	if err != nil || result.Status != "uncertain" {
		t.Fatal("in-flight commit was falsely canceled")
	}
	result, err = s.Step(context.Background(), f.ID, "cancel", Input{})
	if err != nil || result.Status != "uncertain" {
		t.Fatal("repeated cancel cleared an active unknown operation")
	}
	views, _ := s.List()
	if views[0].CanVerify {
		t.Fatal("canceled active driver advertises recovery")
	}
	*now = now.Add(3 * time.Second)
	_, err = s.Step(context.Background(), f.ID, "verify", Input{})
	if err != ErrBusy || d.reads.Load() != 0 {
		t.Fatal("recovery overlapped the canceled operation")
	}
	close(release)
	<-done
	rows, _ := s.List()
	if writes.Load() != 1 || rows[0].Status != "uncertain" || rows[0].ErrorCode != "cancellation_unconfirmed" {
		t.Fatal("late account save was hidden by cancellation")
	}
}
