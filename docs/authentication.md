# 账号接入与认证流程

Phase 03 将账号接入放入独立状态机，并连接新控制台。**真实豆包登录尚未通过验收**；合成协议测试和浏览器通过不等于真实账号可登录。

## Phase12：连接已登录的官方客户端

配置本机官方客户端连接后，在“账号 → 豆包工作 → 连接当前官方账号”开始接入。先核验官方当前身份和实时签名目录，再由用户点击“保存已核验账号”。保存前重复核验身份，以账号修订号做 CAS；切号、并发停用、重复身份或账号修改不会被覆盖。已有同身份别名会复用。

新增 `DesktopConnection` 负责绑定和连接状态；`CommandRuntime.Connection` 通过现有受保护管道取得身份与动态 bot/目录证明；`authflow.Verifier` 只允许显式只读恢复。`GET /admin/api/workbench/client` 受既有管理鉴权保护，返回连接状态和各账号是否匹配，不返回身份证明或官方 Cookie。

断连流程可在服务重启后“重新核验官方账号”。核验受冷却、状态和输入边界约束，不能发送验证码、创建二维码或保存账号。账号保存与流程写入之间中断时，绑定中加密保存的操作标记可识别已经完成的写入；恢复不会重复新增账号或递增修订号。取消尚在执行的操作保留待复核，驱动返回前不能并发开启恢复。

2026-11-16 实测：新面板完成一次官方已登录账号绑定，目录读到 12 个模型；网关重启后绑定和流程状态保留，官方连接关闭后显示断连。上述只证明已有官方会话的绑定，**不代表短信/扫码登录、MFA、续期或三平台免调试端口接入已完成**。目前仍依赖人工配置的本机官方客户端连接。

合成验证入口为 `tests/catalog-workbench -desktop-connection` 与 `tests/desktop_connection_browser.py`，覆盖断线恢复、真实进程重启、身份切换、并发停用、窄屏状态及零认证/任务副作用。真实账号库与合成 fixture 必须使用不同目录。

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

`LOOMGATE_DOUBAO_AUTH_PROFILE` 可指定受保护的官方 SDK 上下文文件，字段为 `query`、`headers`、`bot_id`。设备/版本参数必须来自当前官方环境，不能复制示例假值或固化一次抓包的签名。Phase 04 官方 UI 证实还需要实时签名与完整 Passport 上下文；生产 `Prepare` 驱动尚未接入，单独配置此文件不会开放登录。已经登录的账号 Cookie 不用于创建另一条登录流程。

2026-11-15 使用新建匿名官方网页上下文进行了一次真实二维码初始化。官方响应被拒绝，状态保存为 `manual_action`，未得到可展示二维码；没有提交手机号或验证码。独立官方网页登录窗口曾打开供人工核验，并触发一次本机提示音，窗口在有界等待后关闭。**拒绝原因和完整 SDK 初始化条件仍需进一步确认，真实短信发送、扫码确认、会话导入和续期均未验证。**

`cmd/doubao-login-inspect` 用于一次明确的调查步骤，只打印状态和布尔信息，不打印二维码、Cookie 或令牌。已有流程默认只读取；明确指定 `-new` 才会开始另一条流程，避免断流续作时重复发请求。

Phase 04 进一步观察到官方 UI 在 `accounts.doubao.com` 成功生成一次 QR，轮询为 `new`，仍未扫码登录；旧 Go 拒绝的具体码未保留，不能断定原因。驱动现保留数字业务码并区分未知拒绝。详细差异和当前能力见 [豆包流协议与登录调查](doubao-stream.md)。

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
