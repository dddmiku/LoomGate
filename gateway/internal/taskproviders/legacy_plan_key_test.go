package taskproviders

import (
	"context"
	"path/filepath"
	"testing"
	"time"
	"workbuddy2api/internal/taskengine"
)

// 旧 config 的 growth_hours 表示「成长中心这条流水线」，没有具体任务键；
// 而引擎按 r.TaskKey == t.Key 逐字过滤。本轮实证：导入计划用模块名 "growth"
// 时 CatalogDetails(...,"growth") 返回 0 个任务，Submit 出来的 run 是
// status=skipped、steps=0——计划看着启用、按点触发、实际什么都不做。
//
// 这条测试锁定执行器的两边：裸模块名确实匹配不到任何任务（因此导入侧必须
// 换成通配符），而通配符能展开到当时真实存在的任务。
func TestLegacyModuleNameKeyCannotMatchDiscoveredTasks(t *testing.T) {
	a, _, _ := setupModules(t)
	e, err := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "tasks.json"), PollMin: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Register("workbuddy", a)

	bare, err := e.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{AccountIDs: []string{"cn-fixture"}}, "growth")
	if err != nil {
		t.Fatalf("catalog error: %v", err)
	}
	if len(bare.Tasks) != 0 {
		t.Fatalf("测试前提失效：裸模块名匹配到了 %d 个任务", len(bare.Tasks))
	}
	r, err := e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "growth", AccountIDs: []string{"cn-fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	if run := waitModuleRun(t, e, r.ID); run.Status != taskengine.StatusSkipped || len(run.Steps) != 0 {
		t.Fatalf("裸模块名应静默跳过（这正是导入缺陷的表现）：status=%s steps=%d", run.Status, len(run.Steps))
	}

	// 迁移侧用的是模块前缀（不是全局 "*"）：既覆盖该模块当前的全部任务，
	// 又不把签到、余额刷新等无关侧写任务带进同一条计划。
	growthCatalog, err := e.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{AccountIDs: []string{"cn-fixture"}}, "growth/")
	if err != nil {
		t.Fatalf("catalog error: %v", err)
	}
	if len(growthCatalog.Tasks) == 0 {
		t.Fatal("模块前缀必须能展开到真实的 growth/<任务码>，否则迁移后的计划仍然什么都不做")
	}
	for _, task := range growthCatalog.Tasks {
		if len(task.Key) <= len("growth/") || task.Key[:len("growth/")] != "growth/" {
			t.Fatalf("模块前缀不得带出其他模块的任务：%q", task.Key)
		}
	}

	// 全局 "*" 仍然覆盖全部任务，但它不是迁移语义——这里只确认前缀不是特例。
	all, err := e.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{AccountIDs: []string{"cn-fixture"}}, "*")
	if err != nil {
		t.Fatalf("catalog error: %v", err)
	}
	if len(all.Tasks) < len(growthCatalog.Tasks) {
		t.Fatal("通配符结果不能少于模块前缀的结果")
	}

	// 端到端：用迁移侧的真实键提交一次运行，必须真的展开到任务并产生步骤，
	// 而不是像裸模块名那样 status=skipped、steps=0。
	r2, err := e.Submit(taskengine.Request{ProviderID: "workbuddy", TaskKey: "growth/", AccountIDs: []string{"cn-fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	run := waitModuleRun(t, e, r2.ID)
	if run.Status == taskengine.StatusSkipped || len(run.Steps) == 0 {
		t.Fatalf("模块前缀计划没有展开到真实任务：status=%s steps=%d", run.Status, len(run.Steps))
	}
}

// TestLegacyTravelPrefixReachesTheTravelModule 锁定 travel_hours 的映射：
// 旧 config 的 travel_hours 表示 Buddy 旅行流水线，其发现键是 travel/<动作>。
// 若导入成裸 "travel"，引擎逐字过滤会得到 0 个任务；导入成 "travel/" 才能走到
// travelModule（无 Buddy 时它提供明确的 manual 领养任务，而不是静默消失）。
func TestLegacyTravelPrefixReachesTheTravelModule(t *testing.T) {
	a, _, _ := setupModules(t)
	e, err := taskengine.Open(taskengine.Options{Path: filepath.Join(t.TempDir(), "tasks.json"), PollMin: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Register("workbuddy", a)

	bare, err := e.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{AccountIDs: []string{"cn-fixture"}}, "travel")
	if err != nil {
		t.Fatalf("catalog error: %v", err)
	}
	if len(bare.Tasks) != 0 {
		t.Fatalf("测试前提失效：裸 travel 匹配到了 %d 个任务", len(bare.Tasks))
	}
	prefix, err := e.CatalogDetails(context.Background(), "workbuddy", taskengine.Scope{AccountIDs: []string{"cn-fixture"}}, "travel/")
	if err != nil {
		t.Fatalf("catalog error: %v", err)
	}
	if len(prefix.Tasks) == 0 {
		t.Fatal("travel/ 必须能展开到 travelModule 的任务，否则迁移后的旅行计划什么都不做")
	}
	for _, task := range prefix.Tasks {
		if len(task.Key) <= len("travel/") || task.Key[:len("travel/")] != "travel/" {
			t.Fatalf("travel/ 前缀不得带出其他模块的任务：%q", task.Key)
		}
	}
}
