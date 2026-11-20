// ═══ 更新日志 ═══
// 2026-09-25：统一运行包完整验证后才允许启动，拒绝越界、链接、重复文件、错误架构和缺少辅助程序。
package hotupdate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var runtimeFiles = map[string]os.FileMode{
	"wb2api": 0755, "login": 0755, "credit": 0755, "signin_bin": 0755, "trial_bin": 0755, "activity_bin": 0755,
	"fission_bin": 0755,
	"login.sh":    0755, "credit.sh": 0755, "signin.sh": 0755, "trial.sh": 0755, "fission.sh": 0755,
	"scripts/global_region.py": 0644, "scripts/task_common.py": 0644, "scripts/task_runner.py": 0644,
	"scripts/school_open_day_2026.py": 0644, "scripts/growth_center.py": 0644,
	// 补偿积分领取（一次性任务）的执行体。必须与 scripts/runtime_bundle.py 的
	// FILES 同步——TestRuntimeFileListsMatch 会拦住漏改的那一侧。
	"scripts/compensation_claim.py": 0644,
	// 豆包官方桌面目录桥接。Linux 运行包带上它，才能在已运行官方客户端的机器上
	// 通过回环端口连接现有会话（不启动、不托管客户端）。它们是一整组：
	// 要么全部带上，要么整组省略——半包会让工作台在运行期才发现缺 helper。
	"doubao-runtime/bridge.py":         0644,
	"doubao-runtime/managed.py":        0644,
	"doubao-runtime/stream_bridge.py":  0644,
	"doubao-runtime/runtime.js":        0644,
	"doubao-runtime/tasks_runtime.js":  0644,
	"doubao-runtime/work_runtime.js":   0644,
	"doubao-runtime/stream_runtime.js": 0644,
	"doubao-runtime/probe_runtime.js":  0644,
	"doubao-runtime/probe_cleanup.js":  0644,
	"doubao-runtime/requirements.txt":  0644,
}

// doubaoRuntimeFiles is the optional group above. Bundles released before the
// group existed carry only legacyRuntimeFiles; accepting them keeps the
// upgrade path of the v3.16.1 baseline intact.
var doubaoRuntimeFiles = map[string]bool{
	"doubao-runtime/bridge.py": true, "doubao-runtime/managed.py": true,
	"doubao-runtime/stream_bridge.py": true, "doubao-runtime/runtime.js": true,
	"doubao-runtime/tasks_runtime.js": true, "doubao-runtime/work_runtime.js": true,
	"doubao-runtime/stream_runtime.js": true, "doubao-runtime/probe_runtime.js": true,
	"doubao-runtime/probe_cleanup.js": true, "doubao-runtime/requirements.txt": true,
}

func legacyRuntimeFileCount() int { return len(runtimeFiles) - len(doubaoRuntimeFiles) }

type runtimeManifest struct {
	Format  int               `json:"format"`
	Version string            `json:"version"`
	OS      string            `json:"os"`
	Arch    string            `json:"arch"`
	Files   map[string]string `json:"files"`
}

func extractRuntimeBundle(archive string, release Release, dir string) (binary string, resultErr error) {
	// #nosec G304 -- archive is the SHA-256-verified file returned by Download in the configured update directory.
	file, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer file.Close()
	compressed := bufio.NewReader(file)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	gz.Multistream(false)
	stage, err := os.MkdirTemp(dir, "runtime-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if resultErr != nil {
			_ = os.RemoveAll(stage)
		}
	}()
	root, err := os.OpenRoot(stage)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := root.Mkdir("scripts", 0700); err != nil {
		return "", err
	}
	// Runtime members may live in nested directories (scripts/, doubao-runtime/).
	// os.Root.OpenFile does not create parents, so the package layout decides
	// which directories exist. Create them from the allowlist instead of
	// hard-coding one path: a member whose parent is missing would otherwise
	// fail with a bare "cannot find the path" after the archive was accepted.
	for name := range runtimeFiles {
		dir := filepath.Dir(filepath.FromSlash(name))
		if dir == "." {
			continue
		}
		if err := root.MkdirAll(dir, 0700); err != nil {
			return "", err
		}
	}
	decoded := &io.LimitedReader{R: gz, N: 256<<20 + 1}
	reader := tar.NewReader(decoded)
	hashes := map[string]string{}
	var manifest runtimeManifest
	var manifestSeen bool
	var total int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		name := header.Name
		mode, known := runtimeFiles[name]
		if header.Typeflag != tar.TypeReg || (!known && name != "manifest.json") || hashes[name] != "" || header.Size < 0 || header.Size > maxReleaseBytes {
			return "", fmt.Errorf("invalid runtime member %q", name)
		}
		total += header.Size
		if total > 256<<20 {
			return "", fmt.Errorf("runtime bundle exceeds the unpacked size limit")
		}
		if name == "manifest.json" {
			if manifestSeen || header.Size > 65536 {
				return "", fmt.Errorf("invalid runtime manifest")
			}
			manifestSeen = true
			raw, err := io.ReadAll(reader)
			if err != nil {
				return "", err
			}
			if err := json.Unmarshal(raw, &manifest); err != nil {
				return "", err
			}
			if err := root.WriteFile("manifest.json", raw, 0600); err != nil {
				return "", err
			}
			continue
		}
		out, err := root.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return "", err
		}
		hash := sha256.New()
		_, copyErr := io.CopyN(io.MultiWriter(out, hash), reader, header.Size)
		syncErr := out.Sync()
		closeErr := out.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if syncErr != nil {
			return "", syncErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		hashes[name] = hex.EncodeToString(hash.Sum(nil))
	}
	// Consume the gzip trailer as well, so a corrupt archive cannot pass just
	// because tar ended before its checksum was checked.
	padding, err := io.ReadAll(io.LimitReader(decoded, 16<<10+1))
	if err != nil {
		return "", err
	}
	if len(padding) > 16<<10 || len(bytes.Trim(padding, "\x00")) != 0 {
		return "", fmt.Errorf("invalid data after runtime archive")
	}
	if decoded.N == 0 {
		return "", fmt.Errorf("runtime archive exceeds the total decompression limit")
	}
	if _, err := compressed.Peek(1); err != io.EOF {
		return "", fmt.Errorf("unexpected data after gzip member")
	}
	if !manifestSeen || manifest.Format != 1 || manifest.OS != "linux" || manifest.Arch != runtime.GOARCH || strings.TrimPrefix(manifest.Version, "v") != strings.TrimPrefix(release.Tag, "v") {
		return "", fmt.Errorf("runtime manifest does not match release/platform")
	}
	// The Doubao helper group is optional as a whole: bundles released before
	// it existed (the v3.16.1 upgrade path) carry only the legacy members.
	// Half a group is never acceptable — that would start a workbench whose
	// login path is missing a helper only at runtime.
	present := 0
	for name := range doubaoRuntimeFiles {
		if manifest.Files[name] != "" || hashes[name] != "" {
			present++
		}
	}
	expected := len(runtimeFiles)
	if present == 0 {
		expected = legacyRuntimeFileCount()
	} else if present != len(doubaoRuntimeFiles) {
		return "", fmt.Errorf("runtime bundle has a partial optional component group")
	}
	if len(manifest.Files) != expected || len(hashes) != expected {
		return "", fmt.Errorf("runtime bundle is missing required files")
	}
	for name := range runtimeFiles {
		if manifest.Files[name] == "" && present == 0 && doubaoRuntimeFiles[name] {
			continue
		}
		if len(manifest.Files[name]) != 64 || !strings.EqualFold(manifest.Files[name], hashes[name]) {
			return "", fmt.Errorf("runtime digest mismatch for %s", name)
		}
	}
	// Only a fully checked extraction belongs to the updater. Cleanup never
	// infers ownership from a directory name or follows a marker symlink.
	if err := root.WriteFile(managedRuntimeMarker, []byte(managedRuntimeMarkerContent), 0600); err != nil {
		return "", fmt.Errorf("mark verified runtime: %w", err)
	}
	return filepath.Join(stage, "wb2api"), nil
}
