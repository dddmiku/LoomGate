# 账号接入与认证流程

Phase 03 将账号接入放入独立状态机，并连接新控制台。**真实豆包登录尚未通过验收**；合成协议测试和浏览器通过不等于真实账号可登录。

## 模块与状态

- `internal/authflow` 管理流程、截止时间、退避、取消、最多 16 条记录和中断恢复。流程秘密使用独立 accountvault 加密，放在管理目录的 `connection-vault`。接口只返回可展示状态；手机号、验证码、Cookie 和令牌不出现在状态列表。
- `internal/panel/connections.go` 保留 WorkBuddy 国内/国际授权和 Trae 官方网页授权。WorkBuddy 使用已有设备授权 API，但不再依赖辅助程序明文输出/临时文件保存流程。Trae 兑换请求限定官方地址和上下文期限，账号身份核验通过后才允许保存。
- `internal/providers/doubao/passport.go` 实现官方 WebInterfaceSdk 对应的二维码与短信路径、字段转换、Cookie 连续性和账号信息复核。`waiting_scan`、`scanned`、`verified`、`succeeded` 分开；只有显式保存已核验账号后才显示接入成功。
- 账号启停使用观测到的 revision 做 CAS；停用不删除会话，后续目录和任务发现读取最新启用状态。旧账号启停沿用原管理契约并回读。

发送短信、提交验证码、授权回调、保存账号前先记录操作。中断或传输结果未知时保留 `uncertain`，禁止自动重放。取消正在进行的外部操作也可能需要核验，不能承诺请求未执行。短信冷却跨取消/新建流程保留；关闭弹窗停止浏览器轮询，服务端等待状态仍可恢复。自动同步限时 90 秒，流程默认最长等待 10 分钟，官方返回过期则立即结束。

## 官方协议证据与实际边界

已安装官方客户端 `async/36086.js` 的 `createPassportSDK`、`sendCode`、`smsLogin` 和 `async/106.js` 的 `WebInterfaceSdk`、模块 `491291` 确认了参数来源和转换方式。WebInterfaceSdk 本次对应：

| 操作 | 官方路径 | 状态依据 |
|---|---|---|
| 短信发送 | `/passport/web/send_code/` | 业务结果及 `retry_time` |
| 短信验证 | `/passport/web/sms_login/` | 业务结果，随后账号信息复核 |
| 获取二维码 | `/passport/web/get_qrcode/` | 响应中的 token 与 QR 地址 |
| 等待确认 | `/passport/web/check_qrconnect/` | new / scanned / confirmed / login / refused / expired |
| 账号复核 | `/passport/account/info/v2/` | 官方账号身份，不以 HTTP 200 代替 |

此前调查文档把标准 WebInterfaceSdk 的短信发送路径写为 `/passport/mobile/send_code/`，本阶段按实际导出对象和路径表修正为上表。数字字段的 XOR/hex 是官方传输混淆，不是安全加密；本地流程仍必须使用 AES-GCM 存储。

`LOOMGATE_DOUBAO_AUTH_PROFILE` 可指定受保护的官方 SDK 上下文文件，字段为 `query`、`headers`、`bot_id`。设备/版本参数必须来自当前官方环境，不能复制示例假值或固化一次抓包的签名。未配置时前端明确标为不可开始；已经登录的账号 Cookie 不用于创建另一条登录流程。

2026-11-15 使用新建匿名官方网页上下文进行了一次真实二维码初始化。官方响应被拒绝，状态保存为 `manual_action`，未得到可展示二维码；没有提交手机号或验证码。独立官方网页登录窗口曾打开供人工核验，并触发一次本机提示音，窗口在有界等待后关闭。**拒绝原因和完整 SDK 初始化条件仍需进一步确认，真实短信发送、扫码确认、会话导入和续期均未验证。**

`cmd/doubao-login-inspect` 用于一次明确的调查步骤，只打印状态和布尔信息，不打印二维码、Cookie 或令牌。已有流程默认只读取；明确指定 `-new` 才会开始另一条流程，避免断流续作时重复发请求。

## 验证方法

从 `gateway/` 使用 README 中的 D 盘临时目录和 Go 并发设置执行：

```powershell
go test ./internal/authflow ./internal/providers/doubao ./internal/accountvault ./internal/providercatalog ./internal/panel -count=1 -timeout=90s
go build -o D:/LoomGate-work/build/phase03/catalog-fixture.exe ./tests/catalog-workbench
```

`catalog-workbench` 仅使用合成账号与截获 transport，无真实上游连接。在独立 `catalog-fixture` 目录启动后：

```powershell
python tests/connections_browser.py --runtime D:/LoomGate-work/private/phase03/catalog-fixture --output D:/LoomGate-work/evidence/phase03 --fixture D:/LoomGate-work/build/phase03/catalog-fixture.exe
```

测试包括短信单次派发/冷却、实际进程重启后继续等待验证码、不泄露流程秘密、账号保存/启停、二维码扫描/确认/过期、人工状态、旧服务入口和认证边界。测试会重启它自己的 fixture，不可指向真实运行目录。

## 剩余工作

真实豆包登录上下文与人工验证还需闭环；错误分型、官方二次验证/子账号、续期及旧导入账号与官方身份的关联需要真实证据。原导入账号名称未按新身份摘要命名时，不会静默覆盖。WorkBuddy/Trae 新入口的真实账号端到端验证亦未执行。账号删除/回收、完整账号迁移以及密钥/网络/日志/更新管理继续后续阶段。
