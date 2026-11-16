package taskproviders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskengine"
)

func TestRefreshAttemptCrashHelper(t *testing.T) {
	root := os.Getenv("LOOMGATE_REFRESH_CRASH_ROOT")
	if root == "" {
		t.Skip("isolated subprocess helper")
	}
	p := pool.New("")
	defer p.Close()
	a := keepaliveAccount(t, root, "synthetic-crash", "cn")
	p.Add(a)
	v, err := accountvault.Open(filepath.Join(root, "secure"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var calls atomic.Int32
	adapter, err := NewAccounts(p, nil, "workbuddy", time.Hour, AccountsOptions{RefreshVault: v, Refresh: func(ctx context.Context, s *auth.Auth) (auth.TokenUpdate, error) {
		if calls.Add(1) == 1 {
			return auth.TokenUpdate{AccessToken: s.AccessToken, RefreshToken: s.RefreshToken, ExpiresAt: s.ExpiresAt}, nil
		}
		if err := os.WriteFile(filepath.Join(root, "entered"), []byte("second-attempt-entered"), 0600); err != nil {
			return auth.TokenUpdate{}, err
		}
		<-ctx.Done()
		return auth.TokenUpdate{}, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	e := keepaliveEngine(t, filepath.Join(root, "engine.json"), "workbuddy", adapter)
	defer e.Close()
	if runKeepalive(t, e, "workbuddy", a.UID).Status != taskengine.StatusSucceeded {
		t.Fatal("control failed")
	}
	if _, err = e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "keepalive", AccountIDs: []string{a.UID}}); err != nil {
		t.Fatal(err)
	}
	<-time.After(time.Minute)
}

func TestRefreshOldProofSurvivesNeitherCrashNorLegacyJournal(t *testing.T) {
	for _, mode := range []string{"prepared_crash", "legacy_verified", "legacy_unverified", "old_bound_proof"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestRefreshAttemptCrashHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "LOOMGATE_REFRESH_CRASH_ROOT="+root)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(root, "entered")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("helper not entered")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			cmd.Wait()
			raw, err := os.ReadFile(filepath.Join(root, "workbuddy-synthetic-crash.json"))
			if err != nil {
				t.Fatal(err)
			}
			a, err := auth.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			a.FilePath = filepath.Join(root, "workbuddy-synthetic-crash.json")
			p := pool.New("")
			defer p.Close()
			p.Add(a)
			v, err := accountvault.Open(filepath.Join(root, "secure"))
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			var calls atomic.Int32
			adapter, err := NewAccounts(p, nil, "workbuddy", time.Hour, AccountsOptions{RefreshVault: v, Refresh: func(context.Context, *auth.Auth) (auth.TokenUpdate, error) {
				calls.Add(1)
				return auth.TokenUpdate{}, errors.New("must_not_replay")
			}})
			if err != nil {
				t.Fatal(err)
			}
			var module *keepaliveModule
			for _, m := range adapter.Modules {
				if k, ok := m.(*keepaliveModule); ok {
					module = k
				}
			}
			record, revision, err := module.load(a)
			if err != nil || record.State != "prepared" || record.Verified {
				t.Fatal("write-ahead journal missing")
			}
			if mode != "prepared_crash" {
				record.State = "accepted"
				record.After = record.Before
				record.Update = auth.TokenUpdate{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ExpiresAt: a.ExpiresAt}
				record.Verified = mode != "legacy_unverified"
				if mode == "old_bound_proof" {
					record.Reference = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				} else {
					record.Version = 0
					record.Reference = ""
					record.State = ""
				}
				if _, err = module.save(record, revision); err != nil {
					t.Fatal(err)
				}
			}
			e := keepaliveEngine(t, filepath.Join(root, "engine.json"), "workbuddy", adapter)
			defer e.Close()
			before, _ := os.ReadFile(a.FilePath)
			beforeVault, _ := os.ReadFile(filepath.Join(root, "secure", refreshRecordID("workbuddy", a.UID)+".account"))
			if _, err = e.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{}, "keepalive"); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(a.FilePath)
			afterVault, _ := os.ReadFile(filepath.Join(root, "secure", refreshRecordID("workbuddy", a.UID)+".account"))
			if !bytes.Equal(before, after) || !bytes.Equal(beforeVault, afterVault) {
				t.Fatal("catalog wrote credentials or journal")
			}
			if r := runKeepalive(t, e, "workbuddy", a.UID); r.Status != taskengine.StatusUncertain || calls.Load() != 0 {
				t.Fatal("crashed attempt reused old proof or replayed")
			}
			after, _ = os.ReadFile(a.FilePath)
			if !bytes.Equal(before, after) {
				t.Fatal("unmatched proof rewrote credentials")
			}
		})
	}
}

func TestRefreshCancellationKeepsOwnAcceptedResponseForRecovery(t *testing.T) {
	root := t.TempDir()
	p := pool.New("")
	defer p.Close()
	a := keepaliveAccount(t, root, "synthetic-cancel", "cn")
	p.Add(a)
	v, err := accountvault.Open(filepath.Join(root, "secure"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	adapter, err := NewAccounts(p, nil, "workbuddy", time.Hour, AccountsOptions{RefreshVault: v, Refresh: func(_ context.Context, s *auth.Auth) (auth.TokenUpdate, error) {
		calls.Add(1)
		close(entered)
		<-release
		return auth.TokenUpdate{AccessToken: s.AccessToken, RefreshToken: s.RefreshToken, ExpiresAt: s.ExpiresAt}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	e := keepaliveEngine(t, filepath.Join(root, "engine.json"), "workbuddy", adapter)
	defer e.Close()
	r, err := e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "keepalive", AccountIDs: []string{a.UID}})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if !e.Cancel(r.ID) {
		t.Fatal("cancel missing")
	}
	close(release)
	if waitModuleRun(t, e, r.ID).Status != taskengine.StatusUncertain {
		t.Fatal("canceled accepted attempt completed early")
	}
	if runKeepalive(t, e, "workbuddy", a.UID).Status != taskengine.StatusSucceeded || calls.Load() != 1 {
		t.Fatal("own accepted response not recovered without network")
	}
	raw, _ := os.ReadFile(filepath.Join(root, "engine.json"))
	var persisted map[string]any
	if json.Unmarshal(raw, &persisted) != nil {
		t.Fatal("invalid persisted state")
	}
	if len(persisted["uncertain"].(map[string]any)) != 0 {
		t.Fatal("own proof did not reconcile")
	}
}
