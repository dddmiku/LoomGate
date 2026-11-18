package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
)

// v3Fixture /v3/config 响应（对象形态）：3 个模型，hy4-preview 带 v3 口径 credits
// （x0.29——与 v2 的 x0.00 分歧正是任务书「v3 为主时以 v3 为准」的判据），
// deepseek-v4.1-flash 是 v3 独有模型（缺 4 模型问题的主角）。
// 2026-11-18 P1-01 修正：真实 /v3/config 响应（2026-10-29 抓包，
// _wb_capture_new/20261029-103215.996-032-v3-config-resp.json）带 agents[cli]
// 白名单（58 个模型里 cli 只允许 16 个）。无 agents 的 v3 后端全量表**不得**
// 扩充客户端可见集——那正是用户报的「隐藏模型全列出来」。本 fixture 补上
// agents[cli]，让「v3 主」语义建立在白名单之上；deepseek-v4.1-flash
// 必须在白名单里才会公布。
const v3Fixture = `{"code":0,"data":{"models":[
	{"id":"glm-5.2","name":"GLM-5.2","credits":"x0.05","maxInputTokens":131072,"maxOutputTokens":8192},
	{"id":"hy4-preview","name":"Hy4","credits":"x0.29","maxInputTokens":256000,"maxOutputTokens":32000},
	{"id":"deepseek-v4.1-flash","name":"DS-Flash","credits":"x0.00","maxInputTokens":1000000,"maxOutputTokens":128000},
	{"id":"hidden-backend-model","name":"HiddenBackend","credits":"x0.00","maxInputTokens":100000,"maxOutputTokens":8192}
],"agents":[{"name":"cli","models":["glm-5.2","hy4-preview","deepseek-v4.1-flash"]}]}}`

// v2Fixture /v2/enterprises/personal/models 响应（对象形态，无 agents——global 探测
// 口径不读 agents）：glm-5.2 与 v3 重复；hy4-preview credits 与 v3 分歧（x0.00）；
// gpt-5.3-codex 是 v2 独有模型（任务书点名「作为补充进并集」）。
const v2Fixture = `{"code":0,"data":{"models":[
	{"id":"glm-5.2","name":"GLM-5.2","credits":"x0.05-v2","maxInputTokens":131072,"maxOutputTokens":8192},
	{"id":"hy4-preview","name":"Hy4","credits":"x0.00","maxInputTokens":256000,"maxOutputTokens":32000},
	{"id":"gpt-5.3-codex","name":"Codex","credits":"x1.00","maxInputTokens":131072,"maxOutputTokens":32768}
]}}`

// v3V2ProbeSrv 构造 v3/v2 两路不同响应的探测 fake：/v3/config 返回 v3Fixture，
// /v2 返回 v2Fixture，/console 500（家族兜底不触发）。calls 由调用方声明并传入
// （globalModelsSrv 经 *calls 并发安全地追加，调用方探测后读取即最新值）。
func v3V2ProbeSrv(t *testing.T, calls *[]string, v3Status int, v3Body string) *httptest.Server {
	t.Helper()
	return globalModelsSrv(t, calls, nil, func(path string) (int, string) {
		switch path {
		case "/v3/config":
			return v3Status, v3Body
		case "/v2/enterprises/personal/models":
			return 200, v2Fixture
		case "/console/enterprises/personal/models":
			return 500, `{"code":500,"msg":"boom"}`
		}
		return 404, `{"code":404,"msg":"nope"}`
	})
}

// TestGlobalModelsMergeV3PrimaryV2Supplement 合并优先级（任务书需求 2）：
// /v3/config 为主、/v2 只补缺失——
//   - v3 独有 deepseek-v4.1-flash 进并集（缺 4 模型问题修复判据）；
//   - v2 独有 gpt-5.3-codex 作为补充进并集（任务书点名）；
//   - 重复 id（glm-5.2/hy4-preview）去重为 1，且条目字段以 v3 为准
//     （hy4-preview credits=x0.29 非 v2 的 x0.00；glm-5.2 credits=x0.05 非 x0.05-v2）；
//   - 输出顺序稳定：v3 原序在前、v2 补充项在后。
func TestGlobalModelsMergeV3PrimaryV2Supplement(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := v3V2ProbeSrv(t, &calls, 200, v3Fixture)
	defer srv.Close()

	c := globalModelsClient(t, srv)
	names := c.FetchGlobalModels(globalAcct())
	infos := c.FetchGlobalModelInfos(globalAcct())

	// 两路并发各一次（v2 200 → 不打 /console）。
	if len(calls) != 2 {
		t.Fatalf("probe calls=%v want 2 (v3 + v2)", calls)
	}

	// 名单：v3 白名单原序（glm-5.2, hy4-preview, deepseek-v4.1-flash）+ v2 补充
	// （gpt-5.3-codex）。hidden-backend-model 在 v3 全量表里但不在 cli 白名单
	// 里 → 不得公布（P1-01：后端全量表不得扩充可见集）。
	want := []string{"glm-5.2", "hy4-preview", "deepseek-v4.1-flash", "gpt-5.3-codex"}
	if !sameStrings(names, want) {
		t.Fatalf("merged names=%v want %v (v3 primary order + v2 supplement)", names, want)
	}

	// 倍率口径：v3 为主时以 v3 为准（v2 补充的模型带 v2 的 credits）。
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	if got := byID["hy4-preview"].Credits; got != "x0.29" {
		t.Errorf("hy4-preview credits=%q want x0.29 (v3 primary overrides v2 x0.00)", got)
	}
	if got := byID["glm-5.2"].Credits; got != "x0.05" {
		t.Errorf("glm-5.2 credits=%q want x0.05 (v3 primary)", got)
	}
	if got := byID["gpt-5.3-codex"].Credits; got != "x1.00" {
		t.Errorf("gpt-5.3-codex credits=%q want x1.00 (v2 supplement carries v2 credits)", got)
	}
	if got := byID["deepseek-v4.1-flash"].ContextWindow; got != 1000000 {
		t.Errorf("deepseek-v4.1-flash contextWindow=%d want 1000000 (v3 entry)", got)
	}
	if _, ok := byID["hidden-backend-model"]; ok {
		t.Errorf("hidden-backend-model escaped the cli allowlist: %v", names)
	}
}

// TestGlobalModelsMergeV3FailDegradesToV2 /v3 失败（400 code 12403 UA 门禁形态）→
// 降级为仅 /v2 结果（任务书实现要点：/v3 失败不得影响 /v2），名单 = v2 原样。
func TestGlobalModelsMergeV3FailDegradesToV2(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := v3V2ProbeSrv(t, &calls, 400, `{"code":12403,"msg":"check ua, get coding copilot version error"}`)
	defer srv.Close()

	names := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())

	// v3 400 + v2 200：降级为 v2 结果（v3 不拖累）。
	if len(calls) != 2 {
		t.Fatalf("probe calls=%v want 2 (v3 attempted + v2 succeeded)", calls)
	}
	want := []string{"glm-5.2", "hy4-preview", "gpt-5.3-codex"}
	if !sameStrings(names, want) {
		t.Fatalf("degraded names=%v want %v (v2-only result)", names, want)
	}
	// v2 降级口径：v3 独有模型（deepseek-v4.1-flash）在降级名单中不出现。
	for _, id := range names {
		if id == "deepseek-v4.1-flash" {
			t.Fatalf("v3-only model %s must not appear on v3 failure", id)
		}
	}
}

// TestGlobalModelsMergeDedup 重复 id 去重（任务书实现要点：去重 key 是模型 id）：
// 两路各含 glm-5.2/hy4-preview，并集中各只出现一次。
func TestGlobalModelsMergeDedup(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := v3V2ProbeSrv(t, &calls, 200, v3Fixture)
	defer srv.Close()

	names := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())
	_ = calls

	counts := map[string]int{}
	for _, id := range names {
		counts[id]++
	}
	for _, id := range []string{"glm-5.2", "hy4-preview"} {
		if counts[id] != 1 {
			t.Errorf("shared model %s count=%d want 1 (dedupe by id)", id, counts[id])
		}
	}
}

// TestGlobalModelsMergeStableOutput 合并后排序保持稳定输出（任务书实现要点）：
// 连续两次探测（新 Client 重置缓存）输出顺序一致——v3 原序在前、v2 补充在后，
// 不依赖 map 迭代序。
func TestGlobalModelsMergeStableOutput(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := v3V2ProbeSrv(t, &calls, 200, v3Fixture)
	defer srv.Close()

	first := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())
	second := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())
	if !sameStrings(first, second) {
		t.Fatalf("merge output unstable: %v vs %v", first, second)
	}
	want := []string{"glm-5.2", "hy4-preview", "deepseek-v4.1-flash", "gpt-5.3-codex"}
	if !sameStrings(first, want) {
		t.Fatalf("names=%v want %v", first, want)
	}
}

// TestCNModelsV3PrimaryConsoleSupplement CN 侧合并（P1-01 修正后口径）：
// console 的 agents[cli] 白名单决定集合（v3 无白名单时只补元数据、不扩集合）——
// v3 独有的 hy4-preview-f 不在 cli 白名单里 → 不公布；console 独有 hy4-preview
// 在白名单里 → 公布；共有 glm-5.2 以 v3 富字段为主（credits 由元数据填充）。
func TestCNModelsV3PrimaryConsoleSupplement(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	// v3 面（对象形态无 agents 过滤；fetchV3Models 不按 cli 过滤，补缺进并集即生效）。
	v3Body := `{"code":0,"data":{"models":[
		{"id":"glm-5.2","name":"GLM-5.2","credits":"x0.05","maxInputTokens":131072,"maxOutputTokens":8192},
		{"id":"hy4-preview-f","name":"Hy4F","credits":"x0.00","maxInputTokens":256000,"maxOutputTokens":32000}
	]}}`
	consoleBody := `{"code":0,"data":{"models":[
		{"id":"glm-5.2","name":"GLM-5.2","credits":"x0.05","maxInputTokens":131072,"maxOutputTokens":8192},
		{"id":"hy4-preview","name":"Hy4","credits":"x0.00","maxInputTokens":256000,"maxOutputTokens":32000}
	],"agents":[{"name":"cli","models":["glm-5.2","hy4-preview"]}]}}`

	var mu sync.Mutex
	var paths []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, v3Body), nil
		default:
			return jsonResp(200, consoleBody), nil
		}
	})
	cn := &auth.Auth{AccessToken: "at", UID: "cn1", Domain: "www.codebuddy.cn"}

	infos, err := c.FetchModels(cn)
	if err != nil {
		t.Fatalf("cn fetch models: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths=%v want 2 (console + v3 concurrent)", paths)
	}
	// console 白名单集合：glm-5.2（v3 富字段补 blank）+ hy4-preview（console 补充）。
	// hy4-preview-f 不在 cli 白名单 → 不公布（v3 无白名单时只补元数据不扩集合）。
	byID := map[string]ModelInfo{}
	var ids []string
	for _, mi := range infos {
		byID[mi.ID] = mi
		ids = append(ids, mi.ID)
	}
	for _, want := range []string{"glm-5.2", "hy4-preview"} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("merged cn models missing %s: %v", want, ids)
		}
	}
	if _, ok := byID["hy4-preview-f"]; ok {
		t.Fatalf("non-allowlisted v3-only model published: %v", ids)
	}
	if counts := countIDs(ids, "glm-5.2"); counts != 1 {
		t.Errorf("glm-5.2 count=%d want 1 (dedupe)", counts)
	}
}

// TestCNModelsV3FailDegradesToConsole CN 侧 /v3 失败降级（对称 global）：
// console 结果原样返回，v3 独有模型不出现。
func TestCNModelsV3FailDegradesToConsole(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	consoleBody := `{"code":0,"data":{"models":[
		{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192}
	],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`

	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(400, `{"code":12403,"msg":"check ua, get coding copilot version error"}`), nil
		default:
			return jsonResp(200, consoleBody), nil
		}
	})
	cn := &auth.Auth{AccessToken: "at", UID: "cn1", Domain: "www.codebuddy.cn"}

	infos, err := c.FetchModels(cn)
	if err != nil {
		t.Fatalf("cn fetch models: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "glm-5.2" {
		t.Fatalf("degraded cn infos=%+v want [glm-5.2] (console-only)", infos)
	}
}

// countIDs 统计 id 出现次数（去重断言用）。
func countIDs(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}
