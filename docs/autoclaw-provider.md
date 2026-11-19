# AutoClaw provider（纯模型反代）

LoomGate 内置的 AutoClaw 通道。它把 AutoClaw 桌面客户端的模型接口直接实现成
网关里的一个 `inference.Provider`，**不部署任何独立服务、不启动客户端、不创建会话**。

## 为什么它和豆包那条完全不同

AutoClaw 的上游接口本身就是 OpenAI 兼容的：

```
POST https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw/chat/completions
```

一次请求就是「模型 + messages」，没有 `client_meta`、没有 `option`、没有
`conversation_id`、没有 `need_create_conversation`。请求体固定带 `store:false`。
所以「只用模型」在这里是默认行为，不是需要额外破解的东西。

| 项目 | 豆包（已移除） | AutoClaw |
|---|---|---|
| 上游协议 | 客户端会话协议 | OpenAI 兼容 |
| 是否需要客户端 | 曾经需要 | 不需要 |
| 是否创建会话 | 是（协议数据模型的一部分） | 否 |
| 账号凭证 | Cookie + 抓包模板 | accessToken / refreshToken |
| 模型列表 | 动态目录 | 固定 4 个 |

## 模型

| 客户端填写 | 实际上游模型 |
|---|---|
| `autoclaw:glm-5.3-flash` / `autoclaw:glm-5.3` | `zai_glm-5.3-flash` |
| `autoclaw:auto` | `zai_auto` |
| `autoclaw:glm-5.3-coding` | `zaicoding_glm-5.3` |
| `autoclaw:glm-5.3-flashx` / `autoclaw:zai_glm-5.3-flashx-legal` | `zai_glm-5.3-flashx-legal` |

未知模型名兜底到 `zai_glm-5.3-flash`，不会 500。

## 账号导入

凭证存进与 WorkBuddy / Trae 同一个加密 `accountvault`（`provider-accounts/`），
加密主密钥随仓库备份一起迁移。

账号 blob 的字段：

```json
{
  "accessToken": "…",
  "refreshToken": "…",
  "deviceId": "…",
  "phone": "138****8000",
  "expiresAt": 1795012737
}
```

`phone` 只用于控制台展示，不会出现在任何日志或 API 响应里。

## 调度行为

- 按 vault 顺序轮转，`cursor` 保证不会永远打同一个号。
- 只有可能属于账号维度的失败才换号：`401/403/429/402/409/5xx`。
- `400/404/422` 等客户端错误直接返回，换号没有意义。
- 停用的账号不参与轮转（`enabled=false`）。

## 代码位置

| 文件 | 职责 |
|---|---|
| `internal/providers/autoclaw/autoclaw.go` | Provider、请求构造、SSE 解析、凭证刷新 |
| `internal/providers/autoclaw/source.go` | 控制台目录适配（固定模型、脱敏手机号） |
| `cmd/server/workbench.go` | 装配：`inference["autoclaw"]` + `providercatalog.Sources` |

## 边界

- 不代理 AutoClaw 的积分、邀请、任务接口；那些属于账号运营，不在模型反代范围内。
- 不做刷新令牌的自动轮换；令牌过期时返回明确错误，由控制台重新导入。
- 不把上游原文回显给调用方（避免泄露账号维度信息）。
