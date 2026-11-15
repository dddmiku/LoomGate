# 豆包流协议与登录调查 — Phase 04

本阶段实现可独立运行和测试的 SSE 解码、补丁状态和异步续流模块。**尚未接入对外推理路由**：官方实时签名、初始消息构造、工具授权与真实完整推理仍需验收，目录可见不代表可调用。

## 可验证实现

`gateway/internal/providers/doubao/stream` 分为两层：

- `events.go`：有界 SSE framing，支持 LF/CRLF/CR、BOM、多行 data，拒绝未完整结束的事件。根据官方事件处理 ACK、完整消息、块补丁、delta 上下文、快照与三种结束类型。文本/代码块提供 append/replace/delete 更新；human ACK 不作为 assistant 文本输出。
- `runner.go`：显式 `Opener`/实时 `Prepare` 契约，严格检查官方主机、POST 路径、HTTP 状态和 SSE MIME。处理主流到 `/chat/async/chunk_stream` 的交接，各流独立序号；断流恢复保留原 key/时间和已确认序号，不重新创建工作。初始请求没有 ACK 时禁止重放。请求、字节、消息/块、异步数量、恢复次数与总时长均有上限。

消息、回答和当前流的终态不同。`end_type=1/2` 不会作为整轮完成；`end_type=3` 仅结束当前流，仍须处理已登记的异步交接。EOF、普通 HTTP 200 JSON、上游错误、未知事件及未支持的原生命令都不会被转换成成功。消费者主动取消不会再请求上游。

原生文件内容保留 `NativeContent` 标记；`DOWNLINK_CMD` 和未支持工具块返回明确人工/未支持错误。没有上传伪造工具结果，也没有把桌面原生工具当成 OpenAI function calling。`ProtocolComplete` 只代表该读取器支持的协议流终态，不代表工作产物质量或任务奖励达标。

## 证据来源

官方客户端安装资源 `extensions/ai-views/static/js/async/55264.js` 提供了主/异步发送器、序号恢复、`writeChunkDelta`、`writeSnapshot` 和 `FETCH_STREAM` 语义；`14780.js` 给出 human=1、AIBOT=2、块与补丁枚举。已有两份真实 Word 工作 SSE 抓包仅在本机受保护目录离线读取，不复制进仓库。

`cmd/doubao-stream-inspect` 是离线检查工具：只输出计数、错误枚举、协议终态及原生内容标记，不输出消息正文、会话/任务 ID 或文件内容，也不联网。两份历史抓包读取均通过；它们包含原生工作内容，因此这项结果不能作为奖励已达标的证据。没有重放文档生成或领奖。

```powershell
go test ./internal/providers/doubao/stream -count=1 -timeout=60s
go run ./cmd/doubao-stream-inspect -file <本机受保护SSE抓包路径>
```

测试采用纯合成消息和真实本机 HTTP server，覆盖 framing、补丁替换/删除、重放、人类 ACK、快照替换与 next_seq、异步续流、同步 ACK 恢复、取消关闭阻塞 body、MIME 拒绝、非幂等禁止盲重试和故障请求隔离。测试不消耗真实账号权益。

## Passport 调查的更正

2026-11-15，从匿名官方网页实际点击登录，观察到：

1. 官方请求主机为 `accounts.doubao.com`。
2. `get_oauth_token` HTTP 200、业务码 1；随后 `get_qrcode` HTTP 200、业务码 0，产生一个二维码。
3. `check_qrconnect` 返回业务码 0、状态 `new`。未扫码、未确认，没有真实登录成功。
4. 请求包含当前 Passport SDK 的版本、来源、设备/语言、实时签名和指纹字段。仅记录安全的字段名与业务码，值与原始响应留在本机私密目录。

这是一次官方 UI 生成的二维码，与上一阶段的直接 Go 初始化不是同一请求。上一阶段没有保存具体拒绝码，因此不能恢复其真实原因；本阶段没有重放该 Go 请求，也没有把所有拒绝推断为风控。

Passport 适配器已改用观察到的认证主机，新增逐请求 SDK `Prepare` 接口；没有该驱动时方法为不可用，不能仅凭 `LOOMGATE_DOUBAO_AUTH_PROFILE` 文件宣称已接通。Host-only 认证 Cookie 不会被扩展成工作服务 Cookie。业务拒绝保留有界数字 `upstream_code`，不输出原始 description/ticket，未知原因统一表达为“官方未接受，原因待核实”。

用户离开期间，本阶段只生成上述一次官方 QR，没有发送短信、提交验证码、重复刷新二维码或改变已登录客户端。待人工状态与一次提示音已记录，调查窗口已关闭。完整 SDK 驱动、真实手机号/扫码成功、子账号/二次验证、会话导入和续期仍未完成。

## 下一步

接入官方运行时提供的实时签名与身份上下文，再绑定初始 completion 构造和对外 API transport。先用最小真实账号流程验收，继续原生工具权限与结果链、动态工作任务奖励、全旧任务迁移和平台构建；不能提前开放未验证的模型调用或宣称项目完成。
