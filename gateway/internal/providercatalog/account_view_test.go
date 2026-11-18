package providercatalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/accountvault"
)

type staticAccounts []accountvault.Account

func (s staticAccounts) List() ([]accountvault.Account, error) { return s, nil }

// TestAccountViewsExposeOnlyVerifiedNonSecretDisplayFacts 锁定 P1-04：
// /workbench/accounts 只返回 vault 元数据 + 已核验的 BotID/核验时间；
// 没有绑定时 nickname 保持空（前端显示「未提供」），不得把账号 ID 冒充昵称，
// 也不得泄漏任何会话材料。
func TestAccountViewsExposeOnlyVerifiedNonSecretDisplayFacts(t *testing.T) {
	verified := time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)
	s := New(Options{
		Accounts: staticAccounts{
			{ID: "bot-account-a", Provider: "doubao", Revision: 3, Enabled: true},
			{ID: "bot-account-b", Provider: "doubao", Revision: 1, Enabled: false},
		},
		DoubaoBinding: func(id string) (string, time.Time, bool) {
			if id == "bot-account-a" {
				return "bot_abc123", verified, true
			}
			return "", time.Time{}, false
		},
	})
	rows, err := s.accounts()
	if err != nil {
		t.Fatal(err)
	}
	views := s.accountViews(rows)
	raw, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, leak := range []string{"ciphertext", "nonce", "headers", "query", "authorization", "cookie"} {
		if strings.Contains(strings.ToLower(text), leak) {
			t.Fatalf("account view leaked %q: %s", leak, text)
		}
	}
	if len(views) != 2 || views[0].ID != "bot-account-a" || views[0].Nickname != "" {
		t.Fatalf("account view shape/order wrong or invented nickname: %+v", views)
	}
	if views[0].BotID != "bot_abc123" || views[0].VerifiedAt == nil || !views[0].VerifiedAt.Equal(verified) {
		t.Fatalf("verified binding facts missing: %+v", views[0])
	}
	if views[1].BotID != "" || views[1].VerifiedAt != nil {
		t.Fatalf("unbound account must not report invented binding: %+v", views[1])
	}
}
