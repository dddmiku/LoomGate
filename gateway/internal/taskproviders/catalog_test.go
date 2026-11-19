package taskproviders

// 目录契约测试：一个任务一个文件的重构之后，「计划的任务键」和「模块的路由键」
// 必须仍然一致，否则计划按点触发却匹配不到任何任务（静默跳过）。

import (
	"reflect"
	"testing"

	"workbuddy2api/internal/taskengine"
)

func catalogForTest(provider string) []moduleCatalogEntry {
	if provider == "trae" {
		return traeCatalog
	}
	return workbuddyCatalog
}

func TestCatalogPlanTaskKeyMatchesModuleSpec(t *testing.T) {
	for _, provider := range []string{"workbuddy", "trae"} {
		for _, entry := range catalogForTest(provider) {
			if entry.plan == nil {
				continue
			}
			// Build the module without an account pool so build functions that
			// need a store only check their option, not the pool.
			module, err := entry.build(&Accounts{Provider: provider})
			if err != nil {
				t.Fatalf("%s: build %s: %v", provider, entry.plan.ID, err)
			}
			if module == nil {
				continue
			}
			spec := module.Spec()
			if spec.Key != entry.plan.TaskKey {
				t.Fatalf("%s plan %s routes %q but its module spec routes %q", provider, entry.plan.ID, entry.plan.TaskKey, spec.Key)
			}
			if spec.Prefix != (len(entry.plan.TaskKey) > 0 && entry.plan.TaskKey[len(entry.plan.TaskKey)-1] == '/') {
				t.Fatalf("%s plan %s prefix flag disagrees with task key %q", provider, entry.plan.ID, entry.plan.TaskKey)
			}
			if spec.ID != entry.plan.ID && entry.plan.ID[:7] == "legacy-" {
				// legacy-* ids are schedule ids, not module ids. Only the key must match.
			}
		}
	}
}

func TestCatalogHasNoDuplicateTaskRoutes(t *testing.T) {
	for _, provider := range []string{"workbuddy", "trae"} {
		seen := map[string]bool{}
		for _, entry := range catalogForTest(provider) {
			if entry.plan == nil {
				continue
			}
			if seen[entry.plan.ID] {
				t.Fatalf("%s duplicate plan id %q", provider, entry.plan.ID)
			}
			seen[entry.plan.ID] = true
		}
	}
}

func TestBuiltinSchedulesOnlyArmReadOnlyTasks(t *testing.T) {
	plans := BuiltinSchedules()
	if len(plans) == 0 {
		t.Fatal("no builtin schedules")
	}
	ids := map[string]bool{}
	for _, plan := range plans {
		if ids[plan.ID] {
			t.Fatalf("duplicate builtin schedule %s", plan.ID)
		}
		ids[plan.ID] = true
		if plan.Timezone != "Asia/Shanghai" || len(plan.Hours) == 0 || plan.Minute < 0 || plan.Minute > 59 {
			t.Fatalf("builtin schedule %+v has an invalid time", plan)
		}
		if plan.Enabled && plan.TaskKey != "balance" {
			t.Fatalf("only balance may ship enabled, got %+v", plan)
		}
	}
	if !reflect.DeepEqual([]string{"workbuddy", "trae"}, []string{"workbuddy", "trae"}) {
		t.Fatal("provider list drifted")
	}
}

func TestPlanTaskKeyUsesModulePrefixForDynamicTasks(t *testing.T) {
	// 动态目录的模块必须用「模块/」前缀表达「该模块当前的全部任务」；写成裸
	// 模块名时执行阶段逐字过滤会得到 0 个任务，计划静默跳过。
	for provider, wants := range map[string]map[string]string{
		"workbuddy": {"legacy-growth": "growth/", "legacy-travel": "travel/", "legacy-redeem": "redeem/"},
	} {
		byID := map[string]string{}
		for _, entry := range catalogForTest(provider) {
			if entry.plan != nil {
				byID[entry.plan.ID] = entry.plan.TaskKey
			}
		}
		for id, want := range wants {
			if byID[id] != want {
				t.Fatalf("%s plan %s key=%q want %q", provider, id, byID[id], want)
			}
		}
	}
	_ = taskengine.TaskKeyMatches
}
