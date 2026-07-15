from __future__ import annotations

import argparse
import json
import sys
import uuid
from pathlib import Path
from typing import Any

from .worker import handle


def _load(path: str) -> dict[str, Any]:
    if path == "-":
        return json.load(sys.stdin)
    with Path(path).open("r", encoding="utf-8") as source:
        return json.load(source)


def main() -> None:
    parser = argparse.ArgumentParser(description="Replay a persisted ftw planner diagnostic")
    parser.add_argument("diagnostic", help="diagnostic JSON file, or - for stdin")
    parser.add_argument("--solver", choices=["HIGHS", "CLARABEL"])
    parser.add_argument("--formulation", choices=["auto", "milp", "relaxed"])
    parser.add_argument("--time-limit-s", type=float)
    args = parser.parse_args()

    diagnostic = _load(args.diagnostic)
    request = diagnostic.get("optimizer_input")
    if not isinstance(request, dict):
        raise SystemExit("diagnostic has no optimizer_input; it predates the mathematical planner")
    request = dict(request)
    request["request_id"] = f"replay-{uuid.uuid4()}"
    settings = dict(request.get("settings", {}))
    if args.solver:
        settings["solver"] = args.solver
    if args.formulation:
        settings["formulation"] = args.formulation
    if args.time_limit_s is not None:
        settings["time_limit_s"] = args.time_limit_s
    request["settings"] = settings
    response = handle(request)
    json.dump(response, sys.stdout, indent=2, allow_nan=False)
    sys.stdout.write("\n")
    if not response.get("ok"):
        raise SystemExit(2)


if __name__ == "__main__":
    main()
