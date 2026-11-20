"""Exercise exact v3.16.1 update sources against actual signed bridge assets.

The original checkout is read-only. Selected source files and all outputs are
kept in the explicit external work root. No credential or production data is
read, no private signing key is required, and HTTP uses a loopback fixture.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import sys

BASELINE = "4b6452e5e0e4c49b45604fd878f6174701bce5eb"
GATEWAY = Path(__file__).resolve().parents[1]
DEFAULT_LEGACY = Path.home() / "Desktop/na/workbuddy2api-git-update/gateway"


def frozen_source(repository, work):
    source = work / "source"
    listing = subprocess.check_output(["git", "-C", str(repository), "ls-tree", "-r", "--name-only",
                                       BASELINE, "--", "internal/hotupdate", "internal/version"], text=True)
    digests = {}
    for name in listing.splitlines():
        if not name.endswith(".go") or name.endswith("_test.go"):
            continue
        raw = subprocess.check_output(["git", "-C", str(repository), "show", BASELINE+":"+name])
        path = source / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)
        digests[name] = hashlib.sha256(raw).hexdigest()
    if not digests:
        raise ValueError("legacy baseline source unavailable")
    (source / "go.mod").write_text("module workbuddy2api\n\ngo 1.26.8\n", encoding="ascii")
    (source / "internal/hotupdate/legacy_probe_test.go").write_bytes(
        Path(__file__).with_name("legacy_loader_fixture.go.txt").read_bytes())
    return source, digests


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--legacy-source", type=Path, default=DEFAULT_LEGACY,
                        help="read-only legacy Git checkout; source is read at the pinned baseline commit")
    parser.add_argument("--assets", type=Path, required=True)
    parser.add_argument("--work-root", type=Path, required=True)
    parser.add_argument("--go", default=os.environ.get("GO", "go"))
    parser.add_argument("--target-os", choices=["linux", "windows"], default="windows" if os.name == "nt" else "linux")
    parser.add_argument("--target-arch", choices=["amd64", "arm64"], default="arm64" if platform.machine().lower() in ("arm64", "aarch64") else "amd64")
    parser.add_argument("--compile-only", action="store_true")
    args = parser.parse_args()
    work = args.work_root.resolve()
    if work.is_relative_to(GATEWAY.parent):
        parser.error("legacy probe outputs must be outside the checkout")
    work.mkdir(parents=True, exist_ok=True)
    source, hashes = frozen_source(args.legacy_source, work)
    (work / "tmp").mkdir(exist_ok=True)
    env = dict(os.environ, GOMAXPROCS="2", CGO_ENABLED="0", GOTOOLCHAIN="local",
               GOFLAGS="-mod=readonly -p=1", GOOS=args.target_os, GOARCH=args.target_arch,
               GOPROXY="off", GOSUMDB="off",
               GOTMPDIR=str(work / "tmp"), TEMP=str(work / "tmp"), TMP=str(work / "tmp"),
               LOOMGATE_BRIDGE_ASSETS=str(args.assets.resolve()),
               LOOMGATE_BRIDGE_PUBLIC_KEY=str(GATEWAY / "release-signing.pub"))
    binary = work / ("legacy-loader-"+args.target_os+"-"+args.target_arch+(".exe" if args.target_os == "windows" else ".test"))
    compile_result = subprocess.run([args.go, "test", "-c", "-o", str(binary), "./internal/hotupdate"],
                                    cwd=source, env=env, capture_output=True, timeout=180)
    report = dict(baseline=BASELINE, source_sha256=hashes,
                  target=args.target_os+"/"+args.target_arch, compiled=compile_result.returncode == 0,
                  private_key_read=False, production_changed=False, test_execution="NOT RUN")
    code = compile_result.returncode
    (work / "compile.log").write_bytes(compile_result.stdout+compile_result.stderr)
    if code == 0 and not args.compile_only:
        result = subprocess.run([str(binary), "-test.v", "-test.timeout=90s"],
                                cwd=source, env=env, capture_output=True, timeout=100)
        (work / "probe.log").write_bytes(result.stdout+result.stderr)
        code = result.returncode
        report.update(test_execution="PASS" if code == 0 else "FAIL",
                      pass_count=result.stdout.count(b"--- PASS:"), fail_count=result.stdout.count(b"--- FAIL:"))
    report["exit_code"] = code
    (work / "result.json").write_text(json.dumps(report, indent=2), encoding="utf-8")
    print(json.dumps({key: value for key, value in report.items() if key != "source_sha256"}))
    return code


if __name__ == "__main__":
    sys.exit(main())
