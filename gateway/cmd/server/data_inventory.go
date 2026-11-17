// ═══ 更新日志 ═══
// 2026-11-17：新增只读数据清单，用于安卓同签名覆盖升级前后核对配置/账号/密钥/
// 计划/unknown 状态是否保留。只输出每类文件数量与聚合摘要，不输出文件名或内容。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
)

// inventoryCategory 是单类持久化数据的可比较摘要。Digest 由该类每个文件内容
// 的 SHA-256 排序后再哈希得到：目录内文件改名、增删或任一字节变化都会改变它，
// 但不会把文件名或内容带进日志。
type inventoryCategory struct {
	Files  int    `json:"files"`
	Digest string `json:"digest"`
}

// dataInventoryPaths 描述 LoomGate 认可的持久化面；未知目录一律不读，避免
// 把更新下载件、日志或临时文件混进升级对比。
func dataInventoryPaths(root string) map[string][]string {
	return map[string][]string{
		"config":   {filepath.Join(root, "config", "config.json")},
		"accounts": globJSON(filepath.Join(root, "auths")),
		"panel":    {filepath.Join(root, "panel-data", "credentials.json")},
		"keys": {
			filepath.Join(root, "data", "api_keys.json"),
			filepath.Join(root, "data", "usage.json"),
		},
		"plans": {filepath.Join(root, "data", "task-engine.json")},
		"state": {filepath.Join(root, "data", "state.json")},
	}
}

func globJSON(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		// 不在这里过滤符号链接：让 inventory 的 Lstat 拒掉它，避免清单静默
		// 漏掉一个指向数据根之外的账号文件。
		out = append(out, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(out)
	return out
}

func inventory(root string) (map[string]inventoryCategory, error) {
	result := map[string]inventoryCategory{}
	for name, paths := range dataInventoryPaths(root) {
		hashes := make([]string, 0, len(paths))
		for _, path := range paths {
			info, err := os.Lstat(path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			if !info.Mode().IsRegular() || info.Size() > 64<<20 {
				return nil, fmt.Errorf("unsupported persistent file in %s", name)
			}
			file, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			digest := sha256.New()
			_, copyErr := io.Copy(digest, file)
			closeErr := file.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			hashes = append(hashes, hex.EncodeToString(digest.Sum(nil)))
		}
		sort.Strings(hashes)
		aggregate := sha256.New()
		for _, hash := range hashes {
			_, _ = io.WriteString(aggregate, hash+"\n")
		}
		result[name] = inventoryCategory{Files: len(hashes), Digest: hex.EncodeToString(aggregate.Sum(nil))}
	}
	return result, nil
}

// logDataInventory 在安卓启动时打印一次清单。只读取上面列出的持久化文件，
// 不写盘、不改动任何状态；摘要用于升级前后对比，不用于恢复。
func logDataInventory(root string) {
	categories, err := inventory(root)
	if err != nil {
		log.Printf("WARN: [数据清单] 读取失败：%v", err)
		return
	}
	raw, err := json.Marshal(map[string]any{"ok": true, "root": categories})
	if err != nil {
		log.Printf("WARN: [数据清单] 编码失败：%v", err)
		return
	}
	log.Printf("[数据清单] %s", raw)
}
