from __future__ import annotations

import io
import threading

from ftw_optimizer import worker


def test_health_stays_responsive_without_cleaning_memory_during_solve(
    monkeypatch,
) -> None:
    solve_started = threading.Event()
    finish_solve = threading.Event()
    cleanup_calls: list[bool] = []
    thread_errors: list[BaseException] = []
    monkeypatch.setattr(worker, "SOLVE_LOCK", threading.Lock())

    def fake_handle(_raw: object) -> dict[str, object]:
        solve_started.set()
        if not finish_solve.wait(timeout=2):
            raise TimeoutError("test did not release solve")
        return {"ok": True}

    def fake_cleanup() -> None:
        cleanup_calls.append(worker.SOLVE_LOCK.locked())

    monkeypatch.setattr(worker, "handle", fake_handle)
    monkeypatch.setattr(worker, "release_unused_memory", fake_cleanup)

    solve_output = io.StringIO()

    def run_solve() -> None:
        try:
            worker.process_stream(
                io.StringIO(
                    '{"schema_version":1,"request_id":"test","slots":[{}]}\n'
                ),
                solve_output,
            )
        except BaseException as exc:
            thread_errors.append(exc)

    solve_thread = threading.Thread(target=run_solve)
    solve_thread.start()
    assert solve_started.wait(timeout=1)

    health_output = io.StringIO()
    health_thread = threading.Thread(
        target=worker.process_stream,
        args=(
            io.StringIO('{"type":"handshake","protocol_version":1}\n'),
            health_output,
        ),
    )
    health_thread.start()
    health_thread.join(timeout=1)
    assert not health_thread.is_alive()
    assert '"name":"ftw-optimizer"' in health_output.getvalue()
    assert cleanup_calls == []

    finish_solve.set()
    solve_thread.join(timeout=1)
    assert not solve_thread.is_alive()
    assert thread_errors == []
    assert cleanup_calls == [True]
