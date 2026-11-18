package upstream

import (
	"net/http"
	"testing"
)

func TestClientModelDirectoryNeverAddsModelsOutsideCLIAgent(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[{"id":"fixture-visible","name":"Visible","credits":"x0.12"},{"id":"fixture-hidden","name":"Hidden"}],"agents":[{"name":"cli","models":["fixture-visible"]}]}}`), nil
	})
	rows, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "fixture-visible" {
		t.Fatal("non-client model escaped CLI allowlist")
	}
	if rows[0].Credits != "x0.12" {
		t.Fatal("visible client multiplier lost")
	}
}

func TestGlobalClientModelDirectoryHonorsPresentAgentAllowlist(t *testing.T) {
	names, _, _, _, err := parseGlobalModelNames([]byte(`{"code":0,"data":{"models":[{"id":"fixture-visible"},{"id":"fixture-hidden"}],"agents":[{"name":"cli","models":["fixture-visible"]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "fixture-visible" {
		t.Fatal("global client allowlist ignored")
	}
}

// TestClientDirectoryEmptyAllowlistPublishesNothing 锁定：agents[cli].models
// 显式为空数组时，目录必须是空的——绝不能因为「白名单没有条目」而退回全量
// 后端模型列表（那正是用户报的隐藏模型泄漏）。
func TestClientDirectoryEmptyAllowlistPublishesNothing(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[{"id":"fixture-backend-only","name":"Backend"}],"agents":[{"name":"cli","models":[]}]}}`), nil
	})
	// A zero-length allowlist must never fall back to the backend list. Both
	// outcomes are safe: an explicit error, or an empty directory.
	rows, err := c.FetchModels(authStub())
	if err == nil && len(rows) != 0 {
		t.Fatalf("empty CLI allowlist broadened discovery: %+v", rows)
	}
}

// TestClientDirectoryExcludesDisabledAllowlistedModels 锁定：即使模型在白名单
// 里，disabled=true 的条目也不得公布（白名单是上限，不是无视停用的通行证）。
func TestClientDirectoryExcludesDisabledAllowlistedModels(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[{"id":"fixture-on","name":"On"},{"id":"fixture-off","name":"Off","disabled":true}],"agents":[{"name":"cli","models":["fixture-on","fixture-off"]}]}}`), nil
	})
	rows, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "fixture-on" {
		t.Fatalf("disabled allowlisted model leaked: %+v", rows)
	}
}

// TestGlobalClientDirectoryEmptyAllowlistPublishesNothing 同口径覆盖 global 解析路径。
func TestGlobalClientDirectoryEmptyAllowlistPublishesNothing(t *testing.T) {
	names, _, _, _, err := parseGlobalModelNames([]byte(`{"code":0,"data":{"models":[{"id":"fixture-backend-only"}],"agents":[{"name":"cli","models":[]}]}}`))
	if err == nil && len(names) != 0 {
		t.Fatalf("empty global CLI allowlist broadened discovery: %v", names)
	}
}
