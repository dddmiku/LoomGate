package panel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildStateCarriesNicknameBalanceAndPoolForAccountsPage 锁定账号页数据链路：
// 昵称来自已核验账号文档（不是 UID）、余额 0 不等于缺失、池状态逐项透出。
func TestBuildStateCarriesNicknameBalanceAndPoolForAccountsPage(t *testing.T) {
	h := newHarness(t)
	_ = h.writeCredentials()
	writeAuthFixture(t, h.server.opts.AuthDir, "workbuddy-bbb22222.json", "bbb22222")
	h.runner.credit = []byte(`{"accounts":[{"uid":"bbb22222","remain":0,"size":200,"used":200,` +
		`"expiring":0,"expiry":{},"expiring_window_hours":72,"ok":true}],"total":{"remain":0}}`)
	h.transport.replies["GET /status"] = stubReply{Code: 200, Body: `{"total":1,"healthy":0,"cooling":0,"disabled":1,` +
		`"accounts":[{"uid":"bbb22222","credits":0,"disabled":true,"disabled_reason":"manual","credits_exhausted":true}]}`}
	h.transport.replies["GET /healthz"] = stubReply{Code: 200, Body: `{"version":"v9.9.9"}`}

	payload := h.server.state.Build(context.Background(), true)
	if len(payload.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(payload.Accounts))
	}
	acct := payload.Accounts[0]
	if acct.Nickname != "昵称" || acct.Nickname == acct.UID {
		t.Fatalf("账号页必须展示已核验昵称而非 UID：nickname=%q uid=%q", acct.Nickname, acct.UID)
	}
	if !acct.Credits.Supported {
		t.Fatal("credits 必须可用")
	}
	raw, err := json.Marshal(acct)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if row["nickname"] != "昵称" {
		t.Fatalf("serialized nickname=%v", row["nickname"])
	}
	credits, _ := row["credits"].(map[string]any)
	if credits == nil || credits["remain"] != float64(0) {
		t.Fatalf("余额 0 必须原样透出：%v", row["credits"])
	}
	pool, _ := row["pool"].(map[string]any)
	if pool == nil || pool["disabled"] != true || pool["creditsExhausted"] != true {
		t.Fatalf("池状态未透出：%v", row["pool"])
	}
	for _, key := range []string{`"nickname"`, `"credits"`, `"pool"`, `"expiresAt"`, `"realm"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("账号行缺少键 %s：%s", key, raw)
		}
	}
}
