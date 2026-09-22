#!/usr/bin/env python3
"""Run one test lane and persist a machine-readable result."""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import platform
import shutil
import subprocess
import sys
import time
from pathlib import Path


SCHEMA_VERSION = 1


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run a test lane and write its passed/failed/unavailable result."
    )
    parser.add_argument("lane")
    parser.add_argument("--report", required=True, type=Path)
    parser.add_argument("--requires-command", action="append", default=[])
    parser.add_argument(
        "--unavailable-pattern",
        action="append",
        default=[],
        help="Command output marker that identifies an unavailable capability.",
    )
    parser.add_argument(
        "--available-on",
        action="append",
        default=[],
        help="Operating system required by the lane (darwin for macOS). Repeatable.",
    )
    parser.add_argument(
        "--require-available",
        action="store_true",
        help="Return a failure when a prerequisite is unavailable.",
    )
    try:
        separator = sys.argv.index("--")
    except ValueError:
        parser.error("a command is required after --")
    args = parser.parse_args(sys.argv[1:separator])
    args.command = sys.argv[separator + 1 :]
    if not args.command:
        parser.error("a command is required after --")
    return args


def operating_system() -> str:
    return platform.system().lower()


def write_report(path: Path, result: dict[str, object]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
    os.replace(temporary, path)
    print("TEST_LANE_RESULT=" + json.dumps(result, sort_keys=True))


def main() -> int:
    args = parse_args()
    system = operating_system()
    unavailable: list[str] = []
    supported = [value.lower() for value in args.available_on]
    if supported and system not in supported:
        unavailable.append(
            f"operating system {system!r} is not in {', '.join(sorted(supported))}"
        )
    for command in args.requires_command:
        if shutil.which(command) is None:
            unavailable.append(f"required command {command!r} is not installed")

    started = dt.datetime.now(dt.timezone.utc)
    result: dict[str, object] = {
        "schema_version": SCHEMA_VERSION,
        "lane": args.lane,
        "platform": system,
        "command": args.command,
        "started_at": started.isoformat(),
        "unavailable_reasons": unavailable,
    }
    if unavailable:
        result.update({"status": "unavailable", "duration_ms": 0, "exit_code": None})
        write_report(args.report, result)
        return 1 if args.require_available else 0

    before = time.monotonic()
    process = subprocess.Popen(
        args.command,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        bufsize=1,
    )
    matched_patterns: set[str] = set()
    assert process.stdout is not None
    for line in process.stdout:
        matched_patterns.update(
            pattern for pattern in args.unavailable_pattern if pattern in line
        )
        print(line, end="", flush=True)
    return_code = process.wait()
    output_unavailable = return_code != 0 and bool(matched_patterns)
    if output_unavailable:
        unavailable.extend(
            f"command reported unavailable marker {pattern!r}"
            for pattern in sorted(matched_patterns)
        )
    result.update(
        {
            "status": (
                "unavailable"
                if output_unavailable
                else "passed"
                if return_code == 0
                else "failed"
            ),
            "duration_ms": round((time.monotonic() - before) * 1000),
            "exit_code": return_code,
            "unavailable_reasons": unavailable,
        }
    )
    write_report(args.report, result)
    if output_unavailable:
        return 1 if args.require_available else 0
    return return_code


if __name__ == "__main__":
    sys.exit(main())
