# 统一任务模块 — Phase 05

本阶段将已经接入的 WorkBuddy/Trae 任务拆成 provider 模块，并补回独立余额刷新。**不是全部旧任务迁移完成**，生产仍使用旧发布版本。

## 模块契约

`internal/taskproviders.Module` 提供 `Spec`、`Discover`、`Inspect`、`Execute`、`Claim`、`Verify`。`ModuleSpec` 说明本模块负责的任务 key 或前缀、服务区域；注册时拒绝重叠路由。新增业务模块在组合处注册，不需要修改 taskengine 的核心执行/调度代码。

公共任务定义放在 `definitions.json`：名称、说明、阶段与周期语义由该资源加载；动态成长任务的 ID、名称和奖励仍从上游响应取得。它不是允许任意 HTTP 请求的可执行配置文件。provider 业务操作保留在实现了接口的模块中，后续新任务可按同一契约扩展。

`Accounts` 负责账号范围、启用状态、provider/realm 校验和模块路由。每次实际操作重新检查账号；执行单一任务时仅发现对应模块，不因其他模块目录故障阻断。发现模块有上下文期限，panic 和正常失败转换为固定错误码，不暴露上游原始错误。

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
| 旅行/领养/保活/活动/连登兑换/补登/抽奖/能量/补偿等 | 尚未完整迁入 | 后续逐个按真实条件、权益成本和回读语义实现 |
| 原“任务点亮”及伪造活跃报告 | 不照搬伪造行为 | 用户已要求实际工作，需改为真实业务动作或明确人工步骤 |

旧项目国际版的约定是只查余额，不执行签到写操作。早期重构适配器遗漏了该约束，本阶段恢复：global 不发现 checkin，直接强制提交也不会进入签到模块。

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
