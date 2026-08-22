from __future__ import annotations

import argparse
import ctypes
import gc
import importlib.metadata
import json
import os
import socket
import sys
import threading
import time
import traceback
from collections import OrderedDict
from collections.abc import Callable
from pathlib import Path
from typing import Any

import cvxpy as cp

from .deadline import SolveCancelled, SolveDeadline, SolveDeadlineExceeded
from .model import solve
from .protocol import ParsedRequest, ProtocolError, error_response, parse_request


# PROTOCOL_VERSION is what this worker speaks; MIN_PROTOCOL_VERSION is the
# oldest Core it still works with. Core accepts any overlap with its own window,
# so widening this range — rather than moving it — keeps an updated optimizer
# usable by a Core that has not been updated yet.
#
# Prefer adding to FEATURES over bumping the protocol. A feature an old Core
# does not know about costs it nothing; a protocol bump makes every Core outside
# the window stop using this optimizer at once.
MIN_PROTOCOL_VERSION = 1
PROTOCOL_VERSION = 1
FEATURES = [
    "champion",
    "recourse",
    "multistage",
    "commercial_constraints_v1",
    "cancel_request",
    "preference_flatten_peaks",
]


class _SolveLock:
    def __init__(self) -> None:
        self._condition = threading.Condition()
        self._held = False

    def acquire_until(self, deadline: SolveDeadline) -> bool:
        with self._condition:
            while self._held:
                self._condition.wait(
                    timeout=min(
                        deadline.remaining_s("optimizer queue"),
                        threading.TIMEOUT_MAX,
                    )
                )
            deadline.check("optimizer queue")
            self._held = True
            return True

    def release(self) -> None:
        with self._condition:
            if not self._held:
                raise RuntimeError("cannot release an unlocked solve lock")
            self._held = False
            self._condition.notify_all()

    def locked(self) -> bool:
        with self._condition:
            return self._held

    def notify_waiters(self) -> None:
        with self._condition:
            self._condition.notify_all()


class _ActiveRequests:
    def __init__(self, max_pending_cancels: int = 256) -> None:
        self._lock = threading.Lock()
        self._active: dict[str, list[SolveDeadline]] = {}
        self._pending_cancels: OrderedDict[str, None] = OrderedDict()
        self._max_pending_cancels = max_pending_cancels

    def register(self, request_id: str, deadline: SolveDeadline) -> None:
        with self._lock:
            self._active.setdefault(request_id, []).append(deadline)
            cancel_now = request_id in self._pending_cancels
            self._pending_cancels.pop(request_id, None)
        if cancel_now:
            deadline.cancel()

    def unregister(self, request_id: str, deadline: SolveDeadline) -> None:
        with self._lock:
            deadlines = self._active.get(request_id)
            if deadlines is None:
                return
            self._active[request_id] = [
                candidate for candidate in deadlines if candidate is not deadline
            ]
            if not self._active[request_id]:
                del self._active[request_id]

    def cancel(self, request_id: str) -> bool:
        with self._lock:
            deadlines = tuple(self._active.get(request_id, ()))
            if not deadlines:
                self._pending_cancels[request_id] = None
                self._pending_cancels.move_to_end(request_id)
                while len(self._pending_cancels) > self._max_pending_cancels:
                    self._pending_cancels.popitem(last=False)
        for deadline in deadlines:
            try:
                deadline.cancel()
            except Exception:
                # The token was set before HiGHS was asked to stop. Keep the
                # cancel connection alive even if that best-effort call fails.
                traceback.print_exc(file=sys.stderr)
        return bool(deadlines)


SOLVE_LOCK: Any = _SolveLock()
ACTIVE_REQUESTS = _ActiveRequests()


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


def handle(
    raw: Any,
    *,
    received_at: float | None = None,
    clock: Callable[[], float] = time.perf_counter,
    parsed: ParsedRequest | None = None,
    deadline: SolveDeadline | None = None,
) -> dict[str, Any]:
    if received_at is None:
        received_at = clock()
    request_id = "unknown"
    try:
        if parsed is None:
            parsed = parse_request(raw)
        request_id = parsed.request_id
        if deadline is None:
            deadline = SolveDeadline.from_payload(
                parsed.payload,
                started_at=received_at,
                clock=clock,
            )
        deadline.check("optimizer queue")
        response = solve(parsed.payload, deadline=deadline)
        deadline.check("optimizer response")
        return response
    except ProtocolError as exc:
        return error_response(request_id, "invalid_request", str(exc))
    except SolveCancelled as exc:
        return error_response(request_id, "cancelled", str(exc))
    except SolveDeadlineExceeded as exc:
        return error_response(request_id, "deadline_exceeded", str(exc))
    except cp.error.SolverError as exc:
        return error_response(request_id, "solver_error", str(exc))
    except Exception as exc:  # worker boundary: one bad request must not kill the process
        traceback.print_exc(file=sys.stderr)
        return error_response(request_id, "internal_error", str(exc))


def handshake(raw: Any) -> dict[str, Any] | None:
    if not isinstance(raw, dict) or raw.get("type") != "handshake":
        return None
    try:
        version = importlib.metadata.version("ftw-optimizer")
    except importlib.metadata.PackageNotFoundError:
        version = "dev"
    return {
        "name": "ftw-optimizer",
        "version": os.environ.get("FTW_OPTIMIZER_VERSION", version),
        "protocol_version": PROTOCOL_VERSION,
        "protocol_min": MIN_PROTOCOL_VERSION,
        "protocol_max": PROTOCOL_VERSION,
        "features": FEATURES,
        "build_sha": os.environ.get("FTW_OPTIMIZER_BUILD_SHA", ""),
    }


def cancel_request(raw: Any) -> dict[str, Any] | None:
    if not isinstance(raw, dict) or raw.get("type") != "cancel_request":
        return None
    request_id = raw.get("request_id")
    if not isinstance(request_id, str) or not request_id:
        return error_response(
            "unknown",
            "invalid_request",
            "request_id must be a non-empty string",
        )
    protocol_version = raw.get("protocol_version", PROTOCOL_VERSION)
    if (
        isinstance(protocol_version, bool)
        or not isinstance(protocol_version, int)
        or not MIN_PROTOCOL_VERSION <= protocol_version <= PROTOCOL_VERSION
    ):
        return error_response(
            request_id,
            "invalid_request",
            f"unsupported protocol_version {protocol_version!r}; expected "
            f"{MIN_PROTOCOL_VERSION}..{PROTOCOL_VERSION}",
        )
    active = ACTIVE_REQUESTS.cancel(request_id)
    notify_waiters = getattr(SOLVE_LOCK, "notify_waiters", None)
    if notify_waiters is not None:
        notify_waiters()
    return {
        "type": "cancel_ack",
        "protocol_version": PROTOCOL_VERSION,
        "request_id": request_id,
        "ok": True,
        "active": active,
    }


def _acquire_solve_lock(deadline: SolveDeadline) -> bool:
    acquire_until = getattr(SOLVE_LOCK, "acquire_until", None)
    if acquire_until is not None:
        return bool(acquire_until(deadline))
    wait_s = min(
        deadline.remaining_s("optimizer queue"),
        threading.TIMEOUT_MAX,
    )
    return bool(SOLVE_LOCK.acquire(timeout=wait_s))


def process_stream(
    reader: Any,
    writer: Any,
    *,
    clock: Callable[[], float] = time.perf_counter,
) -> None:
    for line in reader:
        if not line.strip():
            continue
        received_at = clock()
        try:
            raw = json.loads(line)
        except json.JSONDecodeError as exc:
            response = error_response("unknown", "invalid_json", str(exc))
        else:
            response = handshake(raw)
            if response is None:
                response = cancel_request(raw)
            if response is None:
                # Handshakes stay responsive while a solve is in progress.
                # Cancel frames also bypass the solve lock so they can stop its
                # current owner or remove a queued request at once.
                request_id = "unknown"
                deadline: SolveDeadline | None = None
                registered = False
                try:
                    try:
                        parsed = parse_request(raw)
                        request_id = parsed.request_id
                        deadline = SolveDeadline.from_payload(
                            parsed.payload,
                            started_at=received_at,
                            clock=clock,
                        )
                        ACTIVE_REQUESTS.register(request_id, deadline)
                        registered = True
                        acquired = _acquire_solve_lock(deadline)
                    except ProtocolError as exc:
                        response = error_response(
                            request_id,
                            "invalid_request",
                            str(exc),
                        )
                    except SolveCancelled as exc:
                        response = error_response(
                            request_id,
                            "cancelled",
                            str(exc),
                        )
                    except SolveDeadlineExceeded as exc:
                        response = error_response(
                            request_id,
                            "deadline_exceeded",
                            str(exc),
                        )
                    else:
                        if not acquired:
                            response = error_response(
                                request_id,
                                "deadline_exceeded",
                                "optimizer queue deadline exceeded",
                            )
                        else:
                            try:
                                response = handle(
                                    raw,
                                    received_at=received_at,
                                    clock=clock,
                                    parsed=parsed,
                                    deadline=deadline,
                                )
                                try:
                                    if not deadline.is_cancelled():
                                        writer.write(
                                            json.dumps(
                                                response,
                                                separators=(",", ":"),
                                                allow_nan=False,
                                            )
                                            + "\n"
                                        )
                                        writer.flush()
                                finally:
                                    response = None
                                    release_unused_memory()
                            finally:
                                SOLVE_LOCK.release()
                            continue
                finally:
                    if registered:
                        assert deadline is not None
                        ACTIVE_REQUESTS.unregister(request_id, deadline)
                if deadline is not None and deadline.is_cancelled():
                    continue
        writer.write(json.dumps(response, separators=(",", ":"), allow_nan=False) + "\n")
        writer.flush()


def serve_unix(socket_path: str) -> None:
    path = Path(socket_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        path.unlink()
    except FileNotFoundError:
        pass
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
            server.bind(str(path))
            os.chmod(path, 0o660)
            server.listen(16)
            def serve_connection(conn: socket.socket) -> None:
                try:
                    with conn:
                        with conn.makefile("r", encoding="utf-8") as reader:
                            with conn.makefile("w", encoding="utf-8") as writer:
                                process_stream(reader, writer)
                except (BrokenPipeError, ConnectionResetError):
                    # Core timed out/cancelled. The worker stays alive and the
                    # next replan can use it (or core's fallback) normally.
                    return

            while True:
                conn, _ = server.accept()
                threading.Thread(target=serve_connection, args=(conn,), daemon=True).start()
    finally:
        try:
            path.unlink()
        except FileNotFoundError:
            pass


def main() -> None:
    parser = argparse.ArgumentParser(description="FTW mathematical optimizer worker")
    parser.add_argument("--socket", default=os.environ.get("FTW_OPTIMIZER_SOCKET", ""))
    args = parser.parse_args()
    if args.socket:
        serve_unix(args.socket)
        return
    process_stream(sys.stdin, sys.stdout)


if __name__ == "__main__":
    main()
