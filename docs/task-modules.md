# 统一任务模块

WorkBuddy/Trae 的任务由独立 provider 模块接入统一引擎。Phase14 增加旅行、动态兑换、日历补登、资源状态和补偿领取。**不是全部旧任务迁移完成**，生产仍使用旧发布版本。逐个旧入口及子步骤见[完整覆盖矩阵](legacy-task-coverage.md)。

## 模块契约

`internal/taskproviders.Module` 提供 `Spec`、`Discover`、`Inspect`、`Execute`、`Claim`、`Verify`。`ModuleSpec` 说明本模块负责的任务 key 或前缀、服务区域；注册时拒绝重叠路由。新增业务模块在组合处注册，不需要修改 taskengine 的核心执行/调度代码。

公共任务定义放在 `definitions.json`：名称、说明、阶段与周期语义由该资源加载；动态成长任务的 ID、名称和奖励仍从上游响应取得。它不是允许任意 HTTP 请求的可执行配置文件。provider 业务操作保留在实现了接口的模块中，后续新任务可按同一契约扩展。

`Accounts` 负责账号范围、启用状态、provider/realm 校验和模块路由。每次实际操作重新检查账号；执行单一任务时仅发现对应模块，不因其他模块目录故障阻断。发现模块有上下文期限，panic 和正常失败转换为固定错误码，不暴露上游原始错误。

有完整状态读取的模块实现可选 `CatalogModule.DiscoverState`，同次返回定义和观测值，避免页面为了展示进度重复执行任务。其余模块继续使用原 `Discover`。模块目录最多256项；重复key和越界归属会变成独立发现错误。新增模块不修改核心调度器的任务表。

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
| 保活/活动/开学季/夜猫实际工作/抽奖/开盲盒 | 未完整迁入 | 完整剩余清单及限制见覆盖矩阵 |
| 原“任务点亮”及伪造活跃报告 | 不照搬伪造行为 | 用户已要求实际工作，需改为真实业务动作或明确人工步骤 |

旧项目国际版的约定是只查余额，不执行签到写操作。早期重构适配器遗漏了该约束，本阶段恢复：global 不发现 checkin，直接强制提交也不会进入签到模块。

## 未确认实例与调度恢复

写请求前除保存原 `uncertain` 标记，还保存不含凭据的原任务定义、周期/记录。活动换期、原任务消失或网关重启后，仍展示原实例供只读核验；同一账号同一任务的新版实例不能绕过旧待复核写入。确认完成后才移除。旧版本已有unknown记录保持兼容，不伪造缺失的历史实例元数据。

适配器验证成功的操作回执另存 `accepted_stage`，只能由引擎在调用返回且落盘后设置，provider目录不能自行声明。旅行领奖必须同时满足有效领取回执和原记录结算；只有记录消失而没有回执时继续待复核。回执已接受、结算延迟的情况可在重启后只读确认，不重发领奖。跨业务日且缺少可核验历史的旧操作保留保护。

待复核定义最多1000项，目录合并后仍受总数上限限制。账号锁、全局worker、调度时刻与时区、启用开关和下一次时间继续由现有引擎统一管理。Phase14以实际HTTP/进程重启验证新模块计划的时刻、账号、启停、next_run和不重复派发。

## 余额读取与兼容

`UserResourceDetailedContext` 为新模块提供可取消查询，保留既有 CN/global 路径和只读 404 兼容回落。缺少 `Response.Data.Accounts` 被视为模式错误，不能把它当成零余额覆盖池状态；合法空数组仍表示没有套餐。

旧 `UserResourceDetailed` 接口继续保留原调用形式，现有代码不需要同步迁移。新组合从网关配置传入原 `expiring_soon` 窗口，调度时间、时区、启停与账号范围继续通过统一计划持久化。

## 验证与限制

模块测试使用真实 `httptest` 上游及真实任务引擎/账号池，覆盖独立余额读取、部分故障、claim 前复核/已领去重、取消、缺失字段、禁用/跨 provider 拒绝、扩展模块、panic 隔离和 Trae 契约。HTTP 目录与执行记录的部分失败跨重启也有回归。

浏览器测试模式：

```powershell
go build -o D:/LoomGate-work/build/phase05/catalog-fixture.exe ./tests/catalog-workbench
D:/LoomGate-work/build/phase05/catalog-fixture.exe -root D:/LoomGate-work/private/phase05/catalog-fixture -listen 127.0.0.1:17863 -task-modules
python tests/task_modules_browser.py --runtime D:/LoomGate-work/private/phase05/catalog-fixture --output D:/LoomGate-work/evidence/phase05
```

该模式只写合成账号并截获所有上游请求。测试核对真实池余额、执行 ID、服务端记录、计划保存和窄屏布局；不能替代真实账号迁移或业务验收。新模块需要遵守 context；任意第三方不合作的 Go 实现不会被当作安全沙箱。

本阶段没有真实账号查询/签到/领奖/生成、没有新的短信或二维码请求。豆包实时 SDK、登录、推理及任务奖励和三平台/迁移验收仍按总 checkpoint 继续。

Phase14 新增 `tests/lifecycle_modules_browser.py`，使用 `catalog-workbench -lifecycle-modules`：真实生产模块与真实加密管理服务连接合成HTTP transport，覆盖9项目录/操作/计划/重启/窄屏场景。测试账号与上游全部合成；本轮新增 WorkBuddy 业务的真实账号执行和奖励到账均为 `NOT RUN`。
