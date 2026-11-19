package taskproviders

// Trae 任务注册表。这里只有任务文件清单，实现与默认定时都在各自的 task_*.go。

var traeCatalog = []moduleCatalogEntry{
	{provider: "trae", plan: traeCheckinPlan, build: newTraeCheckinModule},
	{provider: "trae", plan: traeBalancePlan, build: newTraeBalanceModule},
	{provider: "trae", plan: traeKeepalivePlan, build: newKeepaliveModule},
}
