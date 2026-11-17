# 旧任务覆盖矩阵

依据只读 v3.16.1 / `4b6452e5e0e4c49b45604fd878f6174701bce5eb` 的 `internal/scheduler/tasks.go`、`scheduler.go`、`travel.go`、`growth_center.go`、`school.go`，以及 `scripts/task_runner.py`、`school_open_day_2026.py`、`compensation_claim.py`。以上文件与旧工作区基线 diff 为空。这里只审代码，不执行旧脚本或真实账号操作。

| 原入口/子步骤 | 新引擎位置 | 当前入口状态与未完成边界 |
|---|---|---|
| checkin / WorkBuddy 国内签到 | checkinModule | 已有检查→签到→复核；余额独立任务 |
| trae_check / Trae 签到、余额、到期桶、9074退避 | traeCheckinModule / traeBalanceModule | Phase15/16已实现独立余额、严格到期快照、加密退避与多账号持久间隔；keepalive已独立接线，旧设备旋转不照搬；真实执行NOT RUN |
| balance / WorkBuddy CN/global | balanceModule | 已有只读余额与池更新；global不做活动写操作 |
| travel / 首次领养、协议、门槛 | travelModule | 本轮显示真实门槛及官方领养步骤；协议与首次领养仍人工 |
| travel / 地点、每日派出、在途状态、记录领奖 | travelModule | 本轮实现，地点/记录来自响应；分别派出与领奖，不阻塞等待数小时 |
| growth / 旅行 | 同 travelModule | 不再重复另一条旅行流水线 |
| growth / 任务接单 | growthModule（`growth/accept/<code>`） | 2026-11-17 实现：目录驱动动态发现、严格 JSON 回执、未知不重放；接单与完成分离 |
| growth / 任务完成、领取 | growthModule（`growth/<code>`） | 只领取官方 completed 或 progress 达标的任务；真实工作仍需在官方客户端完成，不伪造事件 |
| growth / 补登卡、日历、补登 | makeupModule | 本轮实现，服务端日期/上线日/已补记录判定，一轮最多一张 |
| growth / 连登兑换 | redemptionModule | 本轮实现，响应中的档位、状态和服务端月份驱动 |
| growth / 抽奖机会和抽奖 | lotteryModule | 2026-11-17 重做：vault CAS attempt、稳定 client_token、奖品回执 + 机会余额严格减少；无机会为 skipped，不写完成 |
| growth / 能量、Buddy额度、开盲盒 | buddyBoxModule | 2026-11-17 重做：严格 context 写、回执必需、额度严格减少；无能量 skip，不重放 |
| growth / 连登、能量展示 | resourcesModule | 本轮真实只读状态接线 |
| keepalive / 刷新、持久化、停用原因 | keepaliveModule | Phase16已接线；Phase17修复F14旧凭据证明污染新attempt，原外部负例、崩溃窗口与保存失败恢复回归通过；真实续期NOT RUN |
| keepalive / 原自动补注册地 | 官方人工核验 | 不按出口IP擅自填账号国家；续期成功不等于注册已完成 |
| school / 动态任务、官方工作、领奖、抽奖 | 官方活动页（人工）+ growth 目录 | 旧实现为独立脚本伪造事件；按用户“不靠虚假埋点伪造工作”的明确要求，不再迁移伪造上报。当前活动是否存在以官方页面为准，已失效则不显示可执行任务 |
| cat / 夜猫窗口与实际工作 | 官方客户端 | 旧实现为 chat_request_send 伪造事件；不迁移。夜猫类真实对话任务以官方目录与真实完成状态为准 |
| activity / 原活跃上报、连登自检、触发领养 | 不迁移伪造上报 | 旧实现上报 chat_request_send 事件点亮连登，与真实使用无关；按用户范围删除。连登/旅行状态仍由 resources/travel 真实只读 |
| runner / 画布、模板、专家/团队、技能、自动化、playbook、灯塔、微信/QQ、皮肤、聊天/指定模型、夜猫、RichMeow、资料库、订阅、first_buddy等 | growth 目录 + 官方客户端 | 目录、接单、完成状态与领奖已迁移；逐类“点亮”依赖伪造 report/desktop 指纹事件，不再实现；真实工作需在官方客户端完成 |
| compensation / 状态与领取 | compensationModule | 本轮实现，active/claimed/截止时间判定、领取后回读；无硬编码活动日期 |

新模块沿用 taskengine 的账号锁、单次写入前持久化未知状态、取消、复核轮询、时区/时刻/账号范围/开关和重启恢复。只有新运行模式关闭旧调度器与旧手动控制；未切换生产。

Phase14合成HTTP/浏览器绿测后发现F11无请求假回执，Phase15明确动作证据修复已获独立确认。Phase15广泛回归绿测后又独立发现F12每日多账号漏调度和F13坏quota覆盖到期桶，Phase15整体尚未接受。Phase16保留原断言复现红后修复，见[多账号恢复](task-work-recovery.md)及[Trae任务](trae-tasks.md)。新增真实上游执行均 `NOT RUN`，合成行为覆盖不能替代真实任务奖励核验。完整旧任务迁移仍未完成。

2026-11-17 更新：抽奖/盲盒从“只读资源”升级为持久证明的独立单次任务（lottery、buddy_box），接单拆为 `growth/accept/<code>`。伪造上报类入口（school/cat/activity/runner 点亮）按用户拒绝伪造埋点的要求不迁移，只在真实官方完成状态上提供接单/领奖。所有真实奖励仍 NOT RUN。
