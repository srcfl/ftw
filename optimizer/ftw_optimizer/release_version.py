from __future__ import annotations

import re
import sys


LAST_SHARED_RELEASE = (1, 3, 1)
_BASE_VERSION = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")


def validate_independent_release_base(version: str) -> tuple[int, int, int]:
    match = _BASE_VERSION.fullmatch(version)
    if match is None:
        raise ValueError("optimizer package version must match X.Y.Z")
    parsed = tuple(int(part) for part in match.groups())
    if parsed <= LAST_SHARED_RELEASE:
        floor = ".".join(str(part) for part in LAST_SHARED_RELEASE)
        raise ValueError(
            f"optimizer package version {version} must be newer than the last shared release {floor}"
        )
    return parsed


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: python -m ftw_optimizer.release_version X.Y.Z")
    try:
        validate_independent_release_base(sys.argv[1])
    except ValueError as exc:
        raise SystemExit(str(exc)) from exc


if __name__ == "__main__":
    main()
