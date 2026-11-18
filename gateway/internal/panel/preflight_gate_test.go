package panel

import (
	"net/http"
	"strings"
	"testing"
)

// 迁移预检会启动 Python 子进程并读取任意绝对路径目录树。它必须先过会话闸门；
// 目前它被放在 session 检查之前，只要 Origin 为空或同源 + X-Admin-Request 头就能
// 在未登录时触发，LAN 开放时等于把「扫描任意目录」暴露给同网段任何人。
func TestMigrationPreflightRequiresSession(t *testing.T) {
	h := newHarness(t)
	body := `{"source":"D:/synthetic-old","destination":"D:/synthetic-new"}`
	recorder := h.do(http.MethodPost, "/api/migration/preflight", body,
		map[string]string{"X-Admin-Request": "1"})
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未登录调用迁移预检应 401，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "源目录") {
		t.Fatalf("未登录响应不得泄露路径探测结果：%s", recorder.Body.String())
	}
}

// 登录后仍然要能正常进入预检（不能被闸门误伤）。
func TestMigrationPreflightWorksForSignedInAdmin(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	recorder := h.postJSON(doc, "/api/migration/preflight", map[string]any{
		"source": "D:/synthetic-absent-old", "destination": "D:/synthetic-new",
	})
	if recorder.Code == http.StatusUnauthorized || recorder.Code == http.StatusForbidden {
		t.Fatalf("已登录管理员被误拦：%d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("不存在的源目录应 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}
