package taskproviders

import "workbuddy2api/internal/taskengine"

// An internal plan anchor; sequence execution dynamically discovers these task families.
func newLegacyCheckinSequenceModule(a *Accounts) (Module, error) {
	if a.Provider != "workbuddy" {
		return nil, nil
	}
	return &sequenceMarkerModule{moduleBase: moduleBase{
		spec:       ModuleSpec{ID: "legacy_checkin_sequence", Key: "checkin-balance-sequence", InternalOnly: true, Realms: []string{"cn", "global"}},
		definition: taskengine.Definition{Key: "checkin-balance-sequence", Name: "签到与余额刷新", Description: "按账号顺序安全检查凭据、执行适用的签到并读取余额。", SideEffect: true, Stages: []taskengine.Stage{taskengine.StageInspect}},
	}}, nil
}

var legacyCheckinSequencePlan = &DefaultPlan{ID: "legacy-checkin-sequence", TaskKey: "checkin-balance-sequence", TaskSequence: []string{"credential-preflight", "checkin", "balance"}, Hours: []int{9, 21}, LegacyEnabled: boolValue(true)}
