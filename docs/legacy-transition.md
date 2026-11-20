# workbuddy2api 老用户的过渡升级

旧版 3.16.x 不能直接消费所有 LoomGate 1.x 运行包：旧版本比较会把 1.x 判为降级，旧 Linux 解包器也拒绝额外的豆包运行组件。过渡发布使用 3.17.x tag，让旧加载器先取到兼容包；里面运行修复后的 LoomGate 引擎。已经安装过渡版的实例，还需处理旧发送端等待新进程就绪时的排他锁问题，步骤见 [更新与首次恢复](updater-restart.md)。

## 过渡发布的资产契约

| 资产 | 要求 |
| --- | --- |
| `wb2api-runtime-linux-amd64/arm64/armv7.tar.gz` | `manifest.version` 与过渡 Release tag 一致，恰好 18 个旧加载器运行文件，不带 `doubao-runtime/` 整组 |
| `wb2api-windows-amd64` | 对应 Windows 构建的网关本体，供旧启动器读取 `current` 指针 |
| `workbuddy2api-android-arm64.apk` | 原样复用已构建、同签名的 LoomGate 1.x APK；versionCode 使用 9,000,000 世代，不把 APK 改名为 3.17.x 版本 |
| `SHA256SUMS.txt` 和 `.sig` | 保持既有文件名与 Ed25519 公钥身份，发布前必须验签；过渡构建工具不读取私钥 |

Android Release tag 与 APK 自身 versionName 可以不同。旧安装器交接的是已验签文件路径；Android 原地升级以实际 APK 包名、签名和 versionCode 为准。过去发布的 3.17.0 APK versionCode 是 7,017,000，旧 LoomGate 1.0.5 APK 是 5,000,005；直接回到后者会被系统拒绝。新版 1.x 使用更高世代后再覆盖安装，不能通过卸载绕过。

## 构建候选资产

先提交并完成源代码验收，再按正常 Android 构建流程生成当前 1.x 同签名 APK。运行下面的过渡构建工具；tag 和目录应替换为本次候选值：

```powershell
python -I -B tools/build_transition.py `
  --tag v3.17.1 `
  --android-apk D:/LoomGate-work/build/android/workbuddy2api-android-arm64.apk `
  --work-root D:/LoomGate-work/build/transition-candidate
```

工具要求干净源码，复用 `gateway/scripts/build_acceptance.py` 构建 Linux 与 Windows，校验构建报告摘要，并检查 Android 实际 manifest、既有签名及版本与当前 `gateway/VERSION` 一致。必须提供完整重新构建的 APK，不得拿仅调整安装序号的验收包代替。它生成候选资产、SHA-256 清单和构建报告，**不签名、不发布、不上传，也不部署**。Linux 交叉构建的身份验证会如实标记等待目标平台运行，不能把它当成实机通过。

发布前使用既有发布签名身份在受控步骤签署 `SHA256SUMS.txt`，再用公开验证密钥回读验证每项摘要。旧维护仓库的过渡 Release 提供上述资产，LoomGate 仓库提供后续正式 1.x 资产；不要覆盖原有 tag，也不要生成替代签名身份。实际发布属于单独动作。

## 使用真实旧加载器验收

仓库中的 probe 从本机旧 Git checkout **只读取得固定基线** `4b6452e5e0e4c49b45604fd878f6174701bce5eb` 的更新器源码；只复制到指定外部目录编译，不修改旧 checkout，也不把旧源码收入新仓库：

```powershell
python -I -B gateway/tests/legacy_loader_probe.py `
  --assets D:/LoomGate-work/build/transition-v3.17.0/assets `
  --work-root D:/LoomGate-work/evidence/legacy-upgrade/windows `
  --go C:/path/to/go.exe
```

默认旧源码位置是用户目录下 `Desktop/na/workbuddy2api-git-update/gateway`，可用 `--legacy-source` 指定别处。probe 在回环伪 GitHub API 上执行实际旧版查询、资产选择、Ed25519 验签、下载摘要和 Linux 旧解包器，并执行下载件的只读 build-info 检查；篡改清单必须被拒绝。它不接触账号、不会发起官方登录或任务。

Linux 可在目标机器直接执行，也可以用 `--target-os linux --target-arch amd64 --compile-only` 先编译后转移到隔离目录运行。执行时提供 `LOOMGATE_BRIDGE_ASSETS`、`LOOMGATE_BRIDGE_PUBLIC_KEY` 与独立 `TMPDIR`。跨平台编译本身不算运行通过。

完整部署链路还需在隔离工作区携带合成配置、账号、API key、用量、计划及未确认任务记录，验证旧实例退出、新实例健康、实际更新源和版本，并核对这些状态没有丢失。只读配置不应成为迁移前提；新候选启动前不改写旧配置或迁移标记。

## 本轮验证边界

真实 v3.16.1 加载器已在 Windows/amd64 和独立 Linux/amd64 环境消费既有签名 v3.17.0 资产，完成查询、验签、下载、旧格式提取及实际 build-info 执行；篡改清单被拒绝。新过渡包格式与 Android APK 接入有针对性回归。这些结果不表示已经发布新的过渡 Release、修改生产目录或重启用户实例。
