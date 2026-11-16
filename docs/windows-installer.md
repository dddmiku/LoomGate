# Windows 安装与验收

Phase17新增完整Windows amd64开发安装包，尚未正式发布。它由真实网关、桌面启动器、login/credit/fission辅助程序、现有发布验签公钥与文件摘要清单组成。使用当前用户安装，不要求管理员权限，不创建系统服务，不改代理或证书。

安装目录包含 `program/` 和首次启动创建的 `workspace/`。程序、卸载器和快捷方式属于安装器；账号/配置/API密钥/任务状态/日志属于workspace。修复安装及卸载不注册删除workspace，不搜索或自动迁入旧账号。原有 `WB2A_DESKTOP_ROOT` 显式路径仍支持，安装版缺省使用program旁的workspace，兼容便携版的旧启动器不改变默认行为。

安装版启动器明确启用新引擎与新控制台，旧调度器关闭。无需Python启动管理台。空账号的推理readiness仍为503，但启动器核对服务身份和管理登录页可用后打开管理台，不将“没账号”当作进程启动失败。Windows通过父子进程stdin控制通道走现有优雅关停，未增加网络停机接口。`LOOMGATE_DESKTOP_HEADLESS=1`仅抑制自动打开浏览器，供本地自动化通过stdin发送stop并验证退出。

## 构建

```powershell
$env:GO='C:/Users/dddmiku/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.26.8.windows-amd64/bin/go.exe'
$env:GOTMPDIR='D:/LoomGate-work/tmp'
$env:TEMP=$env:GOTMPDIR
$env:TMP=$env:GOTMPDIR
python desktop/build_installer.py --output D:/LoomGate-work/build/new-installer-slot --compiler D:/LoomGate-work/tools/inno/compiler/ISCC.exe --label dev-acceptance
```

输出目录必须在源码树之外且为空；文件清单是显式allowlist，任何额外文件或摘要不符阻止打包。版本是dev标签，不修改VERSION/tag/Release。原Ed25519公钥不替换；实际网关和启动器运行 `--loomgate-build-info` 后立即退出，只输出版本、平台与公钥SHA256用于比对，既不读取账号也不启动网络。Go裁剪构建不总返回ldflags，因此不将缺少那一项元数据误当作验证成功；仍检查实际产物平台和原公钥指纹。

此安装包未做Authenticode产品代码签名，也未生成发布签名。不能把安装编译器的有效签名当成LoomGate安装包已签名。正式发布前仍需独立验收和原信任链发布流程。更新默认关闭，已有更新资产兼容名称和原公钥保持。

## 本机实测

`desktop/verify_installer.py`只允许新建D盘隔离验收目录，使用空账号真实安装器和浏览器，验证：安装文件摘要、无运行数据打包、管理台登录、桌面和390px布局、stdin正常退出、修复安装逐文件保留配置与状态、再次启动、卸载后全部工作区文件保留。所有安装/修复/退出/卸载进程的退出码为0。

这些结论来自实际Windows安装包，不是交叉编译或buildguard模拟。生产账号迁移、从正式旧版升级验证、Authenticode发布签名、Android实际安装与DNS、Linux完整包运行仍未完成。完整原任务继续。
