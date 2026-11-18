package taskengine

import "testing"

// TaskKeyMatches 是计划/请求的任务键过滤口径。逐字比较会让「模块级计划」
// （旧 config 只有 growth_hours 这种粒度）在运行时发现不到任何任务，
// 计划按点触发却以 skipped 结束。这里锁住三条语义：
//   - "" 与 "*" 覆盖全部；
//   - 逐字相等覆盖该任务；
//   - 以 "/" 结尾按前缀覆盖该模块当前的全部任务。
func TestTaskKeyMatches(t *testing.T) {
	cases := []struct {
		pattern, key string
		want         bool
	}{
		{"", "growth/accept/synthetic", true},
		{"*", "checkin", true},
		{"checkin", "checkin", true},
		{"checkin", "growth/accept/x", false},
		{"growth/", "growth/accept/synthetic", true},
		{"growth/", "growth/synthetic", true},
		{"growth/", "growth", false},
		{"growth/", "growthX/a", false},
		{"growth/", "checkin", false},
		{"travel/", "travel/depart", true},
		{"travel/", "travel/claim", true},
		{"travel/", "redeem/tier", false},
		{"growth/accept/", "growth/accept/synthetic", true},
		{"growth/accept/", "growth/synthetic", false},
		// 不带斜杠的模块名不是前缀：这正是迁移缺陷的形态，必须匹配不到。
		{"growth", "growth/accept/synthetic", false},
	}
	for _, tc := range cases {
		if got := TaskKeyMatches(tc.pattern, tc.key); got != tc.want {
			t.Fatalf("TaskKeyMatches(%q,%q)=%v want %v", tc.pattern, tc.key, got, tc.want)
		}
	}
}
