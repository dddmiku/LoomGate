// ═══ 更新日志 ═══
// 2026-11-17：控制台迁移页只读状态。只报告当前工作区的迁移标记与离线工具是否
// 随包提供，不浏览任意目录、不读取源数据、不输出凭据。
package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

const migrationMarkerName = ".loomgate-migration.json"

var migrationDigestRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// migrationMarker 是迁移工具写入目标根目录的固定标记；只暴露可公开核对的
// 字段，源路径与账号数据都不在标记里。
type migrationMarkerView struct {
	Format             int    `json:"format"`
	FileCount          int    `json:"file_count"`
	InventorySHA256    string `json:"inventory_sha256"`
	SourceConfigSHA256 string `json:"source_config_sha256"`
	DestinationBinding string `json:"destination_binding"`
}

func (s *Server) migrationStatus(w http.ResponseWriter, r *http.Request) {
	root := s.opts.BaseDir
	if root == "" {
		root = "."
	}
	payload := map[string]any{"ok": true}
	if marker, err := readMigrationMarker(filepath.Join(root, migrationMarkerName)); err == nil && marker != nil {
		payload["marker"] = marker
	}
	toolPath := filepath.Join(root, "tools", "migration", "migrate.py")
	if info, err := os.Stat(toolPath); err == nil && info.Mode().IsRegular() {
		payload["tool"] = map[string]any{"available": true,
			"message": "离线迁移工具随此工作区提供；导入前请完成预检与回滚演练。"}
	} else {
		payload["tool"] = map[string]any{"available": false,
			"message": "此运行包未包含离线迁移工具；请从同版本源码目录运行迁移，或使用完整运行包。"}
	}
	writeJSON(w, http.StatusOK, payload)
}

func readMigrationMarker(path string) (*migrationMarkerView, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, os.ErrInvalid
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document struct {
		Format             int    `json:"format"`
		FileCount          int    `json:"file_count"`
		InventorySHA256    string `json:"inventory_sha256"`
		SourceConfigSHA256 string `json:"source_config_sha256"`
		DestinationBinding string `json:"destination_binding"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	// Only a structurally valid marker is shown; a corrupt one must not look
	// like a successful migration.
	if document.Format != 2 || document.FileCount < 1 ||
		!migrationDigestRe.MatchString(document.InventorySHA256) ||
		!migrationDigestRe.MatchString(document.SourceConfigSHA256) ||
		document.DestinationBinding == "" {
		return nil, os.ErrInvalid
	}
	return &migrationMarkerView{Format: document.Format, FileCount: document.FileCount,
		InventorySHA256: document.InventorySHA256, SourceConfigSHA256: document.SourceConfigSHA256,
		DestinationBinding: document.DestinationBinding}, nil
}
