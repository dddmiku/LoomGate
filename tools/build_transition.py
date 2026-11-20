#!/usr/bin/env python3
"""Build unsigned legacy bridge assets using the maintained release pipeline.

Linux bridge manifests keep the 3.17.x tag and the exact 18-member legacy
contract. Android uses an existing signed LoomGate 1.x APK unchanged: assigning
the bridge's 3.x version to an APK would block its next 1.x upgrade.

This utility never reads a private key, signs a release or uploads anything.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[1]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


acceptance = load("transition_acceptance", ROOT / "gateway/scripts/build_acceptance.py")
android = load("transition_android", ROOT / "android/build.py")


def bridge_tag(value):
    match = re.fullmatch(r"v3\.(\d+)\.(\d+)", value)
    if not match or int(match.group(1)) < 17:
        raise ValueError("bridge tag must be v3.17.0 or a later 3.x minor")
    return value


def android_bridge_asset(apk, sdk):
    android.verify_apk_signature(apk, sdk)
    identity = android.read_apk_manifest(apk, sdk)
    tag = "v"+identity["version_name"].removeprefix("v")
    if identity["package"] != android.APK_PACKAGE or not re.fullmatch(r"v1\.\d+\.\d+", tag):
        raise ValueError("bridge APK must be an existing LoomGate 1.x package")
    if identity["version_code"] != android.version_code(tag) or identity["version_code"] <= 7_999_999:
        raise ValueError("bridge APK does not advance past the published 3.17.x Android generation")
    if tag != android.read_version():
        raise ValueError("bridge APK must match the current gateway source version")
    return identity


def build(tag, apk, work_root, sdk=None, commit=""):
    bridge_tag(tag)
    work = Path(work_root).resolve()
    if work.is_relative_to(ROOT) or work == ROOT:
        raise ValueError("transition assets must be outside the source checkout")
    apk = Path(apk).resolve()
    identity = android_bridge_asset(apk, sdk or android.find_sdk())
    data = acceptance.load_targets()
    if not commit:
        commit = subprocess.check_output(["git", "-C", str(ROOT), "rev-parse", "HEAD"], text=True).strip()
    if subprocess.check_output(["git", "-C", str(ROOT), "status", "--porcelain"], text=True).strip():
        raise ValueError("transition build requires a clean, attributable source checkout")
    assets = work / "assets"
    allowed = {"wb2api-windows-amd64", "workbuddy2api-android-arm64.apk", "SHA256SUMS.txt",
               "build-report.json", *["wb2api-runtime-linux-"+arch+".tar.gz" for arch in ("amd64", "arm64", "armv7")]}
    if assets.exists() and any(p.is_symlink() or not p.is_file() or p.name not in allowed for p in assets.iterdir()):
        raise ValueError("transition output contains unexpected files; preserve and inspect them")
    assets.mkdir(parents=True, exist_ok=True)
    reports = []
    for target in ("linux-amd64", "linux-arm64", "linux-armv7", "windows-amd64"):
        report = acceptance.build_target(data, target, work / "build", commit, tag)
        declared = next(item for item in data["targets"] if item["id"] == target)
        name = declared.get("runtime_asset", declared["update_asset"])
        source = work / "build/artifacts" / target / name
        if hashlib.sha256(source.read_bytes()).hexdigest() != report["artifacts"][name]:
            raise ValueError("transition source artifact differs from its build report")
        shutil.copyfile(source, assets / name)
        reports.append(report)
    name = "workbuddy2api-android-arm64.apk"
    shutil.copyfile(apk, assets / name)
    hashes = {path.name: hashlib.sha256(path.read_bytes()).hexdigest() for path in assets.iterdir()
              if path.name not in ("SHA256SUMS.txt", "build-report.json")}
    (assets / "SHA256SUMS.txt").write_text("".join(hashes[name]+"  "+name+"\n" for name in sorted(hashes)), encoding="ascii")
    report = dict(tag=tag, commit=commit, targets=reports, android=identity, assets=hashes,
                  release_signature="NOT GENERATED", published=False, private_key_read=False)
    (assets / "build-report.json").write_text(json.dumps(report, indent=2)+"\n", encoding="utf-8")
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--android-apk", type=Path, required=True, help="already signed 1.x APK using the current version-code epoch")
    parser.add_argument("--work-root", type=Path, required=True)
    parser.add_argument("--sdk", type=Path)
    args = parser.parse_args()
    report = build(args.tag, args.android_apk, args.work_root, args.sdk)
    print(json.dumps(dict(tag=report["tag"], assets=sorted(report["assets"]),
                          release_signature=report["release_signature"], published=False)))


if __name__ == "__main__":
    main()
