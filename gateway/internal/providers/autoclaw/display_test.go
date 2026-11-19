package autoclaw

// The account list and the login form have opposite needs. The list is a
// persistent page anyone looking over the shoulder can read, so it shows a
// masked number; the form is where the operator checks what they typed, so it
// must not hide the digits. Confusing the two produces either a private
// number on screen or an unverifiable input.

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
)

func sourceWithPhone(t *testing.T, phone string) Source {
	t.Helper()
	vault, err := accountvault.Open(filepath.Join(t.TempDir(), "provider-accounts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vault.Close() })
	profile, _ := json.Marshal(stored{Phone: phone, AccessToken: "synthetic", RefreshToken: "synthetic"})
	if _, err := vault.Put(accountvault.Account{ID: "autoclaw-" + phone, Provider: "autoclaw", Enabled: true}, profile, 0); err != nil {
		t.Fatal(err)
	}
	return Source{Vault: vault}
}

func TestAccountListMasksThePhoneNumber(t *testing.T) {
	const phone = "15730229941"
	mobile, _, _, _, ok := sourceWithPhone(t, phone).Display("autoclaw-" + phone)
	if !ok {
		t.Fatal("display did not resolve the account")
	}
	if mobile == phone {
		t.Fatal("the account list prints the full phone number")
	}
	if mobile != "157****9941" {
		t.Fatalf("masked form is %q want 157****9941", mobile)
	}
	if !strings.HasPrefix(mobile, "157") || !strings.HasSuffix(mobile, "9941") {
		t.Fatal("masking lost the digits that tell two accounts apart")
	}
}

// The SMS notice is a status message and remains masked; only the editable
// login form exposes the phone number and code for typo checking.
func TestSmsNoticeKeepsThePhoneMasked(t *testing.T) {
	const phone = "15730229941"
	d := &LoginDriver{HTTP: &http.Client{Transport: stubbedOK{}}, InviteCode: "-"}
	got, err := d.Step(context.Background(), flowWithSecret(mustSecretRaw(t, loginState{Phone: phone, DeviceID: newDeviceID()})), "resend", authflow.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Notice, "157****9941") || strings.Contains(got.Notice, phone) {
		t.Fatalf("notice must keep the phone masked: %q", got.Notice)
	}
}

// A short or foreign number must pass through rather than panic on slicing.
func TestMaskPhoneHandlesNonStandardLengths(t *testing.T) {
	for _, v := range []string{"", "123", "1234567890123"} {
		if got := maskPhone(v); got != v {
			t.Fatalf("maskPhone(%q)=%q; non-11-digit values must be returned unchanged", v, got)
		}
	}
	if got := maskPhone("15730229941"); got != "157****9941" {
		t.Fatalf("maskPhone=%q", got)
	}
}
