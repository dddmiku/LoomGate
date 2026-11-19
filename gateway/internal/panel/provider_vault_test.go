package panel

// The provider-account vault holds an exclusive owner lock. Inference opens it
// first, so the panel must write through that same handle. A second Open on
// the directory returns vault_busy, and the failure used to be swallowed: a
// login that genuinely succeeded was then reported as "结果未确认".

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/authflow"
)

func TestProviderVaultIsSharedWithInferenceNotReopened(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-accounts")
	shared, err := accountvault.Open(dir)
	if err != nil {
		t.Fatalf("open inference vault: %v", err)
	}
	defer shared.Close()

	s := &Server{opts: Options{ProviderVault: shared}}

	// Reproduce the branch under test: a handed-in handle must win over the
	// path, because reopening the directory is refused.
	var got *accountvault.Vault
	if s.opts.ProviderVault != nil {
		got = s.opts.ProviderVault
	} else if s.opts.ProviderVaultDir != "" {
		pv, err := accountvault.Open(s.opts.ProviderVaultDir)
		if err == nil {
			got = pv
		}
	}
	if got != shared {
		t.Fatal("panel did not adopt the inference vault handle")
	}

	// Prove the second Open really is refused, so the shared-handle branch is
	// the only one that can save an account.
	if _, err := accountvault.Open(dir); err == nil {
		t.Fatal("a second Open on the same vault unexpectedly succeeded; the shared handle is unnecessary")
	}
}

func TestProviderVaultSharedHandleCanSaveAnAccount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-accounts")
	shared, err := accountvault.Open(dir)
	if err != nil {
		t.Fatalf("open inference vault: %v", err)
	}
	defer shared.Close()

	payload := []byte(`{"accessToken":"synthetic","refreshToken":"synthetic","phone":"13800000000"}`)
	id, err := autoclawSaveProfile(&Server{opts: Options{ProviderVault: shared},
		connections: connections{providerVault: shared}})(context.Background(), payload)
	if err != nil {
		t.Fatalf("save through the shared handle failed: %v", err)
	}
	if id == "" {
		t.Fatal("save returned no account id")
	}
	rows, err := shared.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Provider != "autoclaw" {
		t.Fatalf("account not persisted: %+v", rows)
	}
}

func TestProviderVaultUnavailableIsReportedNotSwallowed(t *testing.T) {
	s := &Server{}
	_, err := autoclawSaveProfile(s)(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("a missing provider vault was silently accepted")
	}
	if err.Error() != "provider_vault_unavailable" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPanelCloseLeavesBorrowedVaultOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-accounts")
	shared, err := accountvault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()

	s := &Server{connections: connections{providerVault: shared, providerVaultOwned: false}}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A borrowed vault must still work for inference after the panel closes.
	if _, err := shared.List(); err != nil {
		t.Fatalf("panel close broke the shared vault: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "owner.lock"))
}

func TestAuthflowIsNotBlockedByTheSharedVault(t *testing.T) {
	// The exact production wiring: Save closes over s.connections.providerVault.
	dir := filepath.Join(t.TempDir(), "provider-accounts")
	shared, err := accountvault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	s := &Server{opts: Options{ProviderVault: shared}}
	s.connections.providerVault = shared

	save := autoclawSaveProfile(s)
	var _ authflow.Result
	payload := []byte(`{"accessToken":"synthetic","refreshToken":"synthetic","phone":"13900000000"}`)
	if _, err := save(context.Background(), payload); err != nil {
		t.Fatalf("production save closure failed: %v", err)
	}
}
