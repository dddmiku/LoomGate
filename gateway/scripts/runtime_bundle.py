#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-25：生成完整运行包清单及确定文件集的压缩包，排除配置、凭据和运行数据。
# 2026-11-18：豆包桥接组件列为可选整组（--without-doubao-runtime）：镜像构建的上下文
#             只有 gateway/，看不到仓库根的 tools/；显式省略后产出的是旧版 18 文件包，
#             解包器按「整组缺失」接受，半包仍拒绝。
"""Package an already built runtime; this utility never reads deployment data."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import tarfile

# doubao-runtime/* 是豆包官方桌面目录桥接组件。它们随完整运行包一起分发，供
# LOOMGATE_DOUBAO_DESKTOP_PORT 的“已运行官方客户端”连接路径使用；不包含
# profile、账号库或任何捕获数据。清单与 internal/hotupdate/bundle.go 的
# runtimeFiles 必须逐字一致（TestRuntimeFileListsMatch）。
FILES = ('wb2api', 'login', 'credit', 'signin_bin', 'trial_bin', 'activity_bin',
         'fission_bin',
         'login.sh', 'credit.sh', 'signin.sh', 'trial.sh', 'fission.sh',
         'scripts/global_region.py', 'scripts/task_common.py', 'scripts/task_runner.py',
         'scripts/school_open_day_2026.py', 'scripts/growth_center.py',
         # compensation_claim.py 是「补偿积分领取」一次性任务的执行体。
         # 漏在这里的后果是**运行期才暴露**：任务在面板上可见、点「跑一次」也返回
         # 已触发，但脚本文件不存在 → 子进程立刻失败，日志里只有一行 python 报错。
         'scripts/compensation_claim.py',
         'doubao-runtime/bridge.py',
         'doubao-runtime/managed.py',
         'doubao-runtime/stream_bridge.py',
         'doubao-runtime/runtime.js',
         'doubao-runtime/tasks_runtime.js',
         'doubao-runtime/work_runtime.js',
         'doubao-runtime/stream_runtime.js',
         'doubao-runtime/probe_runtime.js',
         'doubao-runtime/probe_cleanup.js',
         'doubao-runtime/requirements.txt')
DOUBAO_RUNTIME = tuple(name for name in FILES if name.startswith('doubao-runtime/'))
LEGACY_FILES = tuple(name for name in FILES if not name.startswith('doubao-runtime/'))


def source_path(gateway, name):
    """Map a runtime member to its checked-in source.

    doubao-runtime lives under tools/ at the repository root; every other member
    is staged from the gateway directory itself. Image builds whose context is
    only gateway/ pass include_doubao=False instead of reaching outside it.
    """
    gateway = Path(gateway)
    if name.startswith('doubao-runtime/'):
        return gateway.parent / 'tools' / name
    return gateway / name


def package(stage, version, arch, archive=None, include_doubao=True):
    stage = Path(stage)
    arch = {'aarch64': 'arm64', 'x86_64': 'amd64', 'armv7l': 'armv7', 'armv7': 'armv7',
            'armv6l': 'armv6', 'armv6': 'armv6', 'armv5l': 'armv5', 'armv5': 'armv5'}.get(arch, arch)
    # armv7 是行空板 M10（DFour UNIHIKER，四核 Cortex-A7）用的 32 位 ARM 目标，
    # 用户 2026-10-13 要求支持。**名字里必须带版本**：arm 的 v5/v6/v7 ABI 不同，
    # 只写 "arm" 会让发布端与自更新端对不上（详见 internal/hotupdate/release.go
    # 的 armAssetName 注释）。
    if arch not in ('amd64', 'arm64', 'armv7', 'armv6', 'armv5'):
        raise ValueError('unsupported runtime architecture')
    members = FILES if include_doubao else LEGACY_FILES
    files = {}
    for name in members:
        path = stage / name
        if not path.is_file() or path.is_symlink():
            raise ValueError('missing regular runtime file: ' + name)
        files[name] = hashlib.sha256(path.read_bytes()).hexdigest()
    manifest = {'format': 1, 'version': version, 'os': 'linux', 'arch': arch, 'files': files}
    (stage / 'manifest.json').write_text(json.dumps(manifest, sort_keys=True, indent=2) + '\n', encoding='utf-8', newline='\n')
    if archive:
        with tarfile.open(archive, 'w:gz') as bundle:
            for name in ('manifest.json', *members):
                data = (stage / name).read_bytes()
                item = tarfile.TarInfo(name)
                item.size = len(data)
                item.mode = 0o755 if '/' not in name and name != 'manifest.json' else 0o644
                bundle.addfile(item, io.BytesIO(data))
    return manifest


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--stage', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--arch', required=True)
    parser.add_argument('--archive')
    parser.add_argument('--without-doubao-runtime', action='store_true',
                        help='omit the optional Doubao helper group (image builds without a repo-root context)')
    args = parser.parse_args()
    package(args.stage, args.version, args.arch, args.archive,
            include_doubao=not args.without_doubao_runtime)


if __name__ == '__main__':
    main()
