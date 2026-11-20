# 统一任务模块

WorkBuddy/Trae 的任务由独立 provider 模块接入统一引擎。随包入口和计划来自本地任务文件，活动 ID、周期与奖励来自实时官方目录。任务日志采用运行中逐行追加、空闲时查询最近一轮的文本输出。逐个旧入口及子步骤见[完整覆盖矩阵](legacy-task-coverage.md)；未验证的真实活动与平台交付不因模块测试通过而变成已完成。

## 模块契约

`internal/taskproviders.Module` 提供 `Spec`、`Discover`、`Inspect`、`Execute`、`Claim`、`Verify`。`ModuleSpec` 说明本模块负责的任务 key 或前缀、服务区域；注册时拒绝重叠路由。新增业务模块在组合处注册，不需要修改 taskengine 的核心执行/调度代码。

公共任务定义、实现和默认计划分别位于各自的 `task_*.go` 文件；`workbuddy_modules.go` / `trae_modules.go` 只登记现有任务文件。删除任务就是删除对应文件和注册表一行，避免定义和实现脱节。动态成长任务的 ID、名称、周期与奖励仍从官方目录读取，随包入口不能把过期活动 ID 写死。

`Accounts` 负责账号范围、启用状态、provider/realm 校验和模块路由。每次实际操作重新检查账号；执行单一任务时仅发现对应模块，不因其他模块目录故障阻断。发现模块有上下文期限，panic 和正常失败转换为固定错误码，不暴露上游原始错误。

有完整状态读取的模块实现可选 `CatalogModule.DiscoverState`，同次返回定义和观测值，避免页面为了展示进度重复执行任务。其余模块继续使用原 `Discover`。模块目录最多256项；重复key和越界归属会变成独立发现错误。新增模块不修改核心调度器的任务表。

## 任务输出日志

新引擎通过 `taskengine.Output(ctx, line)` 给模块提供旧版脚本式的有界输出通道。输出会绑定当前 run、provider、账号、任务键、阶段并在阶段返回前持久化，因此面板查询 `/workbench/task/log?provider=<provider>&key=<key>` 时可以看到正在增长的当前执行日志；刷新或重启只读回已有行。输出仅是诊断信息，不是提交/领取成功证据，unknown、receipt、cursor 仍由动作持久化链独立决定。

模块输出会脱敏凭据、URL、邮箱、手机号和控制字符，并受单行、单次、单 run 及全局上限约束；诊断截断不清除业务回执，日志保存失败也不允许重放已提交动作。账号和 provider 归属校验防止同名任务串日志。成长领奖日志包含动态任务名称及官方领取回执明确返回的积分/能量；没有回执字段时保持待核验，不补写目录预告奖励。资源页按能量、抽奖机会、Buddy 额度和连登分别读取，未读字段显示未知，不把失败当零。

## 有序组合与兼容计划

`Schedule`、`Request`、`Run` 的可选 `task_sequence` 保存有序的模块 selector，仍由同一个 taskengine 派发，不启动旧 scheduler。组合入口是：

- `growth-sequence`：旅行 → 成长接单/可领取奖励 → 补登 → 连登兑换 → 抽奖 → Buddy 开箱 → 资源读取。每阶段重新发现，不冻结过期任务 ID，不伪造活动进度。
- `checkin-balance-sequence`：`credential-preflight` → 适用区域的签到 → 余额读取。预检在凭据进入 10 分钟窗口时才委托既有加密 journal/CAS/attempt 续期；窗口外只读核对凭据文件。没有安全存储或文件版本不一致时不提交签到。普通 `keepalive` 的显式续期语义不变。

每个账号按阶段顺序执行；每阶段的任务列表、当前位置与确认记录一起持久化。一个账号的 unknown 会保留该账号的原任务、周期和游标，阻断其后续写操作，其他账号继续。重新触发同一序列只在原 runID 核验未完成账号，已完成账号不重做。手动请求的明确账号列表只有与默认计划**此前冻结的完整范围相同**时，才可恢复其 unknown；不会用今天的账号池重新扩大旧范围。

某一步明确失败或 unknown 后，后续独立读取可以继续，但只接受 `Idempotent && !SideEffect` 的任务且只调用 `Inspect`，从不调用 `Execute`/`Claim`、不清除 unknown 或伪造完成证明。例如签到失败后仍读取余额；取消后不再发起该读取。需人工的独立成长任务保留人工状态，不阻塞其他可执行任务。

序列最多 16 个非重叠 selector，每项最多 512 字节，总长最多 4096 字节；账号最多 500，单 run 的累计任务发现上限 1000，失败后的只读尾部每账号最多 64 项。检查点重开时再次校验 provider、账号、阶段、任务与确认记录归属，损坏的语义状态拒绝加载，不清空成新库。

`loomgate-builtin-v2` 仅补齐缺失的两个组合计划，新增写计划默认暂停。已有单项兼容计划的时刻、账号、开关和删除记录保持不变，不静默把旧单项计划扩大成组合。控制台保存/切换/编辑时间均保留 `task_sequence`，手动运行发送确认框里的明确账号范围；目录未读且无明确范围时不提交。

## 目录部分失败

`GET /workbench/catalog` 保留 `tasks`，新增 `issues`（provider/account/module/固定错误码）。任务引擎校验问题项也属于请求的账号范围。面板显示未读取的账号模块，仍展示其他可用任务。

执行全部任务时，可用任务继续执行，整批记录保留 `partial_discovery_failed` 和 `discovery_issues`，不会因为已完成部分工作而显示整批成功。记录跨重启保留。单独执行 `balance` 不会查询 `growth`，从而不会把成长目录故障混成余额失败。

## 当前迁移范围

| 旧功能 | 当前新引擎状态 | 业务约束 |
|---|---|---|
| WorkBuddy 签到 | 已模块化 | 仅 cn；先读状态，执行前再检查，写后服务端复核 |
| Trae 签到 | 已模块化，保持独立协议 | 只接受 trae 账号；真实账号本阶段未调用 |
| WorkBuddy 成长任务领取 | 已模块化 | ID 动态发现，只有服务端 completed 才领，claimed 不重复；未完成任务需真实官方操作 |
| 余额刷新 | 本阶段新增 | cn/global，只查询余额；更新池中积分、到期分桶和原有余额恢复逻辑；不签到、不生成 |
| 旅行派出/记录领奖 | Phase14 已接线 | 地点来自官方配置、业务日来自官方日历；派出后读取名额/在途状态，领奖必须匹配记录且回读结算；不等待整段旅行 |
| 首次领养/协议 | 明确人工步骤 | 不自动代同意协议，不用虚假聊天补门槛 |
| 连登兑换 | Phase14 已接线 | tiers/status 与服务端月份驱动；只接受明确 available，claimed不重领；稳定操作标记，不尝试盲目换参数重发 |
| 补登 | Phase14 已接线 | 官方上线日、当月历史零分日期、已补记录、卡余额完整；一轮最多一张，缺失score不当作零 |
| 成长资源 | Phase14 只读接线 | 能量、抽奖机会、开箱额度与连登展示；不会消耗机会或能量 |
| 补偿领取 | Phase14 已接线 | active/claimed/额度/截止字段驱动；不固定活动日期，以领取标记回读 |
| 账号保活 | Phase16 已接线 | CN/global/Trae官方续期、加密恢复、版本校验、原子保存与回读、会话失效恢复；人工/审核停用不复活 |
| 抽奖/开盲盒 | 已模块化 | 持久化消费尝试、官方奖品回执与机会/额度减少绑定；丢响应只核验，不重抽 |
| 活动/开学季/夜猫实际工作 | 未完整迁入 | 只执行真实官方业务；剩余清单及限制见覆盖矩阵 |
| 原“任务点亮”及伪造活跃报告 | 不照搬伪造行为 | 用户已要求实际工作，需改为真实业务动作或明确人工步骤 |

旧项目国际版的约定是只查余额，不执行签到写操作。早期重构适配器遗漏了该约束，本阶段恢复：global 不发现 checkin，直接强制提交也不会进入签到模块。

## 未确认实例与调度恢复

写请求前除保存原 `uncertain` 标记，还保存不含凭据的原任务定义、周期/记录。活动换期、原任务消失或网关重启后，仍展示原实例供只读核验；同一账号同一任务的新版实例不能绕过旧待复核写入。确认完成后才移除。旧版本已有unknown记录保持兼容，不伪造缺失的历史实例元数据。

Phase15修复F11：动作返回值必须携带明确、绑定本次attempt的接受证据，`nil error`不生成回执。公开 `accepted_stage` 只是有效v2回执的投影；provider目录与旧v1字段均不能声明可信接受。无提交路径不永久保留本次unknown，真实未知不得重放。旅行领奖仍需接受回执和原记录结算。详细兼容与取消规则见[任务动作证据](task-action-evidence.md)。

待复核定义最多1000项，目录合并后仍受总数上限限制。账号锁、全局worker、调度时刻与时区、启用开关和下一次时间继续由现有引擎统一管理。Phase14以实际HTTP/进程重启验证新模块计划的时刻、账号、启停、next_run和不重复派发。

Phase16增加可选TaskGate和v3持久执行游标，保留v2动作回执。间隔等待在worker和单项超时前发生；同一计划多账号不能因本地间隔而丢弃后续工作，重启恢复同一run。详细边界与测试见[多账号恢复](task-work-recovery.md)。生产保活模块需要安全库才能注册，目录本身只读；见[账号保活](account-keepalive.md)。

## 余额读取与兼容

`UserResourceDetailedContext` 为新模块提供可取消查询，保留既有 CN/global 路径和只读 404 兼容回落。缺少 `Response.Data.Accounts` 被视为模式错误，不能把它当成零余额覆盖池状态；合法空数组仍表示没有套餐。

旧 `UserResourceDetailed` 接口继续保留原调用形式，现有代码不需要同步迁移。新组合从网关配置传入原 `expiring_soon` 窗口，调度时间、时区、启停与账号范围继续通过统一计划持久化。

Trae余额与签到现为独立模块：即使签到9074退避或今日已签，仍可更新余额与到期桶。持久状态采用单独安全库，严格业务码/模式检查。详见[Trae任务](trae-tasks.md)。

## 验证与限制

控制台交互统一验收入口（Node 行为测试 + 当前静态资源的 Chromium 回归；API 均为本地合成夹具，不接真实账号）：

```text
python -m pip install -r gateway/tests/requirements-browser.txt
python -m playwright install chromium
python -I -B gateway/scripts/run_console_tests.py --browser --browser-output D:/LoomGate-work/evidence/console-interactions
```

`--browser-output` 必须在源码树之外。手动构建 CI 使用相同入口并保留结果 JSON；新增用例不会被 Go 测试或 Node 字符串断言自动覆盖。该组交互测试不替代真实 Go panel Handler 的端到端验收。

模块测试使用真实 `httptest` 上游及真实任务引擎/账号池，覆盖独立余额读取、部分故障、claim 前复核/已领去重、取消、缺失字段、禁用/跨 provider 拒绝、扩展模块、panic 隔离和 Trae 契约。HTTP 目录与执行记录的部分失败跨重启也有回归。

真实 panel → 进程内 ManagementClient → taskengine Handler 的组合回归：

```text
go test ./internal/panel -run '^TestWorkbenchSequenceUsesRealPanelAndEngineHandlers$' -count=1
```

该测试验证计划保存保留 selector、原 runID、阶段返回前的实时日志以及重启后的读取；模块业务另用真实账号池/加密库配合合成上游测试。Chromium 交互组依然使用本地合成 API，不冒充浏览器已连通全部真实 Handler 或真实账号。历史 `tests/catalog-workbench` 夹具不在当前源码中，不能用其旧命令作为本版本验收入口。第三方 Go 模块必须配合 context；引擎不是不合作代码的进程沙箱。

本次组合与输出验收不调用真实登录、刷新、签到、领取、生成，也不切换部署、不推送或发布。
