// ═══ 更新日志 ═══
// 2026-11-20：暂存更新同时保护仍在运行的完整目录，清理其它旧版，避免逐版堆积。
// 2026-09-26：更新成功后清理更新目录：保留当前与上一个运行目录，删除更早的运行目录与压缩包。
package hotupdate

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pruneKeptRuntimes 保留的运行目录数量：当前 + 上一个（回滚留一手）。
const pruneKeptRuntimes = 2

const managedRuntimeMarker = ".loomgate-managed-runtime"
const managedRuntimeMarkerContent = "LoomGate verified runtime v1\n"

func managedRuntime(dir string) bool {
	path := filepath.Join(dir, managedRuntimeMarker)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(managedRuntimeMarkerContent)) {
		return false
	}
	raw, err := os.ReadFile(path)
	return err == nil && string(raw) == managedRuntimeMarkerContent
}

// pruneUpdateDir 清理更新目录，避免每次升级都留下一份运行目录与压缩包。
//
// 只删自己生成的东西：带固定所有权标记的 `runtime-*` 运行目录（保留两个）、下载的
// `*-wb2api-linux-*` 二进制、`*-wb2api-runtime-linux-*` 包及 `.download-*.part`。
// `current` 指针指向的文件及其所在目录永不删除；其余文件一律不动。
func pruneUpdateDir(dir, current string, alsoKeep ...string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	protected := map[string]bool{}
	kept := map[string]bool{}
	for _, path := range append([]string{current}, alsoKeep...) {
		if strings.TrimSpace(path) != "" {
			path = filepath.Clean(path)
			protected[path] = true
			kept[filepath.Dir(path)] = true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type entry struct {
		path    string
		modTime int64
	}
	runtimes := []entry{}
	for _, item := range entries {
		name := item.Name()
		path := filepath.Join(dir, name)
		if item.IsDir() {
			if strings.HasPrefix(name, "runtime-") && managedRuntime(path) {
				info, err := item.Info()
				if err != nil {
					continue
				}
				runtimes = append(runtimes, entry{path: path, modTime: info.ModTime().UnixNano()})
			}
			continue
		}
		if !item.Type().IsRegular() || protected[path] {
			continue
		}
		if (strings.HasPrefix(name, ".download-") && strings.HasSuffix(name, ".part")) ||
			strings.Contains(name, "-wb2api-linux-") || strings.Contains(name, "-wb2api-runtime-linux-") {
			removeAndLog(path)
		}
	}
	if len(runtimes) <= pruneKeptRuntimes-1 {
		return
	}
	sort.Slice(runtimes, func(i, j int) bool { return runtimes[i].modTime > runtimes[j].modTime })
	keepCount := pruneKeptRuntimes
	for _, item := range runtimes {
		if kept[item.path] {
			keepCount--
		}
	}
	for _, item := range runtimes {
		if kept[item.path] {
			continue
		}
		if keepCount > 0 {
			kept[item.path] = true
			keepCount--
			continue
		}
		removeAllAndLog(item.path)
	}
}

func removeAndLog(path string) {
	if err := os.Remove(path); err == nil {
		log.Printf("[热更新] 已删除 %s", path)
	}
}

func removeAllAndLog(path string) {
	if err := os.RemoveAll(path); err == nil {
		log.Printf("[热更新] 已删除 %s", path)
	}
}
