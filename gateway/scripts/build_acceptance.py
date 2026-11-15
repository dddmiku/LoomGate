#!/usr/bin/env python3
"""Build CI verification artifacts from release-targets.json; never sign or publish them."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess

import runtime_bundle

GATEWAY = Path(__file__).resolve().parents[1]
MANIFEST = GATEWAY / "scripts" / "release-targets.json"
PROGRAMS = {"wb2api": "server", "login": "login", "credit": "credit", "signin_bin": "signin",
            "trial_bin": "trial", "activity_bin": "activity", "fission_bin": "fission"}


def load_targets(path=MANIFEST):
    data = json.loads(Path(path).read_text(encoding="utf-8"))
    if data.get("schema_version") != 1 or data.get("public_key") != "release-signing.pub":
        raise ValueError("unsupported target manifest or trust-root path")
    if data.get("checksums_asset") != "SHA256SUMS.txt" or data.get("signature_asset") != "SHA256SUMS.txt.sig":
        raise ValueError("legacy integrity asset names must be retained")
    targets = data.get("targets")
    if not isinstance(targets, list) or not targets:
        raise ValueError("target manifest is empty")
    ids, assets = set(), set()
    for target in targets:
        ident = target.get("id", "")
        if not re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+)*", ident) or ident in ids:
            raise ValueError("target ids must be unique safe identifiers")
        ids.add(ident)
        if target.get("goos") not in ("linux", "windows", "android"):
            raise ValueError("unsupported target OS")
        if target.get("goarch") not in ("amd64", "arm64", "arm"):
            raise ValueError("unsupported target architecture")
        if (target.get("goarch") == "arm") != (target.get("goarm") in ("5", "6", "7")):
            raise ValueError("32-bit ARM must declare its GOARM ABI")
        if not isinstance(target.get("ci_build"), bool):
            raise ValueError("every target must explicitly declare CI eligibility")
        if target["goos"] == "android" and (target["ci_build"] or not target.get("manual_requirement")):
            raise ValueError("Android APK requires the existing signing identity outside this unsigned CI")
        if "runtime_asset" in target and target["goos"] != "linux":
            raise ValueError("only Linux uses runtime bundle updates")
        for name in [target.get("update_asset", ""), *([target["runtime_asset"]] if "runtime_asset" in target else [])]:
            if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", name) or name in assets:
                raise ValueError("asset names must be unique basenames")
            assets.add(name)
    return data


def read_public_key(data):
    value = (GATEWAY / data["public_key"]).read_text(encoding="ascii").strip().lower()
    if not re.fullmatch(r"[0-9a-f]{64}", value) or set(value) == {"0"}:
        raise ValueError("existing Ed25519 release public key is missing or invalid")
    return value


def ci_matrix(data):
    return {"include": [{"id": item["id"]} for item in data["targets"] if item["ci_build"]]}


def checked(command, **kwargs):
    result = subprocess.run(command, capture_output=True, text=True, **kwargs)
    if result.returncode:
        # Inputs are source and the public verification key; no credential environment is required.
        raise RuntimeError("build tool failed (%s): %s" % (result.returncode, result.stderr[-5000:]))
    return result.stdout


def verify_binary(go, path, target, public_key, version):
    text = checked([go, "version", "-m", str(path)], timeout=60)
    settings = {}
    for line in text.splitlines():
        parts = line.strip().split("\t", 1)
        if len(parts) == 2 and parts[0] == "build" and "=" in parts[1]:
            key, value = parts[1].split("=", 1)
            settings[key] = json.loads(value) if value.startswith('"') else value
    flags = shlex.split(settings.get("-ldflags", ""))
    injections = {}
    for index, flag in enumerate(flags):
        assignment = flags[index + 1] if flag == "-X" and index + 1 < len(flags) else flag[3:] if flag.startswith("-X=") else ""
        if "=" in assignment:
            key, value = assignment.split("=", 1)
            injections[key] = value
    if any(settings.get(name) != target[name.lower()] for name in ("GOOS", "GOARCH")):
        raise ValueError("compiled artifact does not match the target platform")
    if settings.get("CGO_ENABLED") != "0" or (target.get("goarm") and settings.get("GOARM", "").split(",")[0] != target["goarm"]):
        raise ValueError("compiled artifact has unexpected CGO/ARM ABI settings")
    if injections.get("workbuddy2api/internal/version.ReleaseKey") != public_key or injections.get("workbuddy2api/internal/version.Version") != version:
        raise ValueError("compiled artifact lacks the pinned release verifier or CI version identity")


def build_target(data, ident, work_root, commit):
    if not re.fullmatch(r"[0-9a-fA-F]{40}", commit):
        raise ValueError("a full source commit is required for build attribution")
    target = next((item for item in data["targets"] if item["id"] == ident), None)
    if not target or not target["ci_build"]:
        raise ValueError("target is not eligible for this unsigned CI workflow")
    public_key = read_public_key(data)
    work_root = Path(work_root).resolve()
    if work_root.is_relative_to(GATEWAY.parent.resolve()):
        raise ValueError("build outputs must be outside the source checkout")
    stage = work_root / "stage" / ident
    out = work_root / "artifacts" / ident
    for directory in (stage, out):
        if directory.is_symlink() or not directory.resolve().is_relative_to(work_root):
            raise ValueError("build directories cannot redirect outside the work root")
    allowed = {target["update_asset"], data["checksums_asset"], "build-report.json"}
    if target.get("runtime_asset"):
        allowed.add(target["runtime_asset"])
    if out.exists() and any(p.name not in allowed or not p.is_file() or p.is_symlink() for p in out.iterdir()):
        raise ValueError("artifact output contains files outside the declared build allowlist")
    stage.mkdir(parents=True, exist_ok=True)
    out.mkdir(parents=True, exist_ok=True)
    version = "dev-" + commit[:12].lower()
    built_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    flags = ("-s -w -X workbuddy2api/internal/version.Version=%s "
             "-X workbuddy2api/internal/version.Commit=%s -X workbuddy2api/internal/version.BuiltAt=%s "
             "-X workbuddy2api/internal/version.ReleaseKey=%s" % (version, commit, built_at, public_key))
    env = dict(os.environ, GOOS=target["goos"], GOARCH=target["goarch"], CGO_ENABLED="0", GOMAXPROCS="2")
    if target.get("goarm"):
        env["GOARM"] = target["goarm"]
    else:
        env.pop("GOARM", None)
    go = os.environ.get("GO", "go")
    programs = PROGRAMS if target.get("runtime_asset") else {"wb2api": "server"}
    for output, package in programs.items():
        path = stage / output
        checked([go, "build", "-p=1", "-mod=readonly", "-buildvcs=false", "-trimpath", "-ldflags", flags,
                 "-o", str(path), "./cmd/" + package], cwd=GATEWAY, env=env, timeout=1200)
        verify_binary(go, path, target, public_key, version)
    artifacts = [target["update_asset"]]
    shutil.copyfile(stage / "wb2api", out / target["update_asset"])
    if target.get("runtime_asset"):
        for name in runtime_bundle.FILES:
            if name in PROGRAMS:
                continue
            path = stage / name
            path.parent.mkdir(parents=True, exist_ok=True)
            # Only the runtime packager's explicit source allowlist is eligible.
            path.write_bytes((GATEWAY / name).read_bytes().replace(b"\r\n", b"\n"))
        arch = "armv" + target["goarm"] if target["goarch"] == "arm" else target["goarch"]
        runtime_bundle.package(stage, version, arch, out / target["runtime_asset"])
        artifacts.append(target["runtime_asset"])
    sums = {name: hashlib.sha256((out / name).read_bytes()).hexdigest() for name in artifacts}
    (out / data["checksums_asset"]).write_text("".join("%s  %s\n" % (sums[name], name) for name in sorted(sums)), encoding="ascii")
    report = {"schema_version": 1, "target": ident, "commit": commit, "version": version,
              "public_key_sha256": hashlib.sha256(bytes.fromhex(public_key)).hexdigest(),
              "artifacts": sums, "release_signature": "NOT GENERATED", "published": False,
              "apk_builds": "NOT RUN; existing APK signing identity required"}
    (out / "build-report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    validate = sub.add_parser("validate")
    validate.add_argument("--github-output", type=Path)
    build = sub.add_parser("build")
    build.add_argument("--target", required=True)
    build.add_argument("--work-root", type=Path, required=True)
    build.add_argument("--commit", required=True)
    args = parser.parse_args()
    data = load_targets()
    read_public_key(data)
    if args.command == "validate":
        matrix = json.dumps(ci_matrix(data), separators=(",", ":"))
        if args.github_output:
            with args.github_output.open("a", encoding="utf-8") as output:
                output.write("matrix=" + matrix + "\n")
        print(matrix)
    else:
        print(json.dumps(build_target(data, args.target, args.work_root, args.commit), indent=2))


if __name__ == "__main__":
    main()
