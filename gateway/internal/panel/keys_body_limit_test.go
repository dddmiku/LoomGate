package panel

import (
	"strings"
	"testing"
)

// 面板自己把密钥 body 限在 8192 字节，但密钥库允许 64 项 × 每项 256 字节的模型绑定。
// 合法配置（例如 64 个自动发现的长模型名）的 JSON 能到十几 KB，会在面板层被 413
// 拦下，而网关本身完全接受——用户只看到「请求体超过大小限制」，没有任何字段超限。
func maximumModelBinding() []any {
	models := make([]any, 0, 64)
	for i := 0; i < 64; i++ {
		// 每个名字 200 字节左右，带合法前缀；末尾两字符保证唯一。
		models = append(models, "autoclaw:m"+strings.Repeat("m", 190)+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	return models
}

func TestKeysPostAcceptsMaximumLegalModelBinding(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	models := maximumModelBinding()
	h.transport.replies["POST /keys"] = stubReply{Code: 201, Body: `{"ok":true,"entry":{},"key":"sk-x"}`}
	recorder := h.postJSON(doc, "/api/keys", map[string]any{"name": "k", "models": models})
	if recorder.Code != 201 {
		t.Fatalf("合法最大模型绑定应被接受，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	calls := h.transport.recorded()
	if len(calls) != 1 || calls[0].Endpoint != "/keys" {
		t.Fatalf("应恰好转发一次 POST /keys：%v", calls)
	}
	for _, model := range models {
		if !strings.Contains(calls[0].Body, model.(string)) {
			t.Fatalf("转发体丢失模型绑定：%s", model.(string))
		}
	}
}

// 上限仍然存在：超过 64 KiB 的请求体必须继续拒掉，不能因为放宽到合法最大值就变成无界。
func TestKeysPostStillRejectsOversizedBody(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	note := strings.Repeat("x", 70<<10)
	recorder := h.postJSON(doc, "/api/keys", map[string]any{"name": "k", "note": note})
	if recorder.Code != 413 {
		t.Fatalf("超限请求体应 413，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if len(h.transport.recorded()) != 0 {
		t.Fatal("超限请求不得转发给网关")
	}
}
