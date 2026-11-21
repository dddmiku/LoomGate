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
    parser.add_argument("--browser", action="store_true", help="also run synthetic-loopback Chromium interaction regressions")
    parser.add_argument("--browser-output", type=Path, help="evidence directory outside the source tree (required with --browser)")
    args = parser.parse_args()

    root = Path(__file__).resolve().parents[1]
    output = args.browser_output.resolve() if args.browser_output else None
    if args.browser and (output is None or output.is_relative_to(root.parent)):
        parser.error("--browser requires --browser-output outside the source tree")
    console = Path(args.console).resolve() if args.console else root / "panel" / "console"
    if not console.is_dir():
        print(f"console directory not found: {console}", file=sys.stderr)
        return 1
    if args.browser and console != (root / "panel" / "console").resolve():
        print("browser mode only supports gateway/panel/console", file=sys.stderr)
        return 1

    tests = sorted(console.glob("*.test.cjs"))
    if not tests:
        print(f"no tests under {console}", file=sys.stderr)
        return 1

    command = [args.node, "--test", *[str(path) for path in tests]]
    print("running:", " ".join(str(path.name) for path in tests), flush=True)
    code = subprocess.run(command, cwd=console, check=False).returncode
    if code or not args.browser:
        return code
    command = [sys.executable, "-I", "-B", str(root / "tests" / "console_interactions_browser.py"),
               "--output", str(output), "--assets", str(console.parent)]
    code = subprocess.run(command, cwd=root, check=False).returncode
    if code:
        return code
    for script, directory in [("connection_repeat_browser.py", "connections"),
                              ("usage_filters_browser.py", "usage"),
                              ("token_units_browser.py", "tokens")]:
        command = [sys.executable, "-I", "-B", str(root / "tests" / script),
                   "--output", str(output / directory)]
        code = subprocess.run(command, cwd=root, check=False).returncode
        if code:
            return code
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
