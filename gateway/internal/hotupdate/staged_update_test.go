package hotupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/version"
)

// Keep the real task/vault ownership while applying a fully signed runtime.
// The previous readiness protocol attempted a second owner before releasing
// these locks, so an otherwise valid upgrade could never finish.
func TestStagedUpdatePreservesExclusiveRuntimeAndCompleteBundle(t *testing.T) {
	for _, bundle := range []bool{false, true} {
		t.Run(map[bool]string{false: "binary", true: "bundle"}[bundle], func(t *testing.T) {
			manager, downloads := signedStagingManager(t, bundle)
			root := t.TempDir()
			enginePath := filepath.Join(root, "task-engine.json")
			engine, err := taskengine.Open(taskengine.Options{Path: enginePath})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			vault, err := accountvault.Open(filepath.Join(root, "provider-accounts"))
			if err != nil {
				t.Fatal(err)
			}
			defer vault.Close()
			oldPath := filepath.Join(manager.opts.Dir, "running-version")
			if err = os.WriteFile(oldPath, []byte("fixture old runtime"), 0700); err != nil {
				t.Fatal(err)
			}
			if err = writeCurrentPointer(manager.opts.Dir, oldPath); err != nil {
				t.Fatal(err)
			}
			status, err := manager.Apply(t.Context(), "")
			if err != nil {
				t.Fatalf("valid update was rejected: %v", err)
			}
			if !status.RestartRequired || status.StagedVersion != "v99.0.2" || status.UpdateReady || status.State != StateIdle {
				t.Fatal("staged update was presented as active or downloadable again")
			}
			checked, err := manager.Check(t.Context())
			if err != nil || !checked.RestartRequired || checked.StagedVersion != "v99.0.2" || checked.UpdateReady {
				t.Fatal("checking for updates lost the pending restart state")
			}
			staged := CurrentBinary(manager.opts.Dir)
			if staged == "" || staged == oldPath {
				t.Fatal("next-start pointer not committed")
			}
			if _, err = os.Stat(oldPath); err != nil {
				t.Fatal("running rollback file was removed")
			}
			if bundle {
				for name := range runtimeFiles {
					if _, err = os.Stat(filepath.Join(filepath.Dir(staged), filepath.FromSlash(name))); err != nil {
						t.Fatal("committed runtime was removed by deferred cleanup")
					}
				}
			}
			if other, err := taskengine.Open(taskengine.Options{Path: enginePath}); err == nil {
				other.Close()
				t.Fatal("update released the running task engine lock")
			}
			if other, err := accountvault.Open(filepath.Join(root, "provider-accounts")); err == nil {
				other.Close()
				t.Fatal("update released the running vault lock")
			}
			resp, err := http.Get("http://" + manager.opts.Listener.Addr().String())
			if err != nil {
				t.Fatal("running HTTP service stopped")
			}
			defer resp.Body.Close()
			text, _ := io.ReadAll(resp.Body)
			if string(text) != "old runtime serving" {
				t.Fatal("running HTTP service changed before restart")
			}
			if _, err = manager.Apply(t.Context(), ""); err == nil || downloads.Load() != 1 || CurrentBinary(manager.opts.Dir) != staged {
				t.Fatal("duplicate apply downloaded or changed a pending update")
			}
		})
	}
}

func TestStagedUpdatePointerFailureKeepsOldService(t *testing.T) {
	manager, _ := signedStagingManager(t, true)
	if err := os.Mkdir(filepath.Join(manager.opts.Dir, "current"), 0700); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Apply(t.Context(), "")
	if err == nil || status.State != StateFailed || status.RestartRequired {
		t.Fatal("failed pointer commit reported a staged update")
	}
	entries, _ := os.ReadDir(manager.opts.Dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "runtime-") {
			t.Fatal("uncommitted runtime directory leaked")
		}
	}
}

func signedStagingManager(t *testing.T, bundle bool, native ...[]byte) (*Manager, *atomic.Int32) {
	t.Helper()
	content := []byte("fixture is deliberately not executable")
	if len(native) > 0 {
		content = native[0]
	}
	if bundle {
		var err error
		content, err = os.ReadFile(testBundle(t, func(m *runtimeManifest, files map[string][]byte) {
			m.Version = "v99.0.2"
			if len(native) > 0 {
				files["wb2api"] = native[0]
				sum := sha256.Sum256(native[0])
				m.Files["wb2api"] = hex.EncodeToString(sum[:])
			}
		}, nil))
		if err != nil {
			t.Fatal(err)
		}
	}
	name, err := assetName()
	if err != nil {
		t.Fatal(err)
	}
	if bundle && bundleSupported() {
		name = runtimeAssetName()
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte(digest + "  " + name + "\n")
	signature := hex.EncodeToString(ed25519.Sign(private, sums))
	oldKey := version.ReleaseKey
	version.ReleaseKey = hex.EncodeToString(public)
	t.Cleanup(func() { version.ReleaseKey = oldKey })
	var downloads atomic.Int32
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/fixture/loomgate":
			io.WriteString(w, `{"private":false}`)
		case "/repos/fixture/loomgate/releases/latest":
			json.NewEncoder(w).Encode(map[string]any{"tag_name": "v99.0.2", "assets": []any{
				map[string]any{"name": name, "size": len(content), "digest": "sha256:" + digest, "browser_download_url": api.URL + "/asset"},
				map[string]any{"name": sumsAssetName, "browser_download_url": api.URL + "/sums"},
				map[string]any{"name": sumsSigAssetName, "browser_download_url": api.URL + "/signature"},
			}})
		case "/asset":
			downloads.Add(1)
			w.Write(content)
		case "/sums":
			w.Write(sums)
		case "/signature":
			io.WriteString(w, signature)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	running := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "old runtime serving") }))
	t.Cleanup(running.Close)
	manager := NewManager(Options{Enabled: true, Bundle: bundle, Repo: "fixture/loomgate", Dir: t.TempDir(), Proxy: "-", Listener: running.Listener, RequireRestart: true, OnSwitched: func() { t.Error("staging must not shut down the running instance") }})
	manager.client.APIBase = api.URL
	return manager, &downloads
}
