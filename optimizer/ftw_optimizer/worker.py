from __future__ import annotations

import ctypes
import gc
import json
import sys
import traceback
from typing import Any

import cvxpy as cp

from .model import solve
from .protocol import ProtocolError, error_response, parse_request


def release_unused_memory() -> None:
    """Return solver heap pages when the platform allocator supports it."""
    gc.collect()
    try:
        malloc_trim = ctypes.CDLL(None).malloc_trim
    except (AttributeError, OSError):
        return
    malloc_trim.argtypes = [ctypes.c_size_t]
    malloc_trim.restype = ctypes.c_int
    malloc_trim(0)


def handle(raw: Any) -> dict[str, Any]:
    request_id = "unknown"
    try:
        parsed = parse_request(raw)
        request_id = parsed.request_id
        return solve(parsed.payload)
    except ProtocolError as exc:
        return error_response(request_id, "invalid_request", str(exc))
    except cp.error.SolverError as exc:
        return error_response(request_id, "solver_error", str(exc))
    except Exception as exc:  # worker boundary: one bad request must not kill the process
        traceback.print_exc(file=sys.stderr)
        return error_response(request_id, "internal_error", str(exc))


def main() -> None:
    for line in sys.stdin:
        if not line.strip():
            continue
        try:
            raw = json.loads(line)
        except json.JSONDecodeError as exc:
            response = error_response("unknown", "invalid_json", str(exc))
        else:
            response = handle(raw)
        sys.stdout.write(json.dumps(response, separators=(",", ":"), allow_nan=False) + "\n")
        sys.stdout.flush()
        response = None
        release_unused_memory()


if __name__ == "__main__":
    main()
