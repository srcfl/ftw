---
"forty-two-watts": patch
---

State: skip the boot integrity check when the DB is already known-good so a large
`state.db` restarts in seconds instead of minutes.

`heal.go`'s boot-time `PRAGMA quick_check` scans the entire file, which on a
multi-GB `state.db` is minutes of disk I/O on a Pi — and it ran on every boot,
making a restart look like a hang. Now a persistent `<db>.clean` "verified-good"
marker is armed by `Open` after a successful open, and `openChecked` skips the
boot check whenever it is present. On a 1.3 GB field DB the integrity gate went
from >5 min to ~40 ms (measured).

The marker is deliberately NOT a clean-shutdown flag: it persists across both
clean shutdowns and crashes (a crash doesn't corrupt a WAL database, so it must
not force a slow re-scan), so fast restarts never depend on how the process
exited. The only thing that removes it is `Store.VerifyInBackground` finding real
corruption — that runs the full check off the startup hot path after the app is
already serving, and on failure removes the marker so the next boot runs the full
check and heals from snapshot. At-rest SD-card rot is therefore still caught,
recovery just takes the next boot instead of blocking this one. The background
scan is cancellable (`Close` interrupts it via `sqlite3_interrupt`) so a shutdown
isn't blocked by an in-flight multi-minute scan. Startup phases are now timed in
the logs (`integrity gate complete elapsed=…`, `migrations complete elapsed=…`)
so a slow phase is visible instead of a silent gap. See `docs/fast-restart.md`.
