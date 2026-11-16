# 豆包客户端文本传输

LoomGate 使用已登录的官方豆包客户端构造、签名并发出请求。凭据留在官方进程；网关只通过本机受管进程管道接收有界 SSE 数据。此阶段支持实时目录中的普通对话模型，通过现有鉴权的 `POST /v1/chat/completions` 调用。

## 已接入的范围

- 读取当前官方服务的 bot 与账号专属模型目录，模型标识、模式和推理档位来自当前响应。目录刷新、过期、账号修订与失败退避沿用目录服务。
- API 密钥模型白名单逐字匹配完整模型 ID；模型前缀校验支持新的 provider 命名空间。现有请求速率、并发、请求体上限和请求审计仍经过同一入口。
- 当前文本请求需要恰好一条 `user` 文本消息，最多 65536 字节。文件、工具、多轮历史、系统消息及其他未支持参数会在发送前明确拒绝。
- `stream=false` 返回完整 Chat Completion；`stream=true` 在收到本次 ACK 并验证完整上游事件后立即下发文本增量，每段刷新到调用方，不等待整轮结束。
- 完整消息替换在事件内原子应用，避免先删除再添加造成假撤回；重复游标不重复发送。上游若真正撤回已经下发的文本，返回明确的流内错误 `provider_stream_revised`，不发成功 `stop`/`[DONE]`，调用方必须丢弃不完整回答。非流式仍能返回最终修订后的文本。
- 即时输出以 `X-Gateway-Streaming: incremental` 标识。完成状态和请求摘要在 HTTP trailers 中返回；只有 ACK、整轮终态、账号/身份复核及清理均完成后才发送成功终态。开始时不预先宣称完成。
- `SSE_ACK` 要与本次消息和本地会话相符；`end_type=3` 才是整轮终态。无 ACK、结果未知或身份变化不会自动重发初始请求。
- 断流恢复需要已观察到的 ACK/游标；异步续流需要解析器授予实际收到的 task key 和 scene。所有尝试共享总字节预算，永久性错误阻止恢复。
- 真实验证未观察到用量。接口返回 `X-Gateway-Usage-Status: not-reported`，账本与尝试记录保留未知计量，不补造 token 数。

## 模块边界

| 模块 | 职责 |
|---|---|
| `internal/inference` | provider 模型与完成结果契约 |
| `internal/providers/doubao/text_provider.go` | 安全账号绑定、每段输出前检查账号修订、完成后身份复核 |
| `internal/providers/doubao/text_progress.go` | ACK 前抑制输出、文本前缀增量和撤回检测 |
| `internal/providers/doubao/text_runtime.go` | 单账号占用、子进程管道、输出预算、取消与清理 |
| `internal/providers/doubao/stream` | 事件、append/replace/delete、ACK 归属、游标恢复与整轮终态 |
| `tools/doubao-runtime/stream_runtime.js` | 官方 renderer 服务生命周期、请求构造、动态目录、受管 fetch |
| `tools/doubao-runtime/stream_bridge.py` | 定长管道帧、签名请求归属核验、父进程消失时取消 |
| `internal/server/provider_chat.go` / `provider_stream.go` | Chat API、即时刷新、错误终态、写入背压和审计 |

客户端修改签名或公开 bundle 契约后，适配器可能返回版本/协议错误。不得回落到历史抓包 Cookie，也不得把“目录可见”解释为全部模型和工具都可调用。

## 开发与恢复

在隔离开发运行时设置 `LOOMGATE_REBUILD=1`，以及 `LOOMGATE_DOUBAO_DESKTOP_PYTHON`、`LOOMGATE_DOUBAO_DESKTOP_HELPER`、`LOOMGATE_DOUBAO_DESKTOP_PORT`。端口只允许回环地址，当前账号需要已有加密的 runtime binding。这是当前开发接入方式，正式客户端连接、双登录和跨平台分发仍需完成。

普通生产路径不使用输入框点击。诊断 `PrepareCompletion` 已使用带随机归属标记的独立请求，拦截后等待 fail ACK，再取消自有 fetch。它不会修改编辑器；诊断结果只投影 schema/hash。清理未确认时返回 `desktop_cleanup_uncertain`，不报告成功或 `network_sent=false`。

网关退出、取消或管道中断会通知受管子进程取消 reader/fetch。进程失联由 renderer 的期限看门狗兜底。任何不确定清理都要求先核对状态；不得把超时当作未提交而直接重发。

消息构造依赖官方消息服务延迟加载的模块。初始化必须先等待官方 `getMessageService()`，确认 factory 和导出存在后再访问构造器，防止冷启动时缓存空模块。每次从客户端取回流分片前也检查当前身份；切号时停止并清空自有队列。

## 验证边界

2026-11-16 的一次已授权普通文本验证通过：本地鉴权 API、一个上游签名请求、ACK、完整终态、回复长度/hash匹配。原始请求、响应和准确上游 request ID 仅存本机私密证据目录；公开源码仅使用合成 fixture。

之后加入的 ACK 归属、退出清理和审计增强通过合成回归，并离线重放同一真实响应；没有为此再发送第二条真实请求。实测与后续离线/合成验证应分别表述。

Phase13 在新的隔离副本验证一次即时生成：15个非空文本分片，首段约2.7秒、成功结束约8.4秒；实际 ACK 与 end_type1/2/3 均观察到，离线最终文本哈希与调用方拼接结果相同，未知用量没有补造。首次本地调用遇到冷启动构造错误，审计和抓包均确认0次上游提交后才进行修复后的验证；历史成功文本和任务操作未重放。复制账号绑定直接复用，未重新保存。

合成测试使用真实 HTTP、子进程管道和停止门验证首段早于完成、重写/畸形/异常终止、客户端断开、写出失败、名额释放和审计。既有 provider 的缓冲路径保留；其他协议、工具与多轮不因即时文本通过而宣称可用。

官方手机号/扫码/MFA/续期、完整工作工具、每日/一次性任务执行与奖励、全量多轮协议、真实异步工具运行、三平台完整包与生产迁移仍待完成。此文件不构成正式发布或完整项目验收。
