package taskproviders

// WorkBuddy 任务注册表。
//
// 这里只有「哪些任务文件存在」，没有任何任务实现、任务名或定时。增删任务=
// 增删本文件一行 + 增删对应 task_*.go 文件。顺序即面板默认顺序。

// 顺序与拆分前一致：checkin、balance、growth、lifecycle 五件、rewards 两件，
// 最后 keepalive。外部测试与夹具按索引取模块（如 Module[0] 是签到），
// 因此这个顺序也是兼容契约。
var workbuddyCatalog = []moduleCatalogEntry{
	{provider: "workbuddy", plan: checkinPlan, build: newCheckinModule},
	{provider: "workbuddy", plan: balancePlan, build: newBalanceModule},
	{provider: "workbuddy", plan: growthPlan, build: newGrowthModule},
	{provider: "workbuddy", plan: travelPlan, build: newTravelModule},
	{provider: "workbuddy", plan: redemptionPlan, build: newRedemptionModule},
	{provider: "workbuddy", plan: makeupPlan, build: newMakeupModule},
	{provider: "workbuddy", plan: resourcesPlan, build: newResourcesModule},
	{provider: "workbuddy", plan: compensationPlan, build: newCompensationModule},
	{provider: "workbuddy", plan: lotteryPlan, build: newLotteryModule},
	{provider: "workbuddy", plan: buddyBoxPlan, build: newBuddyBoxModule},
	{provider: "workbuddy", plan: keepalivePlan, build: newKeepaliveModule},
	{provider: "workbuddy", plan: nil, build: newCheckinRefreshModule},
	{provider: "workbuddy", plan: growthSequencePlan, build: newGrowthSequenceModule},
	{provider: "workbuddy", plan: legacyCheckinSequencePlan, build: newLegacyCheckinSequenceModule},
}
