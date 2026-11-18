#!/usr/bin/env python3
"""Run every console Node regression (node --test) in one pass.

The console tests are plain ``*.test.cjs`` files next to the frontend sources.
They assert pure behaviour (grouping, scope, rate parsing, commit retry) without
starting a browser or contacting an upstream. Keeping one entry point avoids the
"forgot to run the new test file" failure mode.
"""
from __future__ import annotations

import argparse
import subprocess
import sys
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--console", default="", help="console directory (default: gateway/panel/console)")
    parser.add_argument("--node", default="node", help="node executable")
    args = parser.parse_args()

    root = Path(__file__).resolve().parents[1]
    console = Path(args.console) if args.console else root / "panel" / "console"
    if not console.is_dir():
        print(f"console directory not found: {console}", file=sys.stderr)
        return 1

    tests = sorted(console.glob("*.test.cjs"))
    if not tests:
        print(f"no tests under {console}", file=sys.stderr)
        return 1

    command = [args.node, "--test", *[str(path) for path in tests]]
    print("running:", " ".join(str(path.name) for path in tests))
    return subprocess.run(command, cwd=console, check=False).returncode


if __name__ == "__main__":
    raise SystemExit(main())
