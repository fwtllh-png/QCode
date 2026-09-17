#!/usr/bin/env bash
# Validate maintained Markdown links and the Chinese-only document layout.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

python3 - "$ROOT" <<'PY'
from __future__ import annotations

import pathlib
import re
import subprocess
import sys
import urllib.parse

root = pathlib.Path(sys.argv[1]).resolve()
errors: list[str] = []

listed = subprocess.run(
    [
        "git",
        "ls-files",
        "--cached",
        "--others",
        "--exclude-standard",
        "-z",
        "--",
        "*.md",
        ":(exclude)docs/DEEPSEEK-LIVE.zh-CN.md",
    ],
    cwd=root,
    check=True,
    capture_output=True,
).stdout
markdown_files = sorted(
    root / pathlib.Path(raw.decode("utf-8"))
    for raw in listed.split(b"\0")
    if raw and (root / pathlib.Path(raw.decode("utf-8"))).is_file()
)
link_pattern = re.compile(r"!?\[[^\]]*\]\(([^)]+)\)")

for source in markdown_files:
    text = source.read_text(encoding="utf-8")
    for raw_target in link_pattern.findall(text):
        target = raw_target.strip()
        if not target or target.startswith(("#", "http://", "https://", "mailto:")):
            continue
        target = target.split(maxsplit=1)[0].strip("<>")
        path_text = urllib.parse.unquote(target.split("#", 1)[0])
        if not path_text:
            continue
        resolved = (source.parent / path_text).resolve()
        try:
            resolved.relative_to(root)
        except ValueError:
            errors.append(f"{source.relative_to(root)}: link escapes repository: {raw_target}")
            continue
        if not resolved.exists():
            errors.append(f"{source.relative_to(root)}: missing link target: {raw_target}")

chinese = root / "docs" / "zh-CN"
for forbidden in (
    root / "docs" / "en",
    root / "docs" / "book" / "en",
):
    if forbidden.exists():
        errors.append(
            f"English documentation tree must not exist: {forbidden.relative_to(root)}"
        )

required_chinese = (
    chinese / "README.md",
    root / "README.md",
    root / "CONTRIBUTING.md",
    root / "SECURITY.md",
    root / "scripts" / "README.md",
)
for required in required_chinese:
    if not required.is_file():
        errors.append(f"missing Chinese documentation entry: {required.relative_to(root)}")

removed_patterns = (
    "docs/rfc/",
    "docs/ARCHITECTURE.zh-CN.md",
    "docs/USAGE.zh-CN.md",
    "docs/ROADMAP.zh-CN.md",
)
for source in markdown_files:
    text = source.read_text(encoding="utf-8")
    for pattern in removed_patterns:
        if pattern in text:
            errors.append(f"{source.relative_to(root)}: references removed document: {pattern}")

if errors:
    print("documentation check failed:", file=sys.stderr)
    for error in errors:
        print(f"  - {error}", file=sys.stderr)
    raise SystemExit(1)

print(f"documentation check passed: {len(markdown_files)} Markdown files")
PY
