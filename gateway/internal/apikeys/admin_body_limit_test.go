package apikeys

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// 密钥库允许 64 项、每项最多 256 字节的模型绑定，合法请求体可以超过 8 KiB。
// 管理接口的 body 上限必须容得下这种合法配置，否则「模型多选」会被 400 拦下，
// 而错误文案说「请输入有效的密钥信息」，指向的是内容而不是大小。
func TestAdminAcceptsMaximumLegalModelBinding(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	models := make([]string, 0, MaxBoundModels)
	for i := 0; i < MaxBoundModels; i++ {
		models = append(models, "autoclaw:m"+strings.Repeat("m", 190)+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	payload, err := json.Marshal(map[string]any{"name": "many-models", "models": models})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) <= 8192 {
		t.Fatalf("测试前提不成立：合法请求体只有 %d 字节", len(payload))
	}
	recorder := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/keys", strings.NewReader(string(payload))))
	if recorder.Code != 201 {
		t.Fatalf("合法最大绑定应创建成功，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		Entry Info `json:"entry"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Entry.Models) != MaxBoundModels {
		t.Fatalf("模型绑定被截断：%d", len(created.Entry.Models))
	}
}

// 上限仍在：超过 AdminBodyLimit 的请求体必须被拒，且不能落盘。
func TestAdminRejectsBodyBeyondLimit(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"name":"k","note":"` + strings.Repeat("x", AdminBodyLimit) + `"}`
	recorder := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/keys", strings.NewReader(body)))
	if recorder.Code != 400 {
		t.Fatalf("超限请求体应 400，实际 %d", recorder.Code)
	}
	if len(s.List()) != 0 {
		t.Fatal("超限请求不得写入密钥库")
	}
}
