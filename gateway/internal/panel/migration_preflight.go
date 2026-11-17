// ═══ 更新日志 ═══
// 2026-11-17：控制台离线迁移预检。只运行迁移工具的 dry-run，不写目标、不读
// 账号内容、不返回文件名称或凭据；真正的 apply 仍要求在命令行离线确认。
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	migrationPreflightTimeout = 120 * time.Second
	migrationOutputLimit      = 1 << 20
)

type migrationPreflightRequest struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

func (s *Server) migrationPreflight(w http.ResponseWriter, r *http.Request, body map[string]any) {
	source, _ := body["source"].(string)
	destination, _ := body["destination"].(string)
	request := migrationPreflightRequest{Source: source, Destination: destination}
	if message := s.validateMigrationPaths(request.Source, request.Destination); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": message})
		return
	}
	tool := filepath.Join(s.opts.BaseDir, "tools", "migration", "migrate.py")
	if info, err := os.Stat(tool); err != nil || !info.Mode().IsRegular() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false,
			"message": "此运行包未包含离线迁移工具；请在源码工作区运行 tools/migration/migrate.py。"})
		return
	}
	python := migrationPython()
	if python == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false,
			"message": "未找到可用的 Python 3 解释器，无法运行离线预检。"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), migrationPreflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-B", "-X", "utf8", tool,
		"copy", "--source", request.Source, "--destination", request.Destination)
	cmd.Dir = s.opts.BaseDir
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedBuffer{buffer: &stdout, limit: migrationOutputLimit}
	cmd.Stderr = &limitedBuffer{buffer: &stderr, limit: 8192}
	runErr := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{"ok": false, "message": "预检超时，未写入任何目标文件。"})
		return
	}
	if runErr != nil {
		// The migration tool emits constant, non-sensitive error codes only.
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(stderr.Bytes(), &failure)
		message := "预检未通过，未写入任何目标文件。"
		if failure.Error != "" {
			message += " 代码：" + failure.Error
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "message": message})
		return
	}
	var report map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": "预检工具返回了无法解析的结果。"})
		return
	}
	// The UI may copy the exact command for the offline apply step; the
	// destination and source are user-provided strings, echoed only as JSON.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "mode": "dry-run", "report": report,
		"apply_requires_offline_confirmation": true,
		"message":                             "预检完成，未写入任何文件。实际复制必须在网关停止写入时离线执行并显式确认。",
	})
}

func (s *Server) validateMigrationPaths(source, destination string) string {
	for _, value := range []string{source, destination} {
		if len(value) == 0 || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return "请填写完整的工作区路径。"
		}
		if !filepath.IsAbs(value) {
			return "源与目标都必须使用绝对路径。"
		}
	}
	source = filepath.Clean(source)
	destination = filepath.Clean(destination)
	base := filepath.Clean(s.opts.BaseDir)
	if samePath(source, base) || insidePath(source, base) {
		return "不能对当前运行中的工作区执行迁移预检。"
	}
	if insidePath(destination, base) || samePath(destination, base) {
		return "目标目录不能位于当前运行中的工作区内。"
	}
	if insidePath(destination, source) || insidePath(source, destination) || samePath(source, destination) {
		return "源目录与目标目录不能重叠。"
	}
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "源目录不存在、不是普通目录或是链接。"
	}
	if _, err := os.Lstat(destination); err == nil {
		return "目标目录已存在；预检不会覆盖任何现有目录。"
	} else if !os.IsNotExist(err) {
		return "无法确认目标目录状态。"
	}
	parent := filepath.Dir(destination)
	if info, err := os.Lstat(parent); err != nil || !info.IsDir() {
		return "目标目录的上级目录不存在。"
	}
	return ""
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func insidePath(child, parent string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// migrationPython returns an absolute interpreter path. Relative names are
// rejected so PATH changes cannot redirect a privileged panel operation.
func migrationPython() string {
	if configured := strings.TrimSpace(os.Getenv("WB2A_PYTHON")); configured != "" {
		if filepath.IsAbs(configured) {
			if info, err := os.Stat(configured); err == nil && info.Mode().IsRegular() {
				return configured
			}
		}
		return ""
	}
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			if absolute, err := filepath.Abs(path); err == nil {
				return absolute
			}
		}
	}
	return ""
}

// limitedBuffer bounds subprocess output; a malicious or broken tool cannot
// exhaust panel memory.
type limitedBuffer struct {
	buffer *bytes.Buffer
	limit  int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	remaining := w.limit - w.buffer.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		w.buffer.Write(p[:remaining])
		return len(p), nil
	}
	return w.buffer.Write(p)
}
