package hotupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/version"
)

// This is a real second process opening the same task-engine and BOTH vault
// directories. No provider adapter or external request is involved.
func TestSequentialRuntimeHelper(t *testing.T) {
	root := os.Getenv("LOOMGATE_TEST_SWITCH_ROOT")
	if root == "" {
		t.Skip("sequential update helper only")
	}
	self, _ := os.Executable()
	old := filepath.Clean(self) == filepath.Clean(os.Getenv("LOOMGATE_TEST_OLD_BINARY"))
	mode := os.Getenv("LOOMGATE_TEST_SWITCH_FAIL")
	if mode == "both" || mode == "crash" && !old {
		os.Exit(3)
	}
	engine, err := taskengine.Open(taskengine.Options{Path: filepath.Join(root, "task-engine.json")})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper_task_state_busy")
		os.Exit(4)
	}
	defer engine.Close()
	for _, name := range []string{"provider-accounts", "task-provider-state"} {
		vault, err := accountvault.Open(filepath.Join(root, name))
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper_vault_busy")
			os.Exit(4)
		}
		defer vault.Close()
	}
	listener, inherited, err := Listen("127.0.0.1:0")
	if err != nil || !inherited {
		os.Exit(5)
	}
	label, activeVersion := "candidate", "v99.0.2"
	if old {
		label, activeVersion = "rollback", version.Version
	}
	if mode == "health" && !old {
		activeVersion = "v99.0.0"
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("X-Service", "workbuddy2api")
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "workbuddy2api", "version": activeVersion, "commit": "synthetic-fixture"})
		} else if strings.HasSuffix(r.URL.Path, "/login") && r.URL.Path != os.Getenv("LOOMGATE_TEST_PANEL_PREFIX")+"/login" {
			w.WriteHeader(http.StatusNotFound)
		} else {
			_, _ = io.WriteString(w, label)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	if fd, ok := InheritedAdminFD(); ok {
		admin, err := ListenerFromFD(fd, "sequential-admin")
		if err != nil {
			os.Exit(5)
		}
		go func() {
			_ = (&http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "admin-"+label) })}).Serve(admin)
		}()
	}
	if NotifyReady() != nil {
		os.Exit(6)
	}
	stop := filepath.Join(root, "stop-helper")
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(stop); err == nil {
			_ = server.Close()
			_ = os.Remove(stop)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSequentialUpdateDrainsRealOwnersAndAutomaticallyRollsBack(t *testing.T) {
	requireListenerInheritance(t)
	native, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"success", "prefix", "crash", "health", "both", "commit-io"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			enginePath := filepath.Join(root, "task-engine.json")
			engine, err := taskengine.Open(taskengine.Options{Path: enginePath})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			var vaults []*accountvault.Vault
			for _, name := range []string{"provider-accounts", "task-provider-state"} {
				vault, err := accountvault.Open(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				defer vault.Close()
				vaults = append(vaults, vault)
			}
			t.Setenv("LOOMGATE_TEST_SWITCH_ROOT", root)
			t.Setenv("LOOMGATE_TEST_SWITCH_FAIL", mode)
			prefix := "/admin"
			if mode == "prefix" {
				prefix = "/custom-control"
			}
			t.Setenv("LOOMGATE_TEST_PANEL_PREFIX", prefix)
			old, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("LOOMGATE_TEST_OLD_BINARY", old)
			m, downloads := signedStagingManager(t, true, native)
			m.opts.Binary, m.opts.Args = old, []string{"-test.run=^TestSequentialRuntimeHelper$", "-test.timeout=75s"}
			m.opts.AdminPrefix = prefix
			if err = writeCurrentPointer(m.opts.Dir, old); err != nil {
				t.Fatal(err)
			}
			started, release, draining := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var flushed atomic.Bool
			running := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "before\n")
				w.(http.Flusher).Flush()
				close(started)
				<-release
				_, _ = io.WriteString(w, "after\n")
				flushed.Store(true)
			}))
			defer running.Close()
			m.opts.Listener = running.Listener
			adminPath := filepath.Join(os.TempDir(), fmt.Sprintf("loomgate-switch-%d-%s.sock", os.Getpid(), mode))
			admin, err := net.Listen("unix", adminPath)
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(adminPath)
			adminServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "old-admin") })}
			go func() { _ = adminServer.Serve(admin) }()
			defer adminServer.Close()
			m.opts.AdminListener = admin
			address := running.Listener.Addr().String()
			request := make(chan string, 1)
			go func() {
				response, err := http.Get(running.URL)
				if err != nil {
					request <- "request-failed"
					return
				}
				defer response.Body.Close()
				raw, _ := io.ReadAll(response.Body)
				request <- string(raw)
			}()
			<-started
			var switched, fatal atomic.Int32
			m.opts.OnSwitched = func() { switched.Add(1) }
			m.opts.OnSwitchFailed = func() { fatal.Add(1) }
			if mode == "commit-io" {
				m.journalWriter = func(dir string, j switchJournal) error {
					if j.State == StateIdle {
						return errors.New("synthetic_commit_journal_io_failure")
					}
					return writeSwitchJournal(dir, j)
				}
				m.pointerWriter = func(dir, path string) error {
					if path == old {
						return errors.New("synthetic_rollback_pointer_io_failure")
					}
					return writeCurrentPointer(dir, path)
				}
			}
			m.opts.PrepareSwitch = func(ctx context.Context) error {
				engine.Quiesce()
				close(draining)
				err := running.Config.Shutdown(ctx)
				if adminErr := adminServer.Shutdown(ctx); err == nil {
					err = adminErr
				}
				if !flushed.Load() {
					t.Error("old SSE and final state were not drained")
				}
				if closeErr := engine.Drain(ctx); err == nil {
					err = closeErr
				}
				for _, vault := range vaults {
					if closeErr := vault.Close(); err == nil {
						err = closeErr
					}
				}
				return err
			}
			done := make(chan error, 1)
			go func() { _, err := m.Apply(t.Context(), ""); done <- err }()
			<-draining
			if other, err := taskengine.Open(taskengine.Options{Path: enginePath}); err == nil {
				other.Close()
				t.Error("old ownership released before SSE finished")
			}
			close(release)
			applyErr := <-done
			if body := <-request; body != "before\nafter\n" {
				t.Fatalf("old SSE interrupted: %q", body)
			}
			if downloads.Load() != 1 {
				t.Fatal("one operation downloaded more than once")
			}
			if mode == "both" {
				if applyErr == nil || fatal.Load() != 1 || switched.Load() != 0 {
					t.Fatal("two failed processes reported a ready successor")
				}
			} else {
				if switched.Load() != 1 || fatal.Load() != 0 {
					t.Fatal("successor was not acknowledged exactly once")
				}
				want := "candidate"
				if mode != "success" && mode != "prefix" {
					want = "rollback"
				}
				if got := dialBody(t, address); got != want {
					t.Fatalf("actual successor=%q want %q", got, want)
				}
				client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", adminPath)
				}}, Timeout: time.Second}
				response, err := client.Get("http://localhost/")
				if err != nil {
					t.Fatal("sequential switch lost the admin socket path")
				}
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				client.CloseIdleConnections()
				if string(body) != "admin-"+want {
					t.Fatal("admin socket did not reach the actual successor")
				}
				stop := filepath.Join(root, "stop-helper")
				t.Cleanup(func() {
					_ = os.WriteFile(stop, []byte("stop"), 0600)
					for i := 0; i < 200; i++ {
						if _, err := os.Stat(stop); os.IsNotExist(err) {
							return
						}
						time.Sleep(10 * time.Millisecond)
					}
					t.Error("successor fixture did not exit")
				})
			}
			if mode == "success" || mode == "prefix" {
				if applyErr != nil || CurrentBinary(m.opts.Dir) == old {
					t.Fatal("healthy target was not committed")
				}
				for name := range runtimeFiles {
					if _, err := os.Stat(filepath.Join(filepath.Dir(CurrentBinary(m.opts.Dir)), filepath.FromSlash(name))); err != nil {
						t.Fatal("complete runtime was pruned")
					}
				}
			} else if mode == "commit-io" {
				pointed := CurrentBinary(m.opts.Dir)
				if applyErr == nil || pointed == "" || pointed == old {
					t.Fatal("fixture did not reach post-pointer journal failure")
				}
				if _, err := os.Stat(pointed); err != nil {
					t.Fatal("deferred cleanup deleted the runtime still referenced by current")
				}
				if !strings.Contains(m.Status().LastError, "指针恢复失败") {
					t.Fatal("rollback pointer failure was presented as complete recovery")
				}
			} else {
				if applyErr == nil || CurrentBinary(m.opts.Dir) != old {
					t.Fatal("failed candidate changed the durable previous-version pointer")
				}
				restarted := NewManager(Options{Dir: m.opts.Dir}).Status()
				if restarted.State != StateFailed || restarted.TargetVersion != "v99.0.2" || !strings.Contains(restarted.LastError, "失败") && !strings.Contains(restarted.LastError, "未通过") {
					t.Fatal("rollback failure reason was lost across a process restart")
				}
			}
		})
	}
}
