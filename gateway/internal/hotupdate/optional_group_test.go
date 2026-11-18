// ═══ 更新日志 ═══
// 2026-11-18：锁定运行包可选组件组的接受规则。
//
// 豆包桥接组件（doubao-runtime/*）是后来加入的整组：v3.16.1 及更早版本产出的
// 18 文件包不含它们，升级路径必须继续可用；而只带一半的包会让工作台在运行期
// 才发现缺 helper，必须拒绝。这两条都不能只写在注释里。
package hotupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// legacyStyleBundle builds an archive whose manifest and members cover only the
// legacy members (no optional group). It mirrors testBundle's construction so
// the accepted path differs from the full package in exactly one variable.
func legacyStyleBundle(t *testing.T) string {
	t.Helper()
	files := map[string][]byte{}
	manifest := runtimeManifest{Format: 1, Version: "v2.2.0", OS: "linux", Arch: runtime.GOARCH, Files: map[string]string{}}
	for name := range runtimeFiles {
		if doubaoRuntimeFiles[name] {
			continue
		}
		data := []byte("fixture:" + name)
		files[name] = data
		sum := sha256.Sum256(data)
		manifest.Files[name] = hex.EncodeToString(sum[:])
	}
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	encoded, _ := json.Marshal(manifest)
	files["manifest.json"] = encoded
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runtime.tar.gz")
	if err := os.WriteFile(path, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLegacyRuntimeWithoutOptionalGroupIsAccepted(t *testing.T) {
	dir := t.TempDir()
	binary, err := extractRuntimeBundle(legacyStyleBundle(t), Release{Tag: "v2.2.0"}, dir)
	if err != nil {
		t.Fatalf("pre-group runtime was rejected: %v", err)
	}
	if filepath.Base(binary) != "wb2api" {
		t.Fatalf("unexpected executable: %s", binary)
	}
	for name := range runtimeFiles {
		path := filepath.Join(filepath.Dir(binary), filepath.FromSlash(name))
		_, statErr := os.Stat(path)
		if doubaoRuntimeFiles[name] {
			if statErr == nil {
				t.Fatalf("legacy bundle synthesized optional member %s", name)
			}
			continue
		}
		if statErr != nil {
			t.Fatalf("legacy bundle dropped required member %s: %v", name, statErr)
		}
	}
}

func TestPartialRuntimeOptionalGroupIsRejected(t *testing.T) {
	archive := testBundle(t, func(m *runtimeManifest, files map[string][]byte) {
		// Drop one member of the optional group from both the manifest and the
		// archive: a half package must never be accepted.
		const dropped = "doubao-runtime/managed.py"
		delete(files, dropped)
		delete(m.Files, dropped)
	}, nil)
	dir := t.TempDir()
	if _, err := extractRuntimeBundle(archive, Release{Tag: "v2.2.0"}, dir); err == nil {
		t.Fatal("partial optional component group was accepted")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("rejected partial package left runtime residue")
	}
}
