package hotupdate

import (
	"encoding/json"
	"testing"
	"workbuddy2api/internal/version"
)

// 面板要显示「签名校验：已启用 / 未内置发布公钥」，就必须从状态里读到真实能力，
// 而不是靠前端猜。开发构建（未注入 ReleaseKey）必须报 false，注入公钥后报 true。
func TestStatusReportsSignatureVerificationCapability(t *testing.T) {
	original := version.ReleaseKey
	defer func() { version.ReleaseKey = original }()

	version.ReleaseKey = ""
	m := NewManager(Options{Enabled: true, Dir: t.TempDir()})
	m.visibility = VisibilityPublic
	status := m.Status()
	if status.SignatureVerification {
		t.Fatal("未注入公钥的构建不得声称具备验签能力")
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["signature_verification"] != false {
		t.Fatalf("wire field missing or wrong: %v", decoded["signature_verification"])
	}

	// ed25519 公钥固定 32 字节（64 个十六进制字符）。
	version.ReleaseKey = "3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c"
	if !m.Status().SignatureVerification {
		t.Fatal("注入公钥后必须报 true")
	}
}
