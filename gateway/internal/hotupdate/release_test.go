// ═══ 更新日志 ═══
// 2026-09-25：CI资产契约改为双架构完整运行包及校验文件，保留旧裸二进制选取的独立兼容测试。
// 2026-09-17：锁定自更新取件契约：按架构挑资产、下载逐字节校验 SHA-256、
//
//	摘要不符时不留残留文件、超大资产直接拒绝。
package hotupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	versionpkg "workbuddy2api/internal/version"
)

func TestLatestPicksAssetForThisArchitecture(t *testing.T) {
	want, err := assetName()
	if err != nil {
		t.Skipf("unsupported arch %s", runtime.GOARCH)
	}
	payload := map[string]any{
		"tag_name":     "v9.9.9",
		"name":         "v9.9.9",
		"published_at": "2026-09-17T10:00:00Z",
		"body":         "notes",
		// 资产清单覆盖三端：测试跑在哪个平台上，就得有对应的那份资产。
		// 只列 linux 会让 Windows/安卓上跑测试时找不到本机资产而失败。
		"assets": []map[string]any{
			{"name": "LoomGate-linux-amd64", "size": 11, "browser_download_url": "https://example.invalid/amd64",
				"digest": "sha256:aaaa"},
			{"name": "LoomGate-linux-arm64", "size": 22, "browser_download_url": "https://example.invalid/arm64",
				"digest": "sha256:bbbb"},
			{"name": "LoomGate-windows-amd64", "size": 44, "browser_download_url": "https://example.invalid/win-amd64",
				"digest": "sha256:cccc"},
			{"name": "LoomGate-android-arm64.apk", "size": 55, "browser_download_url": "https://example.invalid/android-arm64.apk",
				"digest": "sha256:dddd"},
			// Transition release also carries the historical aliases. The new
			// names above must win, but an alias-only release must still work.
			{"name": "wb2api-linux-amd64", "size": 1, "browser_download_url": "https://example.invalid/amd64-legacy",
				"digest": "sha256:eeee"},
			{"name": "wb2api-linux-arm64", "size": 2, "browser_download_url": "https://example.invalid/arm64-legacy",
				"digest": "sha256:ffff"},
			{"name": "wb2api-windows-amd64", "size": 3, "browser_download_url": "https://example.invalid/win-legacy",
				"digest": "sha256:1111"},
			{"name": "workbuddy2api-android-arm64.apk", "size": 4, "browser_download_url": "https://example.invalid/android-legacy",
				"digest": "sha256:2222"},
			{"name": "SHA256SUMS.txt", "size": 33, "browser_download_url": "https://example.invalid/sums"},
		},
	}
	body, _ := json.Marshal(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+DefaultRepo+"/releases/latest" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); !strings.Contains(got, "github") {
			t.Errorf("accept header = %q", got)
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client := NewClient("", "")
	client.APIBase = server.URL
	release, err := client.Latest(context.Background())
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if release.Tag != "v9.9.9" || release.AssetName != want {
		t.Fatalf("release = %+v want tag v9.9.9 asset %s", release, want)
	}
	if release.Digest == "" || release.AssetSize == 0 {
		t.Fatalf("digest/size must be carried over: %+v", release)
	}
	if release.PublishedAt.IsZero() || release.Notes != "notes" {
		t.Fatalf("metadata lost: %+v", release)
	}
}

func TestLatestRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer server.Close()
	client := NewClient("", "")
	client.APIBase = server.URL
	if _, err := client.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v want an HTTP 403 failure", err)
	}
}

func TestDownloadVerifiesDigest(t *testing.T) {
	content := []byte("#!/bin/sh\necho wb2api\n")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()
	dir := t.TempDir()
	client := NewClient("", "")
	release := Release{Tag: "v9.9.9", AssetName: "wb2api-linux-amd64", AssetURL: server.URL, AssetSize: int64(len(content))}

	// 摘要不符：必须失败并清掉临时文件，绝不留下一个未校验的二进制。
	bad := release
	bad.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, _, err := client.Download(context.Background(), bad, dir); err == nil {
		t.Fatal("digest mismatch must fail the download")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed download left files behind: %v", entries)
	}

	// 摘要正确：落盘且带上可执行位。
	good := release
	good.Digest = "sha256:" + digest
	path, got, err := client.Download(context.Background(), good, dir)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if got != digest {
		t.Fatalf("sum = %s want %s", got, digest)
	}
	// filepath.Join normalizes separators, so compare against the cleaned
	// form: t.TempDir() inherits whatever separator TMP uses, and on Windows
	// that can differ from the joined path even though it is the same dir.
	if filepath.Dir(path) != filepath.Clean(dir) || !strings.Contains(filepath.Base(path), "v9.9.9") {
		t.Fatalf("unexpected target path %s", path)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved binary: %v", err)
	}
	if string(saved) != string(content) {
		t.Fatalf("saved content mismatch")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows 没有可执行位，只在 Unix 上核对。
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("downloaded binary must be executable, mode = %v", info.Mode())
	}
}

func TestDownloadRejectsOversizeAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("oversize asset must be rejected before any request")
	}))
	defer server.Close()
	client := NewClient("", "")
	release := Release{Tag: "v9.9.9", AssetName: "wb2api-linux-amd64", AssetURL: server.URL,
		AssetSize: maxReleaseBytes + 1}
	if _, _, err := client.Download(context.Background(), release, t.TempDir()); err == nil {
		t.Fatal("oversize asset must fail")
	}
}

func TestDownloadRequiresAssetURL(t *testing.T) {
	client := NewClient("", "")
	if _, _, err := client.Download(context.Background(), Release{Tag: "v9.9.9"}, t.TempDir()); err == nil {
		t.Fatal("release without an asset must fail")
	}
}

func TestUpdateAvailableComparesVersions(t *testing.T) {
	original := versionpkg.Version
	defer func() { versionpkg.Version = original }()

	versionpkg.Version = "1.2.0"
	cases := []struct {
		tag  string
		want bool
	}{
		{"v1.2.0", false},
		{"1.2.0", false},
		{"v1.3.0", true},
		{"", false},
	}
	for _, item := range cases {
		if got := (Release{Tag: item.tag}).UpdateAvailable(); got != item.want {
			t.Fatalf("UpdateAvailable(%q) = %v want %v", item.tag, got, item.want)
		}
	}
	// 开发构建：远端只要有 tag 就提示可更新。
	versionpkg.Version = "dev"
	if !(Release{Tag: "v1.2.0"}).UpdateAvailable() {
		t.Fatal("dev builds must treat any release as an update")
	}
}

// v3.17.0 是旧线过渡版本，内部已是 LoomGate。按数字比较会把 LoomGate 1.x
// 判成降级，过渡版就永远升不上去；但同一产品线内的防降级必须保留。
func TestTransitionalLegacyAcceptsLoomGateLine(t *testing.T) {
	original := versionpkg.Version
	defer func() { versionpkg.Version = original }()
	for _, test := range []struct {
		current string
		tag     string
		want    bool
	}{
		{"v3.17.0", "v1.0.4", true},   // 过渡到 LoomGate 1.x
		{"v3.17.0", "v1.9.0", true},   // 未来 1.x 也算
		{"v3.17.0", "v3.17.0", false}, // 自身不算
		{"v3.17.0", "v3.16.1", false}, // 旧线防降级
		{"v3.16.1", "v1.0.4", false},  // 未升级的旧客户端仍按原规则
		{"v1.0.4", "v1.0.3", false},   // LoomGate 自身防降级
		{"v1.0.4", "v1.0.5", true},
	} {
		versionpkg.Version = test.current
		if got := (Release{Tag: test.tag}).UpdateAvailable(); got != test.want {
			t.Errorf("current=%s tag=%s: got %v want %v", test.current, test.tag, got, test.want)
		}
	}
}

// TestReleaseAssetNamesMatchManifest 对照构建清单与实际取件函数，独立于仓库布局和 CI 文本。
func TestReleaseAssetNamesMatchManifest(t *testing.T) {
	var manifest struct {
		SchemaVersion  int    `json:"schema_version"`
		PublicKey      string `json:"public_key"`
		ChecksumsAsset string `json:"checksums_asset"`
		SignatureAsset string `json:"signature_asset"`
		Targets        []struct {
			ID                string `json:"id"`
			GOOS              string `json:"goos"`
			GOARCH            string `json:"goarch"`
			GOARM             string `json:"goarm"`
			UpdateAsset       string `json:"update_asset"`
			RuntimeAsset      string `json:"runtime_asset"`
			CIBuild           bool   `json:"ci_build"`
			ManualRequirement string `json:"manual_requirement"`
		} `json:"targets"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "release-targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 || manifest.PublicKey != "release-signing.pub" ||
		manifest.ChecksumsAsset != sumsAssetName || manifest.SignatureAsset != sumsSigAssetName {
		t.Fatal("manifest must preserve the existing release verification contract")
	}
	keyText, err := os.ReadFile(filepath.Join("..", "..", manifest.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(keyText)))
	if err != nil || len(key) != 32 {
		t.Fatal("release verification key must be a 32-byte Ed25519 public key")
	}
	seen := map[string]bool{}
	for _, target := range manifest.Targets {
		t.Run(target.ID, func(t *testing.T) {
			if target.ID == "" || seen[target.ID] {
				t.Fatal("duplicate or empty target id")
			}
			seen[target.ID] = true
			t.Setenv("GOARM", target.GOARM)
			actual, err := assetNameFor(target.GOOS, target.GOARCH)
			if err != nil {
				t.Fatal(err)
			}
			if target.UpdateAsset != actual {
				t.Fatalf("manifest asset %q != actual updater asset %q", target.UpdateAsset, actual)
			}
			if target.GOOS == "linux" {
				// The public naming transition keeps a LoomGate-* runtime alias in
				// addition to the historical wb2api-* runtime asset. The manifest
				// still names the primary asset produced by the build.
				want := strings.Replace(actual, "LoomGate-linux-", "LoomGate-runtime-linux-", 1) + ".tar.gz"
				if target.RuntimeAsset != want {
					t.Fatalf("runtime bundle = %q, want %q", target.RuntimeAsset, want)
				}
				if target.GOARCH == runtime.GOARCH && target.RuntimeAsset != runtimeAssetName() {
					t.Fatalf("runtime bundle does not match actual runtimeAssetName(): %q", runtimeAssetName())
				}
				if !target.CIBuild {
					t.Fatal("Linux compatibility bundles must be built by CI")
				}
			} else if target.RuntimeAsset != "" {
				t.Fatal("non-Linux targets cannot use runtime bundle updates")
			}
			if target.GOOS == "android" && (target.CIBuild || target.ManualRequirement == "") {
				t.Fatal("Android must retain its existing APK signing identity outside unsigned CI")
			}
		})
	}
	for _, required := range []string{"linux-amd64", "linux-arm64", "linux-armv7", "windows-amd64", "android-arm64"} {
		if !seen[required] {
			t.Errorf("missing compatibility target %s", required)
		}
	}
}

func TestNewClientDefaultsRepoAndTimeout(t *testing.T) {
	client := NewClient("  ", "token")
	if client.Repo != DefaultRepo {
		t.Fatalf("repo = %q want default", client.Repo)
	}
	if client.Token != "token" || client.HTTP == nil || client.HTTP.Timeout != apiTimeout {
		t.Fatalf("client = %+v", client)
	}
	if client.apiBase() != defaultAPIBase {
		t.Fatalf("api base = %q want %q", client.apiBase(), defaultAPIBase)
	}
	client.APIBase = "http://127.0.0.1:8080/"
	if client.apiBase() != "http://127.0.0.1:8080" {
		t.Fatalf("api base override = %q", client.apiBase())
	}
}

// TestAssetNameFollowsRuntimePlatform 锁住「三端各拿各的资产」。
//
// 安卓的 GOOS 是 "android" 而不是 "linux"（内核是 Linux，但 Go 把它单独标出来）。
// 不按 GOOS 分的话，手机与桌面都会去下 Linux 的二进制——那个在手机上根本跑不起来。
func TestAssetNameFollowsRuntimePlatform(t *testing.T) {
	name, err := assetName()
	if err != nil {
		// 不支持的架构必须报错，且错误里说清是架构问题。
		if !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("assetName() error = %v; 不支持时错误里应含 unsupported", err)
		}
		return
	}
	want, wantErr := assetNameFor(runtime.GOOS, runtime.GOARCH)
	if wantErr != nil {
		t.Fatalf("assetNameFor(%s,%s) error = %v", runtime.GOOS, runtime.GOARCH, wantErr)
	}
	if name != want {
		t.Fatalf("assetName() = %q，期望 %q（GOOS=%s GOARCH=%s）", name, want, runtime.GOOS, runtime.GOARCH)
	}
}

// TestAssetNamePerPlatform 三端的资产名必须在**任意主机**上都能被断言。
//
// 真事故（2026-10-13）：这条断言原本写成按 runtime.GOOS 分支，安卓那一支期望
// "wb2api-android-<arch>"，而实现早已改成 APK 资产 "workbuddy2api-android-<arch>.apk"。
// 因为该分支只在 GOOS==android 时执行、而本项目从不在安卓上跑 `go test`，这个
// 错误永远暴露不出来——等于安卓这条最容易出问题的路径**完全没有测试**。
//
// 现在断言的是 assetNameFor 这个纯函数，三端一起覆盖，与宿主平台无关。
// 这些名字必须与发布产物一致：linux 见 tools/private_gateway_release_20260925.py，
// windows 见 desktop/build.py，android 见 android/build.py 的 APK 输出名。
func TestAssetNamePerPlatform(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         string
	}{
		{"linux", "amd64", "LoomGate-linux-amd64"},
		{"linux", "arm64", "LoomGate-linux-arm64"},
		{"windows", "amd64", "LoomGate-windows-amd64"},
		{"android", "arm64", "LoomGate-android-arm64.apk"},
	}
	for _, c := range cases {
		got, err := assetNameFor(c.goos, c.goarch)
		if err != nil {
			t.Errorf("assetNameFor(%s,%s) error = %v", c.goos, c.goarch, err)
			continue
		}
		if got != c.want {
			t.Errorf("assetNameFor(%s,%s) = %q，期望 %q", c.goos, c.goarch, got, c.want)
		}
	}
	// 安卓的资产必须以 .apk 结尾：裸二进制在手机上装不起来（SELinux 禁止在
	// 应用私有目录执行），发错了会让更新下载到一个用不了的文件。
	if _, err := assetNameFor("android", "386"); err != nil {
		t.Errorf("assetNameFor(android,386) 不应报错（APK 资产与架构无关）: %v", err)
	}
}

// TestBundleEnabledFollowsPlatform 宿主传给 Manager 的 Bundle 必须与平台一致。
//
// 真机事故（2026-10-13）：安卓上 layout.enabled 为真，Bundle 就被打开了，于是
// 网关把下载到的**裸二进制**当 tar.gz 解包，直接报 `gzip: invalid header`。
// 这个判据由宿主（cmd/server）给出，测试在这里锁住它的取值来源。
func TestBundleEnabledFollowsPlatform(t *testing.T) {
	if runtime.GOOS == "linux" {
		if !BundleSupported() {
			t.Fatal("Linux 必须支持完整运行包")
		}
		return
	}
	if BundleSupported() {
		t.Fatalf("%s 上不得启用完整运行包（会把裸二进制当 tar.gz 解包）", runtime.GOOS)
	}
}

// TestBundleOnlyOnLinux 完整运行包只有 Linux 部署有。
//
// 桌面与安卓用的是单文件资产：桌面版的数据与程序在同一个文件夹里、由启动器管；
// 安卓的程序在 APK 的 nativeLibraryDir 里，「解压到目录再整体替换」的前提不成立。
// 若在这两个平台上仍去要运行包，会去找一个不存在的资产名。
func TestBundleOnlyOnLinux(t *testing.T) {
	if runtime.GOOS == "linux" && !bundleSupported() {
		t.Fatal("Linux 必须支持完整运行包")
	}
	if runtime.GOOS != "linux" && bundleSupported() {
		t.Fatalf("%s 不该走完整运行包路径", runtime.GOOS)
	}
}

// The rename must not strand v1.0.16. Each release carries both names: the new
// LoomGate-* asset is preferred, but a release with only the historical
// wb2api-* asset still resolves. Both names are byte-identical, and the signed
// checksum file covers both, so this is a naming compatibility layer rather
// than a second trust path.
func TestLatestFallsBackToHistoricalAssetName(t *testing.T) {
	current, err := assetName()
	if err != nil {
		t.Skipf("unsupported arch %s", runtime.GOARCH)
	}
	legacy, ok := legacyAssetName(current)
	if !ok || legacy == "" {
		t.Fatalf("legacy mapping = %q,%v", legacy, ok)
	}
	payload := map[string]any{
		"tag_name": "v9.9.9",
		"assets": []map[string]any{
			{"name": legacy, "size": 7, "browser_download_url": "https://example.invalid/legacy",
				"digest": "sha256:aaaa"},
			{"name": "SHA256SUMS.txt", "size": 33, "browser_download_url": "https://example.invalid/sums"},
		},
	}
	body, _ := json.Marshal(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer server.Close()
	client := NewClient("", "")
	client.APIBase = server.URL
	release, err := client.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if release.AssetName != legacy || release.AssetSize != 7 {
		t.Fatalf("legacy asset not selected: %+v", release)
	}
}

// When both names exist, the new public name must win even if the legacy
// marker appears later in the GitHub asset array.
func TestLatestPrefersNewAssetNameOverAlias(t *testing.T) {
	current, err := assetName()
	if err != nil {
		t.Skipf("unsupported arch %s", runtime.GOARCH)
	}
	legacy, ok := legacyAssetName(current)
	if !ok {
		t.Skip("no legacy alias for this platform")
	}
	payload := map[string]any{
		"tag_name": "v9.9.9",
		"assets": []map[string]any{
			{"name": legacy, "size": 1, "browser_download_url": "https://example.invalid/old",
				"digest": "sha256:1111"},
			{"name": current, "size": 2, "browser_download_url": "https://example.invalid/new",
				"digest": "sha256:2222"},
		},
	}
	body, _ := json.Marshal(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer server.Close()
	client := NewClient("", "")
	client.APIBase = server.URL
	release, err := client.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if release.AssetName != current || release.AssetSize != 2 {
		t.Fatalf("new asset did not win: %+v", release)
	}
}
