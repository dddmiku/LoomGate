package taskproviders

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
)

// All network responses are synthetic. The production engine, account module,
// encrypted journal and credential files exercise distinct refresh attempts.
func TestRefreshProofBelongsToItsOwnAttempt(t *testing.T) {
	for _, mode := range []string{"rotated_first_control", "unchanged_first", "unchanged_first_restart"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			p := pool.New("")
			defer p.Close()
			account := keepaliveAccount(t, root, "review-refresh", "cn")
			p.Add(account)
			vaultPath := filepath.Join(root, "secure")
			vault, err := accountvault.Open(vaultPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { vault.Close() }()
			var calls atomic.Int32
			refresh := func(_ context.Context, snapshot *auth.Auth) (auth.TokenUpdate, error) {
				if calls.Add(1) > 1 {
					return auth.TokenUpdate{}, errors.New("synthetic_second_response_lost")
				}
				access := snapshot.AccessToken
				if mode == "rotated_first_control" {
					access = "synthetic-control-rotated"
				}
				return auth.TokenUpdate{AccessToken: access, RefreshToken: snapshot.RefreshToken, ExpiresAt: snapshot.ExpiresAt}, nil
			}
			makeAdapter := func() *Accounts {
				a, err := NewAccounts(p, nil, "workbuddy", time.Hour, AccountsOptions{RefreshVault: vault, Refresh: refresh})
				if err != nil {
					t.Fatal(err)
				}
				return a
			}
			adapter := makeAdapter()
			path := filepath.Join(root, "engine.json")
			engine := keepaliveEngine(t, path, "workbuddy", adapter)
			defer func() { engine.Close() }()
			first := runKeepalive(t, engine, "workbuddy", account.UID)
			if first.Status != taskengine.StatusSucceeded || calls.Load() != 1 {
				t.Fatal("initial accepted refresh control failed")
			}
			lost := runKeepalive(t, engine, "workbuddy", account.UID)
			if lost.Status != taskengine.StatusUncertain || calls.Load() != 2 {
				t.Fatal("second lost response did not enter genuine unknown state")
			}
			if mode == "unchanged_first_restart" {
				engine.Close()
				vault.Close()
				vault, err = accountvault.Open(vaultPath)
				if err != nil {
					t.Fatal(err)
				}
				adapter = makeAdapter()
				engine = keepaliveEngine(t, path, "workbuddy", adapter)
			}
			recheck := runKeepalive(t, engine, "workbuddy", account.UID)
			catalog, err := engine.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{AccountIDs: []string{account.UID}}, "keepalive")
			if err != nil {
				t.Fatal(err)
			}
			pending := false
			for _, task := range catalog.Tasks {
				pending = pending || task.PendingVerification
			}
			t.Logf("mode=%s first=%s lost=%s recheck=%s refresh_calls=%d pending=%t", mode, first.Status, lost.Status, recheck.Status, calls.Load(), pending)
			if calls.Load() != 2 {
				t.Errorf("unknown second refresh was replayed; calls=%d", calls.Load())
			}
			if recheck.Status != taskengine.StatusUncertain {
				t.Errorf("old verified response confirmed a different refresh attempt: status=%s", recheck.Status)
			}
			if !pending {
				t.Error("later unknown marker was cleared using the previous accepted response")
			}
		})
	}
}
