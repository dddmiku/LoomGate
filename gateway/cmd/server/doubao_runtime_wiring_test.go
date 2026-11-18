package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/providers/doubao"
	"workbuddy2api/internal/upstream"
)

// doubaoWiringTransport fails the test if opening the workbench reaches any
// upstream. The Doubao runtime selection is a local wiring decision only.
type doubaoWiringTransport struct{ calls atomic.Int32 }

func (tr *doubaoWiringTransport) RoundTrip(*http.Request) (*http.Response, error) {
	tr.calls.Add(1)
	return nil, errors.New("synthetic_upstream_must_not_be_called")
}

func writeSyntheticBridge(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "doubao-runtime")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name := range map[string]bool{"bridge.py": true, "managed.py": true} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("# synthetic runtime placeholder; never executed here\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "bridge.py")
}

func executablePath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// openDoubaoWiringRuntime opens the real workbench with a synthetic upstream
// transport and an explicit host platform. It starts no engine loop, client,
// login flow or upstream request.
func openDoubaoWiringRuntime(t *testing.T, stateDir, goos, python, helper, port string) (*workbenchRuntime, *doubaoWiringTransport) {
	t.Helper()
	return openDoubaoWiringRuntimeAt(t, stateDir, goos, python, helper, port, "")
}

// openDoubaoWiringRuntimeAt additionally pins the packaged runtime root used by
// auto-discovery. An empty runtimeRoot keeps discovery away from any real
// packaged runtime on this host.
func openDoubaoWiringRuntimeAt(t *testing.T, stateDir, goos, python, helper, port, runtimeRoot string) (*workbenchRuntime, *doubaoWiringTransport) {
	t.Helper()
	for _, key := range []string{
		"LOOMGATE_DOUBAO_DESKTOP_PYTHON",
		"LOOMGATE_DOUBAO_DESKTOP_HELPER",
		"LOOMGATE_DOUBAO_DESKTOP_PORT",
		"LOOMGATE_DOUBAO_EXECUTABLE",
		"LOOMGATE_DOUBAO_AUTH_PROFILE",
		"WB2A_PYTHON",
		"WB2API_RUNTIME_DIR",
	} {
		t.Setenv(key, "")
	}
	if runtimeRoot == "" {
		runtimeRoot = t.TempDir()
	}
	t.Setenv("WB2API_RUNTIME_DIR", runtimeRoot)
	if python != "" {
		t.Setenv("LOOMGATE_DOUBAO_DESKTOP_PYTHON", python)
	}
	if helper != "" {
		t.Setenv("LOOMGATE_DOUBAO_DESKTOP_HELPER", helper)
	}
	if port != "" {
		t.Setenv("LOOMGATE_DOUBAO_DESKTOP_PORT", port)
	}
	p := pool.New("")
	t.Cleanup(func() { p.Close() })
	transport := &doubaoWiringTransport{}
	up := upstream.New()
	up.HTTP.Transport = transport
	up.ChatHTTP.Transport = transport
	w, _, err := openWorkbenchFor(stateDir, p, up, goos)
	if err != nil {
		t.Fatalf("open workbench for %s: %v", goos, err)
	}
	t.Cleanup(func() {
		_ = w.engine.Close()
		_ = w.vault.Close()
		_ = w.taskVault.Close()
	})
	if transport.calls.Load() != 0 {
		t.Fatal("opening the workbench called an upstream")
	}
	return w, transport
}

func TestDoubaoManagedRuntimeIsWindowsOnly(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want bool
	}{
		{"windows", true},
		{"linux", false},
		{"darwin", false},
		{"android", false},
	} {
		if got := doubaoManagedRuntimeSupported(tc.goos); got != tc.want {
			t.Fatalf("doubaoManagedRuntimeSupported(%q)=%v want %v", tc.goos, got, tc.want)
		}
	}
}

func TestDoubaoRuntimeHelperDiscoveryUsesPackagedRuntimeRoot(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "doubao-runtime", "bridge.py")
	if err := os.MkdirAll(filepath.Dir(want), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("# synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := findDoubaoRuntimeHelper(root, t.TempDir()); got != want {
		t.Fatalf("helper=%q want %q", got, want)
	}
	if got := findDoubaoRuntimeHelper(t.TempDir()); got != "" {
		t.Fatalf("missing helper discovered: %q", got)
	}
}

// TestDoubaoPackagedHelperDiscoveryDrivesAttachedClientRuntime covers the
// packaged-runtime path that a Linux deployment actually has: the helper is
// auto-discovered from <root>/doubao-runtime/bridge.py, and an explicit
// loopback port attaches to an already-running official client. The gateway
// must not claim to own a client lifecycle it cannot launch, so managed stays
// nil and Passport sms/qr stay unavailable without an official signer.
func TestDoubaoPackagedHelperDiscoveryDrivesAttachedClientRuntime(t *testing.T) {
	bridge := writeSyntheticBridge(t)
	packagedRoot := filepath.Dir(filepath.Dir(bridge))
	w, tr := openDoubaoWiringRuntimeAt(t, filepath.Join(t.TempDir(), "data"), "linux", "", "", "19231", packagedRoot)
	if w.managed != nil {
		t.Fatal("Linux packaged runtime constructed a managed client lifecycle it cannot launch")
	}
	if w.connection.Runtime == nil {
		t.Fatal("packaged helper discovery did not construct the attached-client runtime")
	}
	driver, ok := workbenchAuthDrivers(w)["doubao"].(*doubao.DesktopConnection)
	if !ok {
		t.Fatalf("driver=%T want attached *doubao.DesktopConnection", workbenchAuthDrivers(w)["doubao"])
	}
	methods := driver.Methods()
	if len(methods) != 3 || methods[0].ID != "desktop" || !methods[0].Available || methods[0].Reason != "" {
		t.Fatalf("attached desktop method not available: %+v", methods)
	}
	for _, method := range methods[1:] {
		if method.Available {
			t.Fatalf("passport method %q advertised without an official signer: %+v", method.ID, method)
		}
	}
	if tr.calls.Load() != 0 {
		t.Fatal("runtime construction contacted an upstream")
	}
}

func TestDoubaoLinuxWithoutRuntimeDoesNotAdvertiseManagedLogin(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	w, _ := openDoubaoWiringRuntime(t, stateDir, "linux", executablePath(t), writeSyntheticBridge(t), "")
	if w.managed != nil || w.connection.Runtime != nil {
		t.Fatal("Linux constructed an official-client lifecycle it cannot launch")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "official-client")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported platform created an official client profile directory")
	}
	driver, ok := workbenchAuthDrivers(w)["doubao"].(*doubao.DesktopConnection)
	if !ok {
		t.Fatalf("driver=%T want *doubao.DesktopConnection", workbenchAuthDrivers(w)["doubao"])
	}
	methods := driver.Methods()
	if len(methods) != 3 || methods[0].ID != "desktop" {
		t.Fatalf("methods=%+v", methods)
	}
	for _, method := range methods {
		if method.Available {
			t.Fatalf("method %q advertised on a host with no official client: %+v", method.ID, method)
		}
	}
	if methods[0].Reason != "official_desktop_connection_required" {
		t.Fatalf("desktop reason=%q", methods[0].Reason)
	}
}

func TestDoubaoLinuxExplicitLoopbackPortUsesAttachedClientPath(t *testing.T) {
	w, _ := openDoubaoWiringRuntime(t, filepath.Join(t.TempDir(), "data"), "linux", executablePath(t), writeSyntheticBridge(t), "19231")
	if w.managed != nil {
		t.Fatal("explicit loopback port also started an owned client lifecycle")
	}
	if w.connection.Runtime == nil {
		t.Fatal("explicit loopback port did not create the attached-client runtime")
	}
	driver, ok := workbenchAuthDrivers(w)["doubao"].(*doubao.DesktopConnection)
	if !ok {
		t.Fatalf("driver=%T want attached *doubao.DesktopConnection", workbenchAuthDrivers(w)["doubao"])
	}
	methods := driver.Methods()
	if len(methods) != 3 || methods[0].ID != "desktop" || !methods[0].Available || methods[0].Reason != "" {
		t.Fatalf("attached desktop method not available: %+v", methods)
	}
	// Passport sms/qr remain explicitly unavailable without Bootstrap+Prepare.
	for _, method := range methods[1:] {
		if method.Available {
			t.Fatalf("passport method %q advertised without a signer: %+v", method.ID, method)
		}
	}
}

func TestDoubaoWindowsPackagedRuntimeUsesManagedOfficialLogin(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("managed official login is hosted by the Windows desktop client")
	}
	w, _ := openDoubaoWiringRuntime(t, filepath.Join(t.TempDir(), "data"), "windows", executablePath(t), writeSyntheticBridge(t), "")
	if w.managed == nil || w.connection.Runtime == nil {
		t.Fatal("Windows packaged runtime did not construct the managed official client")
	}
	driver, ok := workbenchAuthDrivers(w)["doubao"].(*doubao.ManagedConnection)
	if !ok {
		t.Fatalf("driver=%T want *doubao.ManagedConnection", workbenchAuthDrivers(w)["doubao"])
	}
	methods := driver.Methods()
	if len(methods) != 3 {
		t.Fatalf("methods=%+v", methods)
	}
	for _, method := range methods {
		if !method.Available {
			t.Fatalf("managed method %q unavailable: %+v", method.ID, method)
		}
	}
}
