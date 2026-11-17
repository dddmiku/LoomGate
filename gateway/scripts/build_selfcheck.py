#!/usr/bin/env python3
"""Release candidate self-check: exercise the real built gateway entry point.

This is not a test double. It compiles the real cmd/server for the host
platform with the same injected metadata the release scripts use, then runs
`--loomgate-build-info` and compares the reported version, commit, platform and
release-key digest with the expected values.

Usage:
  python -I -B -X utf8 gateway/scripts/build_selfcheck.py \
      --output D:/LoomGate-work/build/selfcheck --version v1.0.0-rc --commit <sha>
"""
import argparse
import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path

GATEWAY = Path(__file__).resolve().parents[1]
KEY = (GATEWAY / "release-signing.pub").read_text(encoding="ascii").strip().lower()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True)
    parser.add_argument("--version", default="dev-selfcheck")
    parser.add_argument("--commit", required=True)
    args = parser.parse_args()
    if len(args.commit) != 40 or any(c not in "0123456789abcdefABCDEF" for c in args.commit):
        raise SystemExit("full_source_commit_required")
    if not hashlib.sha256(bytes.fromhex(KEY)).hexdigest():
        raise SystemExit("release_key_invalid")
    out = Path(args.output).resolve()
    if not out.is_relative_to(Path("D:/").resolve()) and os.name == "nt":
        raise SystemExit("work_output_must_be_on_d_drive")
    out.mkdir(parents=True, exist_ok=True)
    binary = out / ("loomgate-selfcheck.exe" if os.name == "nt" else "loomgate-selfcheck")
    flags = ("-s -w -X workbuddy2api/internal/version.Version=%s "
             "-X workbuddy2api/internal/version.Commit=%s "
             "-X workbuddy2api/internal/version.ReleaseKey=%s" % (args.version, args.commit, KEY))
    env = dict(os.environ, GOFLAGS="-mod=readonly -p=1", GOMAXPROCS="2", CGO_ENABLED="0")
    go = os.environ.get("GO", "go")
    build = subprocess.run([go, "build", "-trimpath", "-buildvcs=false", "-ldflags", flags,
                            "-o", str(binary), "./cmd/server"], cwd=GATEWAY, env=env,
                           capture_output=True, text=True, timeout=1200)
    if build.returncode:
        raise SystemExit("selfcheck_build_failed: " + build.stderr[-2000:])
    run = subprocess.run([str(binary), "--loomgate-build-info"], capture_output=True,
                         text=True, timeout=60)
    if run.returncode:
        raise SystemExit("selfcheck_entrypoint_failed")
    document = json.loads(run.stdout)
    expected = {
        "version": args.version,
        "commit": args.commit,
        "goos": "windows" if os.name == "nt" else "linux",
        "goarch": "amd64" if sys.maxsize > 2**32 else "386",
        "verification_enabled": True,
        "release_key_sha256": hashlib.sha256(bytes.fromhex(KEY)).hexdigest(),
    }
    for field, want in expected.items():
        if document.get(field) != want:
            raise SystemExit("selfcheck_mismatch:" + field)
    print(json.dumps({"result": "PASS", "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                      "binary_bytes": binary.stat().st_size, "identity": document}, ensure_ascii=False))


if __name__ == "__main__":
    main()
