#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-11-15：NDK/CGO/DNS、发布公钥、既有签名和版本号全部失败即中止，保留 GOFLAGS。
# 2026-10-13：初版——把网关 + Go 面板 + 辅助程序打成安卓 APK。
"""构建安卓 APK。

    python android/build.py                 # 完整构建
    python android/build.py --skip-go       # 只重打 APK（Go 二进制已在 jniLibs 里）
    python android/build.py --debug         # debug 变体

产出（默认在 D:/LoomGate-work/build/android，可设 LOOMGATE_WORK_ROOT）：

    LoomGate-android-arm64.apk

═══════════════════════════════════════════════════════════════════════════
为什么要走 jniLibs（而不是 assets / 私有目录）
═══════════════════════════════════════════════════════════════════════════
安卓的 App 私有目录挂载参数是 **noexec**——任何放进去的文件都无法执行，chmod +x
也没用。这是系统级限制，不是权限问题。

唯一可执行的位置是 APK 里的 `lib/<abi>/lib*.so`：安装时系统会把它们解到
`nativeLibraryDir`，那个目录是可执行的。于是本脚本把每个 Go 二进制改名成
`libwb2<name>.so` 放进 jniLibs：

    wb2api      → libwb2gateway.so
    login       → libwb2login.so
    credit      → libwb2credit.so
    fission_bin → libwb2fission_bin.so

命名映射必须与 internal/android/helpers.go 的 LibName() 一致——那里是运行时读的
一侧，这里是打包写入的一侧，两边分叉的表现是「按钮点了报『缺少辅助程序』」。

═══════════════════════════════════════════════════════════════════════════
为什么必须用 CGO_ENABLED=1
═══════════════════════════════════════════════════════════════════════════
纯 Go 的解析器在安卓上**不做 DNS**（Go 源码 net/conf.go 明确写着 "DNS requests
don't work on Android, so prefer the cgo resolver"）。纯 Go 构建的 net.Resolver
会去读 /etc/resolv.conf，而安卓上那个文件不存在，于是退到 127.0.0.1:53——
手机上没有本地 DNS，全部域名解析失败，网关一条请求也发不出去。

所以安卓构建要开 CGO + 用 NDK 的 clang 交叉编译，让 net 包走 libc 的
getaddrinfo（bionic 会转给 netd，这是安卓上唯一正确的解析路径）。

构建时需要：
  - ANDROID_NDK_HOME 或 <SDK>/ndk/<version> 存在；
  - 用 NDK 里的 `aarch64-linux-android<api>-clang` 作为 CC。
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

HERE = Path(__file__).resolve().parent
REPO = HERE.parent
GATEWAY = REPO / "gateway"
WORK_ROOT = Path(os.environ.get("LOOMGATE_WORK_ROOT", "D:/LoomGate-work"
                              if os.name == "nt" else str(REPO.parent / "LoomGate-work")))
JNILIBS_ROOT = WORK_ROOT / "build" / "android-jni"
GRADLE_OUTPUT = WORK_ROOT / "build" / "android-gradle"
DIST = WORK_ROOT / "build" / "android"

# 打进 APK 的 Go 二进制。**左边的名字（打包后的文件名）**必须与
# internal/android/helpers.go 的 HelperNames 以及 AppPaths.gatewayBinary() 对得上；
# 右边的包路径与 desktop/build.py 的映射表同源（cmd/fission 打出来叫 fission_bin）。
GATEWAY_BINARY = "wb2api"
GATEWAY_PACKAGE = "./cmd/server"
# 安卓端的热更新资产就是 **APK 本身**（见 internal/hotupdate/release.go 的
# assetName：android 分支返回 "LoomGate-android-<arch>.apk"），而 dist/ 下
# 打出来的那个 APK 正好同名，所以不需要另外再导出一份。
#
# 这里**不能**再导出一份裸二进制：应用私有目录不能执行（SELinux），裸二进制在手机
# 上装不起来；真发出去会让更新流程下载到一个用不了的文件。
HELPER_PACKAGES = (
    ("login", "./cmd/login"),
    ("credit", "./cmd/credit"),
    ("fission_bin", "./cmd/fission"),
)

# jniLibs 里的文件名前缀，与 internal/android/helpers.go 的 HelperLibPrefix 一致。
LIB_PREFIX = "libwb2"

# 目标 ABI 与最低 API。21 是 Go android/arm64 的下限；这里取 24，与 build.gradle.kts
# 的 minSdk 一致（前台服务与通知渠道的行为在 24 之前差异太大）。
#
# 默认只出 arm64-v8a：手机上的网关要跑整套 Go 服务，32 位设备内存不够，而 x86_64
# 只在模拟器上有意义。带 --abi x86_64 时额外出一份模拟器包——用于在真机之外做
# 端到端验证（MuMu 这类模拟器默认是 x86_64 宿主，跑 arm64 要靠二进制翻译，
# 装得慢、日志里也容易混进翻译层的噪声）。
TARGETS = {
    "arm64-v8a": ("arm64", "aarch64-linux-android%d", "arm64"),
    "x86_64": ("amd64", "x86_64-linux-android%d", "x86_64"),
}
DEFAULT_ABI = "arm64-v8a"
ANDROID_API = 24
# v3.16.1 既有安装链路的最高已发行 code；重构期间不能低于它。
LEGACY_VERSION_CODE = 3_016_001
# 已发行过渡 APK v3.17.0 使用 7_017_000。旧 3.x 与预发布 0.x 保持原编码，
# 新产品线提高到独立代际，确保安装过桥接版的设备仍能覆盖安装 LoomGate。
# Gradle 必须镜像同一分段映射，避免直接构建和 Python 构建产出不同安装序号。
LEGACY_GENERATION = 4_000_000
LOOMGATE_GENERATION = 8_000_000
ANDROID_VERSION_CODE_MAX = 2_100_000_000
APK_PACKAGE = "com.workbuddy2api.app"
# 来自既有发布 APK 的公开证书，仅用于校验，不包含私钥。
LEGACY_SIGNER_SHA256 = "44c18e220f1f6e505c2e702ff2a832cedb0e9be89e549d7874c136c0fb6fafef"


def find_go():
    """找一个 Go 工具链。PATH 里没有时试 C:/tools/go/bin（本机约定）。"""
    explicit = os.environ.get("GO")
    if explicit:
        path = shutil.which(explicit) or (explicit if Path(explicit).is_file() else None)
        if not path:
            raise SystemExit("GO 指定的工具链不存在。")
        return path
    for candidate in ("go", str(Path("C:/tools/go/bin/go.exe")), "/usr/local/go/bin/go"):
        path = shutil.which(candidate) or (candidate if Path(candidate).is_file() else None)
        if path:
            return path
    raise SystemExit("找不到 Go 工具链。请把 go 放进 PATH，或用 GO 环境变量指定。")


def find_ndk():
    """定位 NDK。纯 Go 构建在安卓上解析不了 DNS，必须用 NDK 的 clang 走 bionic。"""
    for key in ("ANDROID_NDK_HOME", "ANDROID_NDK_ROOT", "NDK_HOME"):
        value = os.environ.get(key)
        if value:
            if not Path(value).is_dir():
                raise SystemExit("%s 指定的 NDK 目录不存在。" % key)
            return Path(value)
    sdk = os.environ.get("ANDROID_HOME") or os.environ.get("ANDROID_SDK_ROOT")
    if not sdk:
        default = Path.home() / "AppData" / "Local" / "Android" / "Sdk"
        if default.is_dir():
            sdk = str(default)
    if not sdk:
        return None
    ndk_root = Path(sdk) / "ndk"
    if not ndk_root.is_dir():
        return None
    versions = sorted((p for p in ndk_root.iterdir() if p.is_dir()
                       and re.fullmatch(r"\d+(?:\.\d+)*", p.name)),
                      key=lambda p: tuple(int(n) for n in p.name.split(".")))
    return versions[-1] if versions else None


def find_clang(ndk, abi):
    """找到 NDK 里对应架构的 clang。不同 NDK 版本的目录布局略有差别，逐个试。"""
    if ndk is None:
        return None
    _, clang_template, _ = TARGETS[abi]
    target = clang_template % ANDROID_API
    host = {"win32": "windows-x86_64", "linux": "linux-x86_64",
            "darwin": "darwin-x86_64"}.get(sys.platform)
    if host is None:
        raise SystemExit("当前主机平台不支持 Android NDK 交叉编译。")
    for relative in ("toolchains/llvm/prebuilt/%s/bin" % host,):
        directory = ndk / relative
        if not directory.is_dir():
            continue
        for name in ("%s-clang.cmd" % target, "%s-clang" % target, "%s-clang.bat" % target):
            candidate = directory / name
            if candidate.is_file():
                return candidate
    return None


def release_public_key():
    """只接受仓库内既有 Ed25519 公钥；不读私钥，也不生成替代信任根。"""
    try:
        value = (GATEWAY / "release-signing.pub").read_text(encoding="ascii").strip()
    except (OSError, UnicodeError):
        raise SystemExit("无法读取既有 release-signing.pub，拒绝构建未配置更新验签的 APK。") from None
    if not re.fullmatch(r"[0-9a-fA-F]{64}", value) or set(value) == {"0"}:
        raise SystemExit("release-signing.pub 必须是 32 字节 Ed25519 公钥的十六进制文本。")
    return value.lower()


def require_ndk_tools(clang):
    if clang is None or not Path(clang).is_file():
        raise SystemExit("Android 构建必须使用 NDK clang 与 CGO；缺少 NDK 时禁止产出 DNS 失效包。")
    binary = "llvm-readelf.exe" if sys.platform == "win32" else "llvm-readelf"
    readelf = Path(clang).parent / binary
    if not readelf.is_file():
        raise SystemExit("NDK 缺少 llvm-readelf，无法验证 Android 链接器与 bionic DNS。")
    return readelf


def reject_netgo(flags):
    """netgo 会强制纯 Go DNS，即使 CGO_ENABLED=1 也不能允许。"""
    try:
        values = shlex.split(flags)
    except ValueError:
        raise SystemExit("GOFLAGS 格式无效。") from None
    for index, value in enumerate(values):
        tags = value.removeprefix("-tags=") if value.startswith("-tags=") else ""
        if value == "-tags" and index + 1 < len(values):
            tags = values[index + 1]
        if "netgo" in re.split(r"[\s,]+", tags):
            raise SystemExit("Android 禁止 netgo 构建标签：必须使用 bionic DNS。")


def go_environment(clang, abi):
    require_ndk_tools(clang)
    env = dict(os.environ)
    reject_netgo(env.get("GOFLAGS", ""))
    env.update(GOOS="android", GOARCH=TARGETS[abi][0], CGO_ENABLED="1",
               GOTOOLCHAIN="go1.26.8", GOMAXPROCS="2", CC=str(clang))
    # 不覆盖 GOFLAGS；-p=1/-mod=readonly/-trimpath 在命令行显式约束。
    env["CXX"] = str(clang).replace("-clang", "-clang++")
    return env


def verify_go_binary(go, output, clang, abi, tag, public_key):
    """只读元数据和 ELF 动态符号；不在主机执行 Android 产物。"""
    readelf = require_ndk_tools(clang)
    result = subprocess.run([str(go), "version", "-m", str(output)],
                            capture_output=True, text=True, timeout=60)
    if result.returncode != 0:
        raise SystemExit("无法读取 Go 构建元数据：%s" % output.name)
    settings = {}
    for line in result.stdout.splitlines():
        fields = line.strip().split("\t", 1)
        if len(fields) != 2 or fields[0] != "build" or "=" not in fields[1]:
            continue
        key, value = fields[1].split("=", 1)
        if value.startswith('"'):
            try:
                value = json.loads(value)
            except (ValueError, TypeError):
                raise SystemExit("Go 构建元数据无效：%s" % output.name) from None
        settings[key] = value
    reject_netgo("-tags=" + shlex.quote(settings.get("-tags", "")))
    if (settings.get("GOOS") != "android" or settings.get("GOARCH") != TARGETS[abi][0]
            or settings.get("CGO_ENABLED") != "1"):
        raise SystemExit("Go 产物的 ABI 或 CGO 不匹配：%s" % output.name)
    # Go 1.26 no longer records -ldflags in build metadata. The gateway links
    # internal/version, so the injected values must be present in its bytes;
    # helper binaries never reference that package and the linker correctly
    # drops unused -X targets, so they are exempt from the literal check.
    if output.name == "libwb2gateway.so":
        blob = output.read_bytes()
        if (public_key.encode("ascii") not in blob or tag.encode("ascii") not in blob):
            raise SystemExit("网关产物缺少注入的版本号或发布公钥：%s" % output.name)
    elf = subprocess.run([str(readelf), "--program-headers", "--dynamic", "--dyn-syms", str(output)],
                         capture_output=True, text=True, timeout=60)
    if (elf.returncode != 0 or "/system/bin/linker64" not in elf.stdout
            or not re.search(r"\bUND\b[^\r\n]*\bgetaddrinfo(?:@[^\s]+)?\b", elf.stdout)
            or not re.search(r"\(NEEDED\)[^\r\n]*\[libc\.so\]", elf.stdout)):
        raise SystemExit("产物缺少 Android 动态链接器或 bionic DNS 依赖：%s" % output.name)


def verify_jnilibs(go, clang, abi, tag):
    public_key = release_public_key()
    for name, _ in [(GATEWAY_BINARY, GATEWAY_PACKAGE)] + list(HELPER_PACKAGES):
        output = JNILIBS_ROOT / abi / ("%s%s.so" % (LIB_PREFIX, "gateway" if name == GATEWAY_BINARY else name))
        if not output.is_file():
            raise SystemExit("缺少 Android Go 产物：%s" % output.name)
        verify_go_binary(go, output, clang, abi, tag, public_key)


def run(command, **kwargs):
    print("  $ %s" % " ".join(str(part) for part in command))
    result = subprocess.run([str(part) for part in command], **kwargs)
    if result.returncode != 0:
        raise SystemExit("命令失败（退出码 %d）：%s" % (result.returncode, command[0]))


def build_go_binaries(go, clang, version, abi):
    """交叉编译网关与辅助程序到 jniLibs/<abi>。"""
    version_code(version)
    public_key = release_public_key()
    env = go_environment(clang, abi)  # 前置失败时保留既有产物。
    jnilibs = JNILIBS_ROOT / abi
    if jnilibs.is_symlink() or jnilibs.resolve().parent != JNILIBS_ROOT.resolve():
        raise SystemExit("jniLibs 输出目录不能跳出指定构建目录。")
    if jnilibs.is_dir():
        shutil.rmtree(jnilibs)
    jnilibs.mkdir(parents=True, exist_ok=True)

    # 版本号必须注入 **internal/version**，而不是 main.version：网关的 /healthz
    # 读的是前者。写错的表现是面板与 /healthz 都报 version="dev"——看起来像
    # 没注入，其实是注入到了一个没人读的变量上（2026-10-13 真机实测发现）。
    # 与 desktop/build.py 的 ldflags 保持同一组变量名。
    ldflags = ("-s -w -X workbuddy2api/internal/version.Version=%s "
               "-X workbuddy2api/internal/version.Commit=android "
               "-X workbuddy2api/internal/version.BuiltAt=android "
               "-X workbuddy2api/internal/version.ReleaseKey=%s" % (version, public_key))
    targets = [(GATEWAY_BINARY, GATEWAY_PACKAGE)] + list(HELPER_PACKAGES)

    for name, package in targets:
        output = jnilibs / ("%s%s.so" % (LIB_PREFIX, "gateway" if name == GATEWAY_BINARY else name))
        run([go, "build", "-p=1", "-mod=readonly", "-trimpath", "-buildvcs=false",
             "-ldflags", ldflags, "-o", str(output), package], cwd=GATEWAY, env=env)
        verify_go_binary(go, output, clang, abi, version, public_key)
        size = output.stat().st_size / (1024 * 1024)
        print("    %-40s %6.1f MB" % (output.name, size))


def read_version():
    version_file = GATEWAY / "VERSION"
    if version_file.is_file():
        return version_file.read_text(encoding="utf-8").strip()
    raise SystemExit("缺少 gateway/VERSION，不能回落到开发版本构建安装包。")


def version_code(tag):
    """把 vX.Y.Z 折成一个单调递增的整数，供安卓的 versionCode 用。

    安卓只认 versionCode 的大小关系，而本项目的版本号是语义化的 tag。
    新产品线：LOOMGATE_GENERATION + major*1_000_000 + minor*1_000 + patch。
    旧 3.x 和预发布 0.x 保持 LEGACY_GENERATION，不改变已发行桥接版的编码。
    因此 1.0.0 → 9_000_000，高于已安装桥接版 3.17.0 的 7_017_000；
    3.x 的合法 minor/patch 全部低于 8_000_000，后续桥接版也不会再次越过 1.x。

    拒绝非法版本、低于旧安装链路上限的版本及分段溢出，避免不可覆盖升级。
    """
    match = re.fullmatch(r"v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)", tag)
    if not match:
        raise SystemExit("Android 发行版本必须为 vX.Y.Z，不能回落 versionCode。")
    major, minor, patch = (int(part) for part in match.groups())
    generation = LEGACY_GENERATION if major in (0, 3) else LOOMGATE_GENERATION
    code = generation + major * 1_000_000 + minor * 1_000 + patch
    if minor >= 1000 or patch >= 1000 or not LEGACY_VERSION_CODE < code <= ANDROID_VERSION_CODE_MAX:
        raise SystemExit("Android versionCode 超出合法范围或并非高于既有 v3.16.1 安装版本。")
    return code


def verify_apk_signature(apk, sdk):
    """apksigner 只读取 APK 的公开证书，禁止用新签名覆盖既有安装链路。"""
    tools = Path(sdk) / "build-tools" if sdk is not None else None
    candidates = [] if tools is None or not tools.is_dir() else sorted(
        (p for p in tools.iterdir() if p.is_dir() and re.fullmatch(r"\d+(?:\.\d+)*", p.name)),
        key=lambda p: tuple(int(n) for n in p.name.split(".")), reverse=True)
    name = "apksigner.bat" if sys.platform == "win32" else "apksigner"
    signer = next((p / name for p in candidates if (p / name).is_file()), None)
    if signer is None:
        raise SystemExit("缺少 apksigner，无法验证既有 APK 签名身份。")
    result = subprocess.run([str(signer), "verify", "--print-certs", str(apk)],
                            capture_output=True, text=True, timeout=60)
    fingerprints = re.findall(r"^Signer #\d+ certificate SHA-256 digest:\s*([0-9a-fA-F]{64})\s*$",
                              result.stdout, re.MULTILINE)
    if result.returncode != 0 or [v.lower() for v in fingerprints] != [LEGACY_SIGNER_SHA256]:
        raise SystemExit("APK 签名不匹配既有安装链路，拒绝输出分发包。")


def read_apk_manifest(apk, sdk):
    """读取实际 APK 的安装身份，不根据文件名或发布 tag 猜测安装版本。"""
    tools = Path(sdk) / "build-tools" if sdk is not None else None
    candidates = [] if tools is None or not tools.is_dir() else sorted(
        (p for p in tools.iterdir() if p.is_dir() and re.fullmatch(r"\d+(?:\.\d+)*", p.name)),
        key=lambda p: tuple(int(n) for n in p.name.split(".")), reverse=True)
    name = "aapt2.exe" if sys.platform == "win32" else "aapt2"
    aapt = next((p / name for p in candidates if (p / name).is_file()), None)
    if aapt is None:
        raise SystemExit("缺少 aapt2，无法验证 APK 安装版本。")
    result = subprocess.run([str(aapt), "dump", "badging", str(apk)],
                            capture_output=True, text=True, timeout=60)
    match = re.search(r"^package: name='([^']+)' versionCode='([0-9]+)' versionName='([^']+)'",
                      result.stdout, re.MULTILINE)
    if result.returncode != 0 or match is None:
        raise SystemExit("无法读取 APK 的包名与安装版本，拒绝分发。")
    return {"package": match[1], "version_code": int(match[2]), "version_name": match[3]}


def verify_apk_manifest(apk, sdk, tag):
    expected = {"package": APK_PACKAGE, "version_code": version_code(tag),
                "version_name": tag.removeprefix("v")}
    if read_apk_manifest(apk, sdk) != expected:
        raise SystemExit("APK 实际安装版本与声明不符，拒绝输出分发包。")


def build_apk(gradle, variant, abi, tag, go=None, clang=None):
    """调 Gradle 打 APK。"""
    if gradle is None:
        raise SystemExit(
            "找不到 Gradle。安装方式：\n"
            "  1. 装 Android Studio（自带 Gradle）；或\n"
            "  2. 下载 gradle-7.5 并把 bin/gradle 放进 PATH；或\n"
            "  3. 用 Gradle wrapper（本目录没有 wrapper，可先跑一次 `gradle wrapper`）。")
    task = "assembleDebug" if variant == "debug" else "assembleRelease"
    code = version_code(tag)
    if code < version_code(read_version()):
        raise SystemExit("请求的 Android versionCode 低于当前源码版本，拒绝降级构建。")
    verify_jnilibs(go or find_go(), clang or find_clang(find_ndk(), abi), abi, tag)
    # 只打包本次构建的 ABI：jniLibs 下可能同时留着上一次为别的架构编的 .so
    # （--skip-go 复用产物时尤其容易），不限定就会把两份都塞进同一个 APK。
    # 只在第一次跑时需要联网拉 AGP；之后 Gradle 缓存能离线工作。
    run([gradle, task, "--no-daemon", "-p", str(HERE),
         "--project-cache-dir", str(WORK_ROOT / "build" / "android-gradle-cache"),
         "-PabiFilter=" + abi, "-PjniLibsDir=" + str(JNILIBS_ROOT.resolve()),
         "-PbuildOutputDir=" + str(GRADLE_OUTPUT.resolve()),
         "-PversionCode=%d" % code, "-PversionName=" + tag.lstrip("v")])
    built = GRADLE_OUTPUT / "outputs" / "apk" / variant
    apks = sorted(built.glob("*.apk")) if built.is_dir() else []
    if len(apks) != 1:
        raise SystemExit("Gradle 输出必须包含且只包含本次构建的一个 APK，请检查构建输出。")
    verify_apk_signature(apks[0], find_sdk())
    verify_apk_manifest(apks[0], find_sdk(), tag)
    DIST.mkdir(parents=True, exist_ok=True)
    target = DIST / ("LoomGate-android-%s.apk" % TARGETS[abi][2])
    shutil.copy2(apks[0], target)
    return target


# Gradle 版本上限：AGP 7.4.2 不认识 Gradle 8.x 的若干 API（实测在 8.13 上直接报
# Unresolved reference）。所以本机缓存里同时有 7.5 与 8.13 时，**优先选 7.5**，
# 而不是「取最新」。要升 AGP 时这个常量与 build.gradle.kts 里的版本一起改。
GRADLE_PREFERRED = "7.5"


def find_gradle():
    for candidate in ("gradle", "gradle.bat"):
        path = shutil.which(candidate)
        if path:
            return path
    # 本机约定：Gradle 发行版在 ~/.gradle/wrapper/dists 下。
    dists = Path.home() / ".gradle" / "wrapper" / "dists"
    if not dists.is_dir():
        return None
    candidates = []
    for version in sorted(dists.iterdir()):
        for inner in list(version.glob("*/gradle-*/bin/gradle.bat")) +                 list(version.glob("*/gradle-*/bin/gradle")):
            candidates.append((version.name, str(inner)))
    for name, path in candidates:
        if name.startswith("gradle-%s-" % GRADLE_PREFERRED):
            return path
    return candidates[0][1] if candidates else None


def find_sdk():
    """定位 Android SDK（Gradle 需要它来编译与打包）。"""
    for key in ("ANDROID_HOME", "ANDROID_SDK_ROOT"):
        value = os.environ.get(key)
        if value and Path(value).is_dir():
            return Path(value)
    default = Path.home() / "AppData" / "Local" / "Android" / "Sdk"
    if default.is_dir():
        return default
    return None


def write_local_properties(sdk):
    """把 SDK 路径写进 local.properties。

    不依赖用户事先配 ANDROID_HOME：本机 SDK 的默认位置是已知的，直接写进
    local.properties（Gradle 的标准做法）比要求每个构建者去设环境变量可靠。
    """
    if sdk is None:
        return
    path = HERE / "local.properties"
    # local.properties 含本机绝对路径，不进版本库（.gitignore 里已排除）。
    path.write_text("sdk.dir=%s\n" % str(sdk).replace("\\", "/"), encoding="utf-8")


def write_readme(abi):
    text = """LoomGate · 安卓版
=========================

安装
----
把 LoomGate-android-arm64.apk 拷到手机安装即可（需要允许「安装未知来源应用」）。
只支持 arm64 设备（近几年的手机都是）。

首次使用
--------
1. 打开 App，点「启动」。第一次会弹两个系统窗口，都请同意：
     · 忽略电池优化 —— 不同意的话，手机进入省电模式后会冻结服务，
       定时签到之类的任务就不再按时跑。
     · 允许通知     —— 不同意的话前台服务会被系统降级甚至杀掉，
       症状是「App 过一会儿自己停了」。
2. 面板会自动打开。首次登录的账号密码在：
     Android/data/com.workbuddy2api.app/files/data/panel-data/initial-password.txt
   （用手机上的文件管理器或 adb 查看；也可以在 App 里登录后改掉。）
3. 在面板里扫码加号，或用 Trae 授权加号。

在手机上使用 API
----------------
网关只监听 127.0.0.1:7863（仅本机可达，同一 Wi-Fi 下的别人连不上）。
在别的 App 里把接口地址填成：

    http://127.0.0.1:7863/v1

API 密钥在面板的「密钥」页创建。

数据在哪
--------
全部在应用私有目录里（Android/data/com.workbuddy2api.app/files/data/）：
  config/config.json   配置
  auths/               账号凭据
  data/                密钥库、用量账本、状态
  panel-data/          面板登录凭证
  logs/                运行日志

卸载 App 会一并删掉这些数据。要保留请先备份该目录。

常见问题
--------
· 「启动」后一直没反应：点面板按钮看提示；也可用 `adb logcat -s wb2api` 看日志。
· 网关自己停了：检查是否给了电池优化白名单与通知权限（见上）。
· 换手机/重装后账号没了：数据在应用私有目录，卸载即删，请先备份。
"""
    DIST.mkdir(parents=True, exist_ok=True)
    (DIST / "README.txt").write_text(text, encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description="构建安卓 APK")
    parser.add_argument("--skip-go", action="store_true", help="跳过 Go 交叉编译（复用已有 jniLibs）")
    parser.add_argument("--variant", choices=("release", "debug"), default="release")
    parser.add_argument("--version", default=None, help="版本号（默认读 gateway/VERSION）")
    parser.add_argument("--abi", default=DEFAULT_ABI, choices=sorted(TARGETS),
                        help="目标 ABI（默认 %s；x86_64 供模拟器验证用）" % DEFAULT_ABI)
    args = parser.parse_args()

    version = args.version or read_version()
    version_code(version)
    release_public_key()
    print("构建安卓包 version=%s variant=%s abi=%s" % (version, args.variant, args.abi))

    go = find_go()
    ndk = find_ndk()
    clang = find_clang(ndk, args.abi)
    require_ndk_tools(clang)  # --skip-go 也必须验证既有 ELF，不能绕过 DNS 校验。
    if not args.skip_go:
        print("Go: %s" % go)
        print("NDK: %s" % (ndk or "（未找到）"))
        print("clang: %s" % (clang or "（未找到）"))
        print("交叉编译 Go 二进制 → jniLibs")
        build_go_binaries(go, clang, version, args.abi)

    write_local_properties(find_sdk())
    gradle = find_gradle()
    print("Gradle: %s" % (gradle or "（未找到）"))
    apk = build_apk(gradle, args.variant, args.abi, version, go=go, clang=clang)
    write_readme(args.abi)
    print("\n完成：%s" % apk)
    print("大小：%.1f MB" % (apk.stat().st_size / (1024 * 1024)))


if __name__ == "__main__":
    main()
