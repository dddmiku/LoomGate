# LoomGate

多平台 AI 网关与任务工作台。项目从 workbuddy2api v3.16.1 的已发布代码独立导入，保留原协议、数据和更新身份的兼容约束，正在重构服务接入、任务执行和管理界面。

**当前是开发分支，尚未完成全项目验收，不能替换生产版本。** 正式发布、迁移切换和旧项目停更入口将在独立验收后执行。旧仓库与 Release 保留。

## 当前可验证的实现

- 任务引擎具有持久化计划与执行记录、账号互斥、跨账号并发、取消、进程锁、业务周期幂等和中断恢复。发送副作用前记录未确认状态，成功必须以服务端复核为依据。
- 新 provider 会话使用 AES-GCM 加密存储，账号身份、启用状态和修订号一起验证完整性。丢失原主密钥时拒绝生成替代密钥。
- 豆包使用官方客户端对应的接口。模型与活动标识运行时获取，模型缓存按账号和登录修订隔离；失败明确报告，保留时效受限的旧目录，不静默换模型。
- 豆包官方客户端的单轮普通文本已通过一次真实鉴权 API 验证；动态活动目录、任务状态、签到与领取路径已接入统一引擎。普通文本和两轮真实对话已有证据；短信登录和扫码后的已登录会话保存已验证。历史未知签到/领取仍待复核。豆包范围仅为模型反代和签到任务，不建设原生工具或文档平台。
- 新控制台使用独立设计的左侧分组导航，手机为侧边抽屉。动态目录采用能力对照列表，表单标题固定、内容独立滚动。账号与任务等入口接真实后端；计划增删改、重启恢复和取消经过隔离浏览器验收。完整账号登录与运维控制仍在开发。
- 离线迁移工具支持 dry-run、副本校验、中断恢复和回滚。构建脚本保留原发布公钥、Android 包名与签名身份，并检查 NDK/CGO。

任务接口与 UI 的存在不代表所有 provider 流程已完成。具体缺口见 [开发状态](docs/development-status.md)。

## 代码布局

| 路径 | 职责 |
|---|---|
| `gateway/internal/taskengine` | 任务状态、执行、租约、调度与管理接口 |
| `gateway/internal/taskproviders` | provider 业务到任务引擎的适配 |
| `gateway/internal/providers/doubao` | 官方豆包目录、活动与客户端请求 |
| `gateway/internal/providers/doubao/stream` | 官方受管传输的 SSE 补丁、终态、异步交接与有界恢复 |
| `gateway/internal/accountvault` | 新 provider 会话的加密存储与并发写保护 |
| `gateway/internal/authflow` | 加密的认证流程、冷却、取消与中断恢复 |
| `gateway/internal/providercatalog` | provider 能力、账号和目录管理视图；有界并发与失败隔离 |
| `gateway/internal/server` | 已有 Chat / Responses / Messages / Gemini 协议 |
| `gateway/internal/panel`、`gateway/panel` | 管理接口和内嵌前端 |
| `gateway/panel/console` | 独立的新控制台；不依赖旧面板的布局、样式或组件 |
| `tools/doubao-runtime` | 官方桌面 SDK 的受限目录桥接、版本守卫和身份绑定 |
| `tools/migration` | 离线复制迁移、校验、恢复与回滚 |
| `desktop`、`android` | 平台包装与保留签名身份的构建 |

Go 模块路径暂时保留为 `workbuddy2api`，更新资产也保留兼容别名；这些标识不是新产品名称。原历史未导入新仓库，导入基线为 `4b6452e5e0e4c49b45604fd878f6174701bce5eb`。

## 本地验证

需要 Go 1.26.8、Python 3.12 或更新版本、Node.js。Windows 使用 D 盘原生临时路径，并限制并行链接：

```powershell
$env:GOMAXPROCS = '2'
$env:GOFLAGS = '-mod=readonly -p=1'
$env:GOTMPDIR = 'D:\LoomGate-work\tmp'
$env:TEMP = $env:GOTMPDIR
$env:TMP = $env:GOTMPDIR
$env:GOCACHE = 'D:\LoomGate-work\build\go-cache'
New-Item -ItemType Directory -Force $env:GOTMPDIR | Out-Null
Set-Location gateway
python -X utf8 panel/build.py
go test ./... -count=1
go vet ./...
python -X utf8 -m unittest discover -s panel -p 'test_*.py'
```

在仓库根目录验证辅助工具：

```powershell
python -X utf8 -m unittest discover -s tools/migration -p 'test_*.py'
python -X utf8 -m unittest discover -s tools/buildtests -p 'test_*.py'
```

LoomGate 默认使用新工作台、统一任务引擎与 Go 控制台，旧调度器不会并发启动。尚未迁移完成的部署可显式设置 `LOOMGATE_LEGACY_SCHEDULER=1` 暂时回到旧调度器与旧面板；该兼容模式仅允许单独运行。测试运行应使用独立配置和数据目录。

新控制台与隔离验收见 [控制台验收](docs/console-acceptance.md)，豆包协议、已验证边界见 [模型传输](docs/doubao-text-transport.md) 和 [范围矩阵](docs/doubao-scope.md)。任务中的文档、表格等内容只作为官方活动要求处理，不提供独立编辑、产物管理、下载或任意工作编排接口。需要真实内容或官方技能不可用时，展示具体人工操作。

官方手机号/扫码接入状态见 [官方登录](docs/official-login.md)。MFA 和自然续期尚无真实触发证据，遇到时沿官方界面处理；不要求人为触发这些场景才继续其他开发。正式切换仍须账号基本可用、数据与签名迁移安全、独立验收通过。

## 迁移与构建

先阅读 [迁移与回滚](docs/migration.md)。真实数据必须在离线副本演练通过后再安排切换。Android 保留 `com.workbuddy2api.app` 和原签名；不能卸载后重装，也不能改用 CI 新生成的签名证书。

GitHub 的 `Manual build acceptance` 工作流仅手动触发，生成开发验收资产，不生成发布签名，不发布 Release、不部署。资产清单见 `gateway/scripts/release-targets.json`。Windows、Android、Linux 的完整发行包和设备/服务器实测仍须完成。
