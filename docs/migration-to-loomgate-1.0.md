# 从 workbuddy2api 迁移到 LoomGate 1.0.0

LoomGate 沿用原协议、账号文件、API key 格式和更新签名身份。已有 workbuddy2api 3.16.x 安装先通过旧发布源获取 3.17.x 过渡版，再进入 LoomGate 1.x 更新线。Linux 过渡包必须使用旧加载器的 18 文件契约；Android 过渡 Release 直接附同签名、使用新 versionCode 世代的 LoomGate 1.x APK。构建与验收步骤见 [过渡版构建与升级验收](legacy-transition.md)。

已经安装 v3.17.0 或 v1.0.5、遇到 `new instance exited before reporting ready: EOF` 的运行实例，需要按 [工作台版本的更新与重启](updater-restart.md) 完成首次恢复。旧实例持有工作台排他锁时，不能等待新实例同时打开原目录再报告就绪。

## 原工作区升级与定时继承

常规热更新继续使用原 `data/task-engine.json`，已有计划的启停、时间、账号范围和删除状态优先，不用新版本的内置默认计划覆盖。首次从旧调度进入新引擎时，有明确旧配置或旧账号的工作区按旧 `schedule` 继承可支持任务的开关和小时；原来关闭的仍关闭，原来启用的保持启用。没有旧配置和旧账号的全新工作区使用新安装默认值。没有确切旧对应的新增计划保持自己的默认值，不因为一个旧模块启用而同时打开整套组合计划及其子计划。

早期版本首次导入时曾忽略旧开关与自定义时间，统一使用固定内置计划；历史 `legacy_enabled` 只记录旧开关，不能证明当前暂停是不是用户后来修改的选择。后续版本保留已经保存的计划，不据此批量重开暂停项。

以下离线复制流程用于迁移到一个独立工作区、提前验证副本或保留完整回滚环境。它不是每次正常升级都需要执行的步骤；独立验证副本仍默认停用导入计划，避免启动副本时重复运行生产任务。

## 迁移前

1. 停止旧网关写入（停服务或确认无任务、无在途请求），并对五个持久化目录做完整备份：`config/`、`auths/`、`auths-trash/`、`data/`、`panel-data/`。
2. 确认 `data/api_keys.json.enc-key` 与 `api_keys.json` 一起备份；丢失主密钥会导致 API key 无法解密。
3. 记录旧目录摘要、旧版本、端口、`data/updates/current` 指针和实际运行进程，作为回滚依据。
4. 准备一个不存在的新目录作为目标。不要让它与当前运行工作区重叠。

## 预检

```bash
python -I -X utf8 tools/migration/migrate.py copy \
  --source /path/to/old-workspace \
  --destination /path/to/new-workspace
```

预检只读源目录，不创建目标，报告各类文件数量、清单摘要、加密密钥记录数、旧计划转换结果与排除项。容器配置里的 `/app/...` 路径会按无歧义方式映射为相对路径；外部绝对路径、符号链接、硬链接、普通 socket、缺失主密钥和重叠目录都会拒绝。

## 正式复制

```bash
python -I -X utf8 tools/migration/migrate.py copy \
  --source /path/to/old-workspace \
  --destination /path/to/new-workspace \
  --apply --offline-confirmed
```

- 先写固定 staging 槽，逐文件校验，再原子提交到新目录。
- 目标配置里 `update.enabled` 强制为 `false`，自动更新不会在首次启动时替换文件。
- 旧计划转换为统一计划，全部 `enabled=false`；不会自动派发历史任务。
- 诊断目录（`data/channel-dumps`、`data/logs`、`data/task-logs`）、旧 `data/updates`、socket 与 lock 文件不会复制，并在报告中计数；完整离线备份仍保留它们。

## 新副本只读验证

用新版本二进制在隔离端口和独立目录启动副本，确认：

- `/healthz` 的版本、commit 与所测候选一致；
- 账号能加载、API key 身份与用量可读、计划存在且全部停用；
- unknown/未确认操作仍保留，不被标成成功；
- 不进行签到、领取、生成或登录重放。

## 回滚

```bash
python -I -X utf8 tools/migration/migrate.py rollback \
  --destination /path/to/new-workspace --offline-confirmed
```

回滚把新副本移动到 `.rolled-back` 保留，不覆盖旧源目录。若新目录已产生用户数据，先按保留策略备份；不要用新库反向覆盖旧库。

## 平台版本

- Windows 安装包从 LoomGate 仓库的 Release 获取；同签名覆盖安装，修复/卸载保留 `workspace/`。
- Android 包名 `com.workbuddy2api.app` 和签名不变，LoomGate 1.x 使用 `9_000_000 + minor*1_000 + patch`，高于已经发布的 3.17.0 过渡 APK 的 7,017,000。覆盖安装保留应用数据；不要先卸载。
- Linux 使用完整运行包，不在运行中导入旧库。

## 更新源

默认发布仓库为 `dddmiku/LoomGate`。修复后的过渡版与 LoomGate 1.x 进程，对空更新源或已知 workbuddy2api 旧源，在内存中使用 LoomGate 更新源，不要求写入配置文件。只读配置与文件级挂载同样可用；新候选启动失败时，旧实例配置字节不受影响。控制台更新状态的 `repo` 是当前进程的实际更新源。

第三方更新仓库保持原值。若此前已有有效的 `loomgate-transition.json` 迁移标记，随后用户主动把仓库改回旧源，也保留这一选择。修复不会覆盖它。
