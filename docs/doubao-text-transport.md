# 豆包客户端文本传输

LoomGate 使用已登录的官方豆包客户端构造、签名并发出请求。凭据留在官方进程；网关只通过本机受管进程管道接收有界 SSE 数据。此阶段支持实时目录中的普通对话模型，通过现有鉴权的 `POST /v1/chat/completions` 调用。

## 已接入的范围

- 读取当前官方服务的 bot 与账号专属模型目录，模型标识、模式和推理档位来自当前响应。目录刷新、过期、账号修订与失败退避沿用目录服务。
- API 密钥模型白名单逐字匹配完整模型 ID；模型前缀校验支持新的 provider 命名空间。现有请求速率、并发、请求体上限和请求审计仍经过同一入口。
- 当前文本请求需要恰好一条 `user` 文本消息，最多 65536 字节。文件、工具、多轮历史、系统消息及其他未支持参数会在发送前明确拒绝。
- `stream=false` 返回完整 Chat Completion；`stream=true` 接收同一官方流，但暂存可被替换/删除的内容，等整轮结束后输出最终 SSE。它尚不提供逐字即时下发。
- `SSE_ACK` 要与本次消息和本地会话相符；`end_type=3` 才是整轮终态。无 ACK、结果未知或身份变化不会自动重发初始请求。
- 断流恢复需要已观察到的 ACK/游标；异步续流需要解析器授予实际收到的 task key 和 scene。所有尝试共享总字节预算，永久性错误阻止恢复。
- 真实验证未观察到用量。接口返回 `X-Gateway-Usage-Status: not-reported`，账本与尝试记录保留未知计量，不补造 token 数。

## 模块边界

| 模块 | 职责 |
|---|---|
| `internal/inference` | provider 模型与完成结果契约 |
| `internal/providers/doubao/text_provider.go` | 安全账号绑定、请求前后修订核验、内容块拼接 |
| `internal/providers/doubao/text_runtime.go` | 单账号占用、子进程管道、输出预算、取消与清理 |
| `internal/providers/doubao/stream` | 事件、append/replace/delete、ACK 归属、游标恢复与整轮终态 |
| `tools/doubao-runtime/stream_runtime.js` | 官方 renderer 服务生命周期、请求构造、动态目录、受管 fetch |
| `tools/doubao-runtime/stream_bridge.py` | 定长管道帧、签名请求归属核验、父进程消失时取消 |
| `internal/server/provider_chat.go` | Chat API 返回、错误脱敏、用量与尝试审计 |

客户端修改签名或公开 bundle 契约后，适配器可能返回版本/协议错误。不得回落到历史抓包 Cookie，也不得把“目录可见”解释为全部模型和工具都可调用。

## 开发与恢复

在隔离开发运行时设置 `LOOMGATE_REBUILD=1`，以及 `LOOMGATE_DOUBAO_DESKTOP_PYTHON`、`LOOMGATE_DOUBAO_DESKTOP_HELPER`、`LOOMGATE_DOUBAO_DESKTOP_PORT`。端口只允许回环地址，当前账号需要已有加密的 runtime binding。这是当前开发接入方式，正式客户端连接、双登录和跨平台分发仍需完成。

普通生产路径不使用输入框点击。诊断 `PrepareCompletion` 已使用带随机归属标记的独立请求，拦截后等待 fail ACK，再取消自有 fetch。它不会修改编辑器；诊断结果只投影 schema/hash。清理未确认时返回 `desktop_cleanup_uncertain`，不报告成功或 `network_sent=false`。

网关退出、取消或管道中断会通知受管子进程取消 reader/fetch。进程失联由 renderer 的期限看门狗兜底。任何不确定清理都要求先核对状态；不得把超时当作未提交而直接重发。

## 验证边界

2026-11-16 的一次已授权普通文本验证通过：本地鉴权 API、一个上游签名请求、ACK、完整终态、回复长度/hash匹配。原始请求、响应和准确上游 request ID 仅存本机私密证据目录；公开源码仅使用合成 fixture。

之后加入的 ACK 归属、退出清理和审计增强通过合成回归，并离线重放同一真实响应；没有为此再发送第二条真实请求。实测与后续离线/合成验证应分别表述。

官方手机号/扫码/MFA/续期、完整工作工具、每日/一次性任务执行与奖励、全量多轮协议、真实异步工具运行、三平台完整包与生产迁移仍待完成。此文件不构成正式发布或完整项目验收。
