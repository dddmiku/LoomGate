package taskproviders

import (
	"errors"
	"testing"

	"workbuddy2api/internal/upstream"
)

// 用户截图：14 项任务目录读取失败全部显示同一个 module_discovery_failed，
// 面板只能重复「模块状态读取失败」，无法判断是上游 5xx、账号未登录、还是该
// 账号本来没有这项能力。失败必须保留可分类的原因。
func TestDiscoveryFailureKeepsAClassifiableReason(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{&upstream.Error{Kind: upstream.ErrServer, Status: 500}, "upstream_unavailable"},
		{&upstream.Error{Kind: upstream.ErrSessionDead, Status: 401}, "authentication_required"},
		{&upstream.Error{Kind: upstream.ErrNotFound, Status: 404}, "module_not_offered"},
		{&upstream.Error{Kind: upstream.ErrAccountFault, Status: 403}, "account_unavailable"},
		{&upstream.Error{Kind: upstream.ErrSoftRate, Status: 429}, "upstream_rate_limited"},
		{errors.New("buddy_state_missing"), "state_unrecognized"},
		{errors.New("totally-unknown"), "module_discovery_failed"},
	} {
		if got := discoveryReason(test.err); got != test.want {
			t.Errorf("discoveryReason(%v)=%q want %q", test.err, got, test.want)
		}
	}
}

// 原因码必须是稳定的枚举值，不能把上游原文、账号或链接带进面板。
func TestDiscoveryReasonIsAStableCode(t *testing.T) {
	got := discoveryReason(errors.New("upstream said https://secret.invalid/token=abc for account 0793"))
	if got != "module_discovery_failed" {
		t.Fatalf("raw error text leaked into reason code: %q", got)
	}
}
