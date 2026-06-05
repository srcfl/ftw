---
"forty-two-watts": patch
---

State: skip the boot integrity check after a clean shutdown so a large `state.db`
restarts in seconds instead of minutes.

`heal.go`'s boot-time `PRAGMA quick_check` scans the entire file, which on a
multi-GB `state.db` is minutes of disk I/O on a Pi — and it ran on every boot,
making a restart look like a hang. Now `Close` drops a single-use `<db>.clean`
marker on a clean shutdown and `openChecked` skips the check when it's present
(consuming it, so a crash still forces a check next boot). On a 1.3 GB field DB
the integrity gate went from >5 min to 48 ms (measured).

The safety net is preserved: `Store.VerifyInBackground` runs the full check off
the startup hot path after the app is already serving, and on detected corruption
arms a heal for the next boot (leaves no clean marker) — so at-rest SD-card rot is
still caught without blocking control. That background scan is cancellable:
`Close` interrupts it (`sqlite3_interrupt`) before closing the DB, so a restart
inside the scan window can't block `db.Close()` and the clean marker is reliably
written — without this, a redeploy/restart within the (minutes-long) scan window
would leave no marker and the next boot would be slow again. Startup phases are
now timed in the logs (`integrity gate complete elapsed=…`, `migrations complete
elapsed=…`) so a slow phase is visible instead of a silent gap. See
`docs/fast-restart.md`.
