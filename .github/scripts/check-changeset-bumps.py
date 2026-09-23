#!/usr/bin/env python3
"""Reject major bumps in changed Core changesets."""

import re
import sys
from pathlib import Path


def has_major_bump(path: Path) -> bool:
    lines = path.read_text(encoding="utf-8").splitlines()
    if not lines or lines[0] != "---":
        return False  # Changesets validates malformed entries separately.
    for line in lines[1:]:
        if line == "---":
            break
        match = re.fullmatch(r"\s*['\"]?ftw['\"]?\s*:\s*(.*?)\s*", line)
        if match and match.group(1).split("#", 1)[0].strip().strip("'\"") == "major":
            return True
    return False


def main() -> int:
    rejected = []
    for name in sys.argv[1:]:
        path = Path(name)
        if path.name == "README.md" or not path.is_file():
            continue
        if has_major_bump(path):
            rejected.append(name)
    if rejected:
        for name in rejected:
            print(f"Core major bump is not allowed: {name}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
