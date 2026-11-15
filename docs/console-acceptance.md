# 新控制台验收

新 UI 位于 `gateway/panel/console/`，以独立 HTML、CSS 和 JavaScript 实现。顶部导航分为连接、自动化和管理；主题、筛选、表格、卡片、状态及弹窗均为新组件。业务数据复用管理 API。

## 开发入口

隔离网关进程设置 `LOOMGATE_REBUILD=1` 和 `WB2API_PANEL_GO=1` 后，`/admin/` 使用新控制台及新登录页。默认运行模式保留旧面板，不能据此认为生产已经迁移。新控制台不依赖 `panel/build.py`，资源由 Go 直接内嵌。

## 本阶段功能边界

| 页面 | 本阶段实现 | 仍待完成 |
|---|---|---|
| 工作台、服务 | provider descriptor 驱动的账号统计与推理能力 | 实际健康与熔断概况 |
| 账号 | 服务动态选择、检索、启停与回读、重新登录入口和接入进度；Phase 03 增加双路径认证界面 | 真实豆包登录成功、续期、删除/回收与全量迁移 |
| 模型目录 | 按服务/账号读取、分页、能力检索、旧缓存/错误/退避、切页取消 | 豆包消息推理接入；目录可见不代表 API 可调用 |
| 任务 | 动态发现、逐项与范围执行入口、具体人工说明 | 全 provider 工作任务与奖励闭环 |
| 计划 | 动态任务与账号选择、多时段/时区/开关、增删改、服务端回读 | 全量旧任务和生产配置迁移 |
| 执行记录 | 阶段详情、取消、90 秒有界同步、未确认提交保护及人工核对入口 | 更完整的历史筛选与服务端分页 |
| 用量、请求记录 | 实际账本累计与每日统计，请求元数据分页 | 密钥/模型维度筛选、明细及日志页的完整运维功能 |
| 设置 | 主题持久化、会话、密码表单、迁移待验状态 | 完整密钥、网络、更新和迁移操作界面 |

异步任务 HTTP 202 仅代表接受。完成状态从任务引擎重新读取。计划保存/删除也会重新读取服务端；任务提交遇到结果未知时，不自动重试。模型页不显示上游图标 URL、会话信息或原始错误。

## 合成环境复现

以下环境没有真实账号。`catalog-workbench` 使用真实面板、目录解析/缓存、任务引擎、用量和请求存储；上游请求由合成 transport 截获。任务适配器仅改变合成计数，不出网上游。

从 `gateway/` 执行，使用仓库 README 规定的 D 盘临时目录和 Go 并发设置：

```powershell
go build -o D:/LoomGate-work/build/catalog-fixture.exe ./tests/catalog-workbench
D:/LoomGate-work/build/catalog-fixture.exe -root D:/LoomGate-work/private/catalog-fixture -listen 127.0.0.1:17863
```

另一终端执行：

```powershell
python tests/console_browser.py --runtime D:/LoomGate-work/private/catalog-fixture --output D:/LoomGate-work/evidence/phase02 --fixture D:/LoomGate-work/build/catalog-fixture.exe
```

浏览器测试从该合成环境的本地文件读取管理凭据，测试中不打印凭据。它会创建、编辑、启停和删除合成计划，执行/取消合成任务，并实际停止和重启该 fixture 进程以核对持久化。不可指向真实账号目录。生成的截图与报告保留在指定输出目录。

报告区分页面异常、预期故障 HTTP 诊断及外网请求。外网请求必须为零。浏览器测试失败后先读取报告的 `failed_stage`；不会盲目重放真实操作。

## 回退边界

新控制台仅由隔离运行开关启用。停止测试进程即可保留所有源代码与测试证据。回到 phase-01 可从 Git 提交恢复独立副本；不要回滚或覆盖旧生产工程。正式迁移、Release 和旧库停更仍须独立验收。
