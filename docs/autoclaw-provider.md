# AutoClaw provider（纯模型反代）

AutoClaw 作为现有网关中的 `inference.Provider` 运行，不部署独立服务、不启动桌面客户端，也不创建上游会话对象。请求携带模型和完整 `messages`；工具目录、助手工具调用和工具结果保留结构，思考与正文分开传递。

## 请求与模型

上游使用 chat/completions 形态的 HTTPS POST。该服务并非所有 OpenAI 参数都兼容：请求使用既有客户端身份说明，始终请求 `stream:true`；客户端非流式请求由网关本地聚合。`store`、输出预算和其他不兼容通用参数不发送，不能据此宣称上游一定不存储内容。

| 客户端填写 | 实际上游模型 |
|---|---|
| `autoclaw:glm-5.3-flash` / `autoclaw:glm-5.3` | `zai_glm-5.3-flash` |
| `autoclaw:auto` | `zai_auto` |
| `autoclaw:glm-5.3-coding` | `zaicoding_glm-5.3` |
| `autoclaw:glm-5.3-flashx` / `autoclaw:zai_glm-5.3-flashx-legal` | `zai_glm-5.3-flashx-legal` |

目录由当前 provider 的四项固定模型定义提供，不探测其他模型。现有未识别别名按 `within()` 的兼容规则解析；可见模型和调用权限仍由网关 API key 白名单限定。

## 账号与安全续期

- 凭证存入加密 `accountvault`。迁移必须保留原主密钥和记录，不能只复制密文文件。
- 控制台可以登录或导入凭证。登录时支持可选邀请码：填入则使用用户的值，留空才采用现有默认值；不会靠反复登录验证。
- 账号列表只显示脱敏手机号。新账号标识由 vault 域内 HMAC 派生；旧含手机号的物理 ID 在公开目录和请求记录中投影为不透明别名，不改写旧记录或账号修订。
- 停用账号不参与推理。账号会话在实际发送前再次校验；不会因刷新而重新启用人工停用账号。
- 只有明确 token 过期响应才进入既有单次续期路径。发送前以 CAS 保存 `refresh_pending`；成功后保存新凭据对，响应丢失或保存冲突保留待核验，不重放单次 refresh token。权限/额度失败不能拿来消耗 refresh token。

## 流式契约

`internal/providers/autoclaw/stream.go` 解析实际数据流，流式和本地聚合共用同一路径：

1. 每条 `data:` 行中的 JSON 立即解析，空分隔行和 SSE 元数据不算模型输出。上游当前协议是一条数据行对应一个 JSON chunk。
2. `[DONE]` 是明确终态，遇到后停止读取和输出，不继续处理其后的数据。没有 `[DONE]` 时，只在明确 `finish_reason` 后正常 EOF 才可完成。
3. 普通 EOF、传输中断或只有思考但没有终态都不是成功。合法的无换行尾帧仍会解析；无终态的部分正文、思考和用量保留，但不发成功结束帧，也不自动重新生成。
4. JSON 损坏、显式错误包、非法形态、冲突的结束原因、输出结束后的新增内容、非法/冲突工具索引等返回稳定协议错误，不能静默跳过。
5. 思考不拼入正文；已结束的纯思考或纯工具轮次不是空回答。明确 `content_filter` 可以产生空正文并正常返回过滤终态，不重复请求来回避过滤。
6. `finish_reason` 保留上游的 `stop`、`length`、`content_filter` 或 `tool_calls`；已有工具调用也不能把明确的 `length` 改写成 `tool_calls`。
7. 消费者回调失败立即停止，保留取消传播和单次发送计数；已交付的输出不与另一请求的生成拼接。

资源边界：单行最多 1 MiB，累计 wire 数据最多 64 MiB，正文/思考/工具数据合计最多 8 MiB，工具调用最多 64 个，工具标识和函数名最多 256 字节。超限明确失败，不截断成成功回答。

## 用量、错误与请求记录

用量按上游显式报告的字段逐项保留：缺失不补零，缓存 `0` 是已知值；分块或重复的累计快照不相加。非法负数、类型、溢出或缓存大于输入的值不能覆盖此前有效观测。错误包本身带有的合法用量也保留。

每次实际模型 HTTP 调用独立计为 attempt。401 后进行一次合法续期再调用模型会保留两次模型调用，而本地待核验保护的拒绝不能伪造模型调用。中途失败也保留已知账号别名、部分用量、错误状态，内部总账和请求明细一致；日志不保存正文、思考、原始上游错误或凭据。

对外固定错误码：

| 错误码 | 含义 |
|---|---|
| `autoclaw_stream_incomplete` | 未见明确终态或传输中断，结果不完整 |
| `autoclaw_stream_invalid` | 数据帧、工具归属或终态不满足协议 |
| `autoclaw_stream_limit` | 超出已声明的流/工具数据边界 |

这些错误不自动重试或切换账号。流已开始时用 SSE 错误帧结束，不能同时发 `[DONE]` 或成功 `finish_reason`；未开始时返回失败 HTTP 响应。

## 本地验收入口

```text
go test ./internal/providers/autoclaw -count=1 -timeout=60s
go test ./internal/server -run '^TestAutoClawStreamContractReachesPublicResponseAndAccounting$' -count=3 -timeout=60s
```

测试使用真实 provider、vault、网关 Handler、用量账本和请求明细；HTTP 在注入的合成 transport 处终止，不连接上游。覆盖流式/非流式、部分 EOF、坏帧、尾行、过滤空响应、工具截断原因、分块用量、取消与有界输入。通过这些测试不等于执行过真实模型、登录或平台发布；本次修复不重放真实成功/unknown 操作，不部署或推送。
