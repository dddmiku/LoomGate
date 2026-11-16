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
| growth / 任务接单、完成、领取 | growthModule | 已有完成状态与领取；接单/真实工作执行未全迁移，禁止硬编码盲领回落 |
| growth / 补登卡、日历、补登 | makeupModule | 本轮实现，服务端日期/上线日/已补记录判定，一轮最多一张 |
| growth / 连登兑换 | redemptionModule | 本轮实现，响应中的档位、状态和服务端月份驱动 |
| growth / 抽奖机会和抽奖 | resourcesModule | 本轮只读机会；完整抽奖/奖品历史复核未完成，不自动消耗 |
| growth / 能量、Buddy额度、开盲盒 | resourcesModule | 本轮只读余额/额度；开箱与所得Buddy权威复核未完成 |
| growth / 连登、能量展示 | resourcesModule | 本轮真实只读状态接线 |
| keepalive / 刷新、持久化、停用原因 | keepaliveModule | Phase16已接线；Phase17修复F14旧凭据证明污染新attempt，原外部负例、崩溃窗口与保存失败恢复回归通过；真实续期NOT RUN |
| keepalive / 原自动补注册地 | 官方人工核验 | 不按出口IP擅自填账号国家；续期成功不等于注册已完成 |
| school / 动态任务、官方工作、领奖、抽奖 | 待迁移 | 原限时活动/任务/抽奖全部未迁移；不能靠历史日期或伪造事件完成 |
| cat / 夜猫窗口与实际工作 | 动态成长目录可见 | 官方真实工作与窗口执行未迁移 |
| activity / 原活跃上报、连登自检、触发领养 | resourcesModule 只读连登 | 不迁入伪造对话事件；真实官方活动路径待实现 |
| runner / 画布、模板、专家/团队、技能、自动化、playbook、灯塔、微信/QQ、皮肤、聊天/指定模型、夜猫、RichMeow、资料库、订阅、first_buddy等 | 动态growth目录 | 目录/已完成领取已有；逐类真实操作仍待迁移，不照搬light_up/report埋点 |
| compensation / 状态与领取 | compensationModule | 本轮实现，active/claimed/截止时间判定、领取后回读；无硬编码活动日期 |

新模块沿用 taskengine 的账号锁、单次写入前持久化未知状态、取消、复核轮询、时区/时刻/账号范围/开关和重启恢复。只有新运行模式关闭旧调度器与旧手动控制；未切换生产。

Phase14合成HTTP/浏览器绿测后发现F11无请求假回执，Phase15明确动作证据修复已获独立确认。Phase15广泛回归绿测后又独立发现F12每日多账号漏调度和F13坏quota覆盖到期桶，Phase15整体尚未接受。Phase16保留原断言复现红后修复，见[多账号恢复](task-work-recovery.md)及[Trae任务](trae-tasks.md)。新增真实上游执行均 `NOT RUN`，合成行为覆盖不能替代真实任务奖励核验。完整旧任务迁移仍未完成。
