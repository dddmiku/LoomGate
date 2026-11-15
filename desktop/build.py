#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-11-15：构建强制注入并复核既有发布公钥，保留 GOFLAGS 并限制串行编译。
# 2026-10-13：初版——把网关 + 内嵌面板打成桌面分发包（Windows .exe 目录包 / macOS .app）。
"""构建桌面分发包（Windows / macOS）。

    python desktop/build.py --target windows --arch amd64
    python desktop/build.py --target macos   --arch arm64

产出（默认在 D:/LoomGate-work/build/desktop，可设 LOOMGATE_WORK_ROOT）：

    workbuddy2api-windows-amd64/          ← 整个文件夹拷给用户即可
      workbuddy2api.exe                   ← 双击启动（启动器，非网关本体）
      wb2api.exe                          ← 网关本体
      credit.exe / login.exe / ...        ← 辅助程序
      README.txt                          ← 首次使用说明

    workbuddy2api-macos-arm64/
      WorkBuddy2API.app/                  ← 双击启动
      README.txt

设计取舍：
  * **不内嵌 Python**（用户已确认）。启动器会检测 Python 3 并在缺失时给出
    带下载链接的指引。内嵌运行时会让包体积从 ~15MB 涨到 60-100MB，且要分别
    处理两个平台的运行时嵌入与签名，收益不抵复杂度。
  * **macOS 用 .app 目录而不是单文件**：.app 是只读包体，数据（账号/配置/密钥）
    放在 .app 旁边，所以启动器通过 WB2A_DESKTOP_ROOT 指定数据目录。这样用户
    备份 = 拷贝整个文件夹，换机器直接可用。
  * **Windows 同时产出两种形态**：目录包（透明、能看到数据在哪）与**单文件 exe**
    （用户 2026-10-13 要求「打包成单 exe 就行，简化下操作」）。单文件版把网关与
    辅助程序用 go:embed 嵌进启动器，首次运行解到 <root>/runtime——解压是**幂等**
    的（内容一致就不重写），所以只有首次启动多花几百毫秒。
    注意：单文件版**不是**自解压壳，不会触发杀软对 SFX 的误报模式；它就是一个普通
    的 Go 可执行文件，只是体积里带着自己的运行时。
"""
import argparse
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
from pathlib import Path

BASE = Path(__file__).resolve().parents[1]
GATEWAY = BASE / "gateway"
WORK_ROOT = Path(os.environ.get("LOOMGATE_WORK_ROOT", "D:/LoomGate-work"
                              if os.name == "nt" else str(BASE.parent / "LoomGate-work")))
DIST = WORK_ROOT / "build" / "desktop"
GO = os.environ.get("GO", r"C:\tools\go\bin\go.exe")

# 辅助程序：与运行包同一套（桌面版把它们放在网关旁边，面板的 native 模式按
# WB2API_RUNTIME_DIR 解析它们，见 panel/native_runtime.py）。
HELPERS = ("login", "credit", "signin_bin", "trial_bin", "activity_bin", "fission_bin")

# 脚本：面板的部分任务会调 scripts/*.py（native 模式直接跑 python）。
SCRIPTS = ("global_region.py", "task_common.py", "task_runner.py",
           "school_open_day_2026.py", "growth_center.py")


def version() -> str:
    return (GATEWAY / "VERSION").read_text(encoding="utf-8").strip()


def release_public_key() -> str:
    """只读仓库内既有公钥；缺失或损坏时不能产出跳过更新验签的发行包。"""
    try:
        value = (GATEWAY / "release-signing.pub").read_text(encoding="ascii").strip()
    except (OSError, UnicodeError):
        raise SystemExit("无法读取既有 release-signing.pub，拒绝构建未配置更新验签的发行包。") from None
    if not re.fullmatch(r"[0-9a-fA-F]{64}", value) or set(value) == {"0"}:
        raise SystemExit("release-signing.pub 必须是 32 字节 Ed25519 公钥的十六进制文本。")
    return value.lower()


def verify_go_binary(out: Path, goos: str, goarch: str, public_key: str) -> None:
    """读取 Go 构建元数据验证发布参数，不执行交叉编译产物。"""
    proc = subprocess.run([GO, "version", "-m", str(out)], cwd=GATEWAY,
                          capture_output=True, text=True, timeout=60)
    if proc.returncode != 0:
        raise SystemExit("无法读取构建元数据，拒绝分发：%s" % out.name)
    settings = {}
    for line in proc.stdout.splitlines():
        fields = line.strip().split("\t", 1)
        if len(fields) != 2 or fields[0] != "build" or "=" not in fields[1]:
            continue
        key, value = fields[1].split("=", 1)
        if value.startswith('"'):
            try:
                value = json.loads(value)
            except (ValueError, TypeError):
                raise SystemExit("构建元数据无效：%s" % out.name) from None
        settings[key] = value
    try:
        flags = shlex.split(settings.get("-ldflags", ""))
    except ValueError:
        raise SystemExit("链接元数据无效：%s" % out.name) from None
    wanted = "workbuddy2api/internal/version.ReleaseKey=" + public_key
    if (settings.get("GOOS") != goos or settings.get("GOARCH") != goarch
            or settings.get("CGO_ENABLED") != "0" or wanted not in flags):
        raise SystemExit("产物平台或发布验签公钥不符合构建要求：%s" % out.name)


def build_env(goos: str, goarch: str) -> dict:
    env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0",
               GOTOOLCHAIN="go1.26.8", GOMAXPROCS="2")
    # 交叉编译到 macOS/Windows 时依赖要走本地代理（本机直连 github.com 不稳）。
    env.setdefault("HTTP_PROXY", "http://127.0.0.1:7897")
    env.setdefault("HTTPS_PROXY", "http://127.0.0.1:7897")
    return env


def go_build(pkg: str, out: Path, goos: str, goarch: str, ldflags: str = "-s -w") -> None:
    public_key = release_public_key()
    ldflags += " -X workbuddy2api/internal/version.ReleaseKey=" + public_key
    out.parent.mkdir(parents=True, exist_ok=True)
    # 命令行覆盖同名 GOFLAGS，保留调用方的其它设置，避免并行链接耗尽提交内存。
    cmd = [GO, "build", "-p=1", "-mod=readonly", "-buildvcs=false", "-trimpath",
           "-ldflags", ldflags, "-o", str(out), "./cmd/" + pkg]
    proc = subprocess.run(cmd, cwd=GATEWAY, env=build_env(goos, goarch),
                          capture_output=True, text=True)
    if proc.returncode != 0:
        raise SystemExit("构建 %s 失败：\n%s\n%s" % (pkg, proc.stdout, proc.stderr))
    verify_go_binary(out, goos, goarch, public_key)


def write_readme(target: Path, goos: str, tag: str) -> None:
    start = "双击 workbuddy2api.exe" if goos == "windows" else "双击 WorkBuddy2API.app"

    text = """WorkBuddy2API {tag} — 桌面版
==============================================

一、怎么用
----------
{start}，稍等几秒会自动打开浏览器里的管理面板。
关闭那个窗口（或按 Ctrl+C）即停止服务。

**不需要装 Python。** 面板已经是 Go 原生的，和网关在同一个进程里。

二、想要「一个文件」的话
------------------------
同目录下的 **workbuddy2api-single-amd64.exe** 是单文件版：只有它一个文件，
双击即用，不需要这个文件夹里的其它任何东西（网关与辅助程序都在它里面，
首次运行会自动解出来）。

两种形态功能完全相同，选一个就行：
  · 单文件版 —— 省事，适合直接发给别人
  · 文件夹版 —— 透明，能直接看到每个程序与数据文件

二、首次使用
------------
1. 打开面板后，在「账号」页添加你的账号，在「密钥」页创建调用密钥。
2. 把网关地址（默认 http://127.0.0.1:7863/v1）和密钥填进你的客户端即可。
3. 首次登录管理面板的用户名是 wbadmin，密码在 panel-data/initial-password.txt。
   登录后请立刻在「系统」页改掉。

三、数据在哪
------------
账号、配置、密钥、用量记录都在本文件夹里：

  config/config.json   配置（监听端口、任务时间等）
  auths/               账号凭据（**请勿外发**）
  data/                密钥库、用量账本、运行状态
  panel-data/          面板登录凭据
  auths-trash/         删除的账号（可手动恢复）

备份 = 把整个文件夹拷走；换机器 = 拷过去继续用。
想放到别处（比如 D 盘）？设环境变量 WB2A_DESKTOP_ROOT 指向目标目录。

四、常见问题
------------
* 端口被占用：启动器会自动换一个端口并打印实际地址。
* 想换端口：改 config/config.json 里的 listen，重启即可。
* 杀软报警：本包未做数字签名（签名需付费证书），属正常。可加入信任。
* 面板打不开：确认那个黑窗口还开着 —— 关掉窗口服务就停了。
* 定时任务（签到/旅行等）需要 Python 3：那些任务跑的是 scripts/*.py，
  没装 Python 时它们会被跳过，网关与面板的其它功能不受影响。
  装法：https://www.python.org/downloads/（安装时勾选 Add python.exe to PATH）。

五、安全提醒
------------
本程序默认只监听 127.0.0.1（仅本机可访问）。auths/ 里是你的账号凭据，
把这个文件夹发给别人等于把账号给了对方 —— 分发前请先清空 auths/ 和 data/。
""".format(tag=tag, start=start)
    newline = "\r\n" if goos == "windows" else "\n"
    (target / "README.txt").write_text(text, encoding="utf-8", newline=newline)


# 绝不能进分发包的目录：它们是运行期数据，含账号凭据与调用密钥。
# 打包前若存在，说明有人在构建目录里跑过服务——必须报错而不是打包，
# 否则一个「顺手打个包」的动作就把自己的账号发出去了。
FORBIDDEN = ("auths", "auths-trash", "data", "panel-data", "config", "logs")


def assert_no_runtime_data(target: Path) -> None:
    """分发包里不得出现运行期数据目录。

    这条守卫是被真事故逼出来的：在构建目录里跑过一次服务后直接打包，会把
    auths/（账号凭据）、data/（调用密钥）一并打进分发包。对用户来说那是
    「下载了一个别人的账号」——而且从外面完全看不出来。
    """
    leaked = [name for name in FORBIDDEN if (target / name).exists()]
    if leaked:
        raise SystemExit(
            "拒绝打包：%s 里存在运行期数据目录 %s。\n"
            "这些目录含账号凭据与调用密钥，绝不能进分发包。\n"
            "原目录保持不变；请先备份数据并选择空的构建输出目录。"
            % (target, "、".join(leaked)))


def stage_common(target: Path, goos: str, goarch: str, tag: str) -> None:
    """放网关本体、辅助程序、脚本 —— 两个平台共用。"""
    exe = ".exe" if goos == "windows" else ""

    # 网关本体（带版本元数据，面板「关于」页会显示）
    ldflags = ("-s -w -X workbuddy2api/internal/version.Version=%s "
               "-X workbuddy2api/internal/version.Commit=desktop "
               "-X workbuddy2api/internal/version.BuiltAt=desktop" % tag)
    go_build("server", target / ("wb2api" + exe), goos, goarch, ldflags)

    # 辅助程序
    for name in HELPERS:
        pkg = {"signin_bin": "signin", "trial_bin": "trial",
               "activity_bin": "activity", "fission_bin": "fission"}.get(name, name)
        go_build(pkg, target / (name + exe), goos, goarch)

    # 脚本（面板的脚本类任务用）
    scripts_dir = target / "scripts"
    scripts_dir.mkdir(exist_ok=True)
    for name in SCRIPTS:
        shutil.copyfile(GATEWAY / "scripts" / name, scripts_dir / name)


# 自更新资产名：桌面端的热更新按 GOOS 选资产（见 internal/hotupdate/release.go
# 的 assetName），这里产出的就是那一份——**只有网关本体**，不含启动器与辅助程序。
# 辅助程序不参与热更新：它们随包分发，且各自很少变；把它们也放进更新会显著放大
# 资产体积，而收益接近于零。
RELEASE_ASSET = "wb2api-%s-%s"


def emit_release_asset(target: Path, goos: str, goarch: str) -> None:
    """把网关本体另存一份成自更新资产名，放到外部 DIST 下。"""
    suffix = ".exe" if goos == "windows" else ""
    source = target / ("wb2api" + suffix)
    if not source.is_file():
        return
    asset = DIST / (RELEASE_ASSET % (goos, goarch))
    shutil.copyfile(source, asset)
    print("  发布资产：%s（%.1f MB）" % (asset.name, asset.stat().st_size / (1024 * 1024)))


def build_windows(goarch: str, tag: str) -> Path:
    name = "workbuddy2api-windows-%s" % goarch
    target = DIST / name
    if target.is_symlink() or target.resolve().parent != DIST.resolve():
        raise SystemExit("构建输出目录必须位于指定 DIST 内，不能是重定向目录。")
    if target.exists():
        assert_no_runtime_data(target)
        shutil.rmtree(target)
    target.mkdir(parents=True)

    stage_common(target, "windows", goarch, tag)
    go_build("desktop", target / "workbuddy2api.exe", "windows", goarch,
             "-s -w -X main.version=%s" % tag)
    write_readme(target, "windows", tag)
    assert_no_runtime_data(target)
    emit_release_asset(target, "windows", goarch)
    build_windows_single_exe(target, goarch, tag)
    return target


# 单文件分发：把网关与辅助程序内嵌进启动器，首次运行解到 <root>/runtime。
#
# 内嵌清单必须是**构建期**确定的，因为 go:embed 在编译时读取 cmd/desktop/embedded/。
# 所以这里先把 staging 目录里的二进制复制过去、编出单文件 exe，再把那份临时拷贝清掉
# （源码树里不能留下十几 MB 的二进制）。
EMBED_DIR = GATEWAY / "cmd" / "desktop" / "embedded"


def build_windows_single_exe(staging: Path, goarch: str, tag: str) -> None:
    """产出 workbuddy2api-single.exe（一个文件，双击即用）。"""
    exe = ".exe"
    payload = {
        "wb2api" + exe: staging / ("wb2api" + exe),
        "login" + exe: staging / ("login" + exe),
        "credit" + exe: staging / ("credit" + exe),
        "signin_bin" + exe: staging / ("signin_bin" + exe),
        "trial_bin" + exe: staging / ("trial_bin" + exe),
        "activity_bin" + exe: staging / ("activity_bin" + exe),
        "fission_bin" + exe: staging / ("fission_bin" + exe),
    }
    missing = [name for name, path in payload.items() if not path.is_file()]
    if missing:
        raise SystemExit("单文件打包失败：缺少 %s（先跑一次完整构建）" % "、".join(missing))

    EMBED_DIR.mkdir(parents=True, exist_ok=True)
    try:
        for name, path in payload.items():
            shutil.copyfile(path, EMBED_DIR / name)
        # 脚本也一起内嵌：面板的脚本类任务要跑它们，而单文件分发时用户手上没有
        # scripts/ 目录。启动器把它们解到 <root>/runtime/scripts/。
        scripts_dir = EMBED_DIR / "scripts"
        scripts_dir.mkdir(exist_ok=True)
        for name in SCRIPTS:
            shutil.copyfile(GATEWAY / "scripts" / name, scripts_dir / name)
        go_build("desktop", DIST / ("workbuddy2api-single-%s%s" % (goarch, exe)),
                 "windows", goarch, "-s -w -X main.version=%s" % tag)
    finally:
        # 无论成败都要清掉：这些是构建中间产物，绝不能进源码树（十几 MB 二进制
        # 会让 git status 和编辑器都很难受）。
        for entry in EMBED_DIR.iterdir():
            if entry.name.startswith("."):
                continue  # 保留 .PLACEHOLDER：go:embed 不接受空目录
            if entry.is_dir():
                shutil.rmtree(entry)
            else:
                entry.unlink()

    single = DIST / ("workbuddy2api-single-%s%s" % (goarch, exe))
    size = single.stat().st_size / (1024 * 1024)
    print("  单文件版：%s（%.1f MB，双击即用）" % (single.name, size))


def build_macos(goarch: str, tag: str) -> Path:
    name = "workbuddy2api-macos-%s" % goarch
    target = DIST / name
    if target.is_symlink() or target.resolve().parent != DIST.resolve():
        raise SystemExit("构建输出目录必须位于指定 DIST 内，不能是重定向目录。")
    if target.exists():
        assert_no_runtime_data(target)
        shutil.rmtree(target)
    target.mkdir(parents=True)

    app = target / "WorkBuddy2API.app"
    macos_dir = app / "Contents" / "MacOS"
    resources = app / "Contents" / "Resources"
    macos_dir.mkdir(parents=True)
    resources.mkdir(parents=True)

    # 可执行文件与辅助程序都放进 Contents/MacOS（启动器按自身目录找网关）。
    stage_common(macos_dir, "darwin", goarch, tag)
    go_build("desktop", macos_dir / "WorkBuddy2API", "darwin", goarch,
             "-s -w -X main.version=%s" % tag)

    # Info.plist：macOS 靠它识别这是一个可双击的应用。
    (app / "Contents" / "Info.plist").write_text("""<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>WorkBuddy2API</string>
  <key>CFBundleDisplayName</key><string>WorkBuddy2API</string>
  <key>CFBundleIdentifier</key><string>com.dddmiku.workbuddy2api</string>
  <key>CFBundleVersion</key><string>%s</string>
  <key>CFBundleShortVersionString</key><string>%s</string>
  <key>CFBundleExecutable</key><string>WorkBuddy2API</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
""" % (tag.lstrip("v"), tag.lstrip("v")), encoding="utf-8")

    # 数据目录放在 .app **外面**（.app 是只读包体，写进去会失败）。
    # 启动脚本负责设置 WB2A_DESKTOP_ROOT 并拉起真正的可执行文件。
    launcher = target / "启动 WorkBuddy2API.command"
    launcher.write_text("""#!/bin/bash
# 数据放在 .app 同级目录，而不是包体内（包体只读）。
cd "$(dirname "$0")"
export WB2A_DESKTOP_ROOT="$(pwd)"
exec "$(pwd)/WorkBuddy2API.app/Contents/MacOS/WorkBuddy2API"
""", encoding="utf-8", newline="\n")
    launcher.chmod(0o755)

    write_readme(target, "darwin", tag)
    return target


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--target", choices=("windows", "macos", "all"), default="all")
    parser.add_argument("--arch", choices=("amd64", "arm64"), default=None,
                        help="省略时：windows→amd64、macos→arm64")
    parser.add_argument("--tag", default=None, help="版本号，默认读 gateway/VERSION")
    args = parser.parse_args()

    if not Path(GO).exists():
        raise SystemExit("找不到 Go 工具链：%s（可用 GO 环境变量指定）" % GO)

    tag = args.tag or version()
    if not re.fullmatch(r"v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)", tag):
        raise SystemExit("发行版本必须为 vX.Y.Z，不能用开发或无效版本冒充发行包。")
    release_public_key()  # 在清理旧构建目录之前先完成发布前置校验。
    DIST.mkdir(parents=True, exist_ok=True)
    print("构建 WorkBuddy2API 桌面版 %s" % tag)

    built = []
    if args.target in ("windows", "all"):
        arch = args.arch or "amd64"
        print("\n[1/2] Windows/%s …" % arch)
        built.append(build_windows(arch, tag))
    if args.target in ("macos", "all"):
        arch = args.arch or "arm64"
        print("\n[2/2] macOS/%s …" % arch)
        built.append(build_macos(arch, tag))

    print("\n完成：")
    for path in built:
        size = sum(f.stat().st_size for f in path.rglob("*") if f.is_file())
        print("  %s  (%.1f MB)" % (path, size / 1024 / 1024))
    print("\n提示：分发前请确认包内没有账号数据（auths/ 与 data/ 应为空）。")


if __name__ == "__main__":
    main()
