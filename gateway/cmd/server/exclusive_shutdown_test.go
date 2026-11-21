package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/taskengine"
)

// A request may still read provider state after scheduling has stopped. These
// are real state/vault locks, not a mocked ordering callback.
func TestWorkbenchKeepsExclusiveStateUntilInflightRequestsDrain(t *testing.T) {
	root := t.TempDir()
	w, transport := openScheduleMigrationRuntime(t, root)
	backgroundCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	backgroundDone := make(chan struct{})
	go func() { defer close(backgroundDone); _ = runWorkbench(backgroundCtx, w) }()
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "completed")
	}))
	defer server.Close()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := http.Get(server.URL)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- drainRuntime(ctx, runtimeShutdown{
			stopBackground: stopBackground, backgroundDone: backgroundDone,
			quiesceWorkbench: w.engine.Quiesce,
			closeWorkbench:   func(ctx context.Context) error { return closeWorkbench(ctx, w) },
			shutdownAdmin:    func(context.Context) error { return nil },
			shutdownHTTP:     server.Config.Shutdown,
			closeUsage:       func() error { return nil }, closePool: func() {}, closeStore: func() error { return nil },
		})
	}()
	<-backgroundDone
	if other, err := taskengine.Open(taskengine.Options{Path: filepath.Join(root, "task-engine.json")}); err == nil {
		_ = other.Close()
		t.Error("task engine lock released while an HTTP request was still running")
	}
	for _, name := range []string{"provider-accounts", "task-provider-state"} {
		if other, err := accountvault.Open(filepath.Join(root, name)); err == nil {
			_ = other.Close()
			t.Errorf("%s lock released while an HTTP request was still running", name)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("drain: %v", err)
	}
	<-requestDone
	if other, err := taskengine.Open(taskengine.Options{Path: filepath.Join(root, "task-engine.json")}); err != nil {
		t.Fatalf("drained task engine remained locked: %v", err)
	} else {
		_ = other.Close()
	}
	for _, name := range []string{"provider-accounts", "task-provider-state"} {
		other, err := accountvault.Open(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("drained %s remained locked: %v", name, err)
		}
		_ = other.Close()
	}
	if transport.calls.Load() != 0 {
		t.Fatal("synthetic shutdown contacted a provider")
	}
}
