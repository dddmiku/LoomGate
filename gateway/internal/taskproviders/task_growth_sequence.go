package taskproviders

import "workbuddy2api/internal/taskengine"

// An internal schedule anchor; sequence execution dynamically discovers these families.
func newGrowthSequenceModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" {
		return nil, nil
	}
	return &sequenceMarkerModule{moduleBase: moduleBase{
		spec:       ModuleSpec{ID: "growth_sequence", Key: "growth-sequence", InternalOnly: true, Realms: []string{"cn"}},
		definition: taskengine.Definition{Key: "growth-sequence", Name: "成长中心有序流水线", Description: "按每个账号顺序动态发现旅行、成长领奖、补登、兑换、抽奖、开箱与资源读取；未知结果停止当前账号后续阶段并保留复核游标。", SideEffect: true, Stages: []taskengine.Stage{taskengine.StageInspect}},
	}}, nil
}

var growthSequencePlan = &DefaultPlan{ID: "legacy-growth-sequence", TaskKey: "growth-sequence", TaskSequence: []string{"travel/", "growth/", "makeup", "redeem/", "lottery", "buddy_box", "resources"}, Hours: []int{12}, Minute: 0, LegacyEnabled: boolValue(true)}
