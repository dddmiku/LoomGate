package autoclaw

// The console renders the phone/code form only for the statuses the authflow
// service recognises, and submits the action names send_code / submit_code.
// A driver that invents its own status or action silently produces a dialog
// with no input field — the exact bug this test locks down.

import (
	"context"
	"encoding/json"
	"testing"

	"workbuddy2api/internal/authflow"
)

func TestStartAsksForPhoneWithAFormRenderableStatus(t *testing.T) {
	d := &LoginDriver{}
	got, err := d.Start(context.Background(), authflow.Request{Method: "sms"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "waiting_phone" {
		t.Fatalf("status=%q; the console only renders an input for waiting_phone/waiting_code", got.Status)
	}
	if got.InputMode != "phone" {
		t.Fatalf("input_mode=%q", got.InputMode)
	}
	if len(got.Secret) == 0 {
		t.Fatal("flow secret missing; the phone step needs the persisted intent")
	}
}

func TestPhoneStepAcceptsTheConsoleActionName(t *testing.T) {
	var sentTo string
	d := &LoginDriver{HTTP: stubHTTP(t, &sentTo)}
	flow := authflow.Flow{Secret: mustSecret(t, loginState{DeviceID: loginDeviceID})}
	got, err := d.Step(context.Background(), flow, "send_code", authflow.Input{Phone: "13800000000"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "waiting_code" {
		t.Fatalf("status=%q after a valid phone; want waiting_code", got.Status)
	}
	if got.InputMode != "code" {
		t.Fatalf("input_mode=%q", got.InputMode)
	}
	if sentTo == "" {
		t.Fatal("no verification code was requested from the upstream")
	}
}

func TestInvalidPhoneKeepsAsking(t *testing.T) {
	for _, phone := range []string{"", "123", "abcdefghijk", "2380000000"} {
		d := &LoginDriver{}
		flow := authflow.Flow{Secret: mustSecret(t, loginState{DeviceID: loginDeviceID})}
		got, err := d.Step(context.Background(), flow, "send_code", authflow.Input{Phone: phone})
		if err != nil {
			t.Fatalf("phone %q: %v", phone, err)
		}
		if got.Status != "waiting_phone" {
			t.Fatalf("phone %q: status=%q, want waiting_phone", phone, got.Status)
		}
	}
}

func TestCodeStepAcceptsTheConsoleActionName(t *testing.T) {
	d := &LoginDriver{HTTP: stubHTTP(t, nil)}
	flow := authflow.Flow{Secret: mustSecret(t, loginState{Phone: "13800000000", DeviceID: loginDeviceID})}
	// A non-numeric code must keep asking rather than call upstream.
	got, err := d.Step(context.Background(), flow, "submit_code", authflow.Input{Code: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "waiting_code" {
		t.Fatalf("status=%q for an invalid code; want waiting_code", got.Status)
	}
}

func mustSecret(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
