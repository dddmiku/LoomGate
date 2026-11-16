package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRefreshWaiterCanCancelWithoutInterruptingOwner(t *testing.T) {
	a := &Auth{UID: "synthetic", AccessToken: "synthetic-old"}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.RefreshOnce(func(*Auth) (TokenUpdate, error) {
			close(started)
			<-release
			return TokenUpdate{AccessToken: "synthetic-new"}, nil
		})
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.RefreshOnceContext(ctx, func(*Auth) (TokenUpdate, error) { t.Error("waiter issued second refresh"); return TokenUpdate{}, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal("waiter cancellation lost")
	}
	close(release)
	if err = <-done; err != nil || a.Snapshot().AccessToken != "synthetic-new" {
		t.Fatal("owner disrupted")
	}
}
func TestRefreshCommitRejectsExternalFileReplacement(t *testing.T) {
	a := &Auth{UID: "synthetic", AccessToken: "synthetic-old", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), FilePath: filepath.Join(t.TempDir(), "workbuddy-fixture.json")}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	before := a.CredentialRevision()
	replacement := a.Snapshot()
	replacement.AccessToken = "synthetic-new-login"
	if err := replacement.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	if err := a.CommitTokenUpdate(before, TokenUpdate{AccessToken: "synthetic-stale", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}); err == nil {
		t.Fatal("external replacement overwritten")
	}
	persisted, err := a.PersistedRevision()
	if err != nil || persisted != replacement.CredentialRevision() || a.Snapshot().AccessToken != "synthetic-old" {
		t.Fatal("CAS changed memory or disk")
	}
}
