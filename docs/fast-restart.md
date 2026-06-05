# Fast, trustworthy restarts

A home energy system that takes minutes to come back after a restart reads as
*broken*. Users lose trust fast when a restart looks like a hang — so startup
time is a product property, not just an ops detail. This note captures the
principles and the concrete work (shipped + planned).

## The incident that motivated this

On a field Pi with a **1.3 GB `state.db`**, upgrading from `0.109.0` to a newer
build hung at startup for >5 minutes. Root cause: `heal.go` added a boot-time
`PRAGMA quick_check` (new since `0.109.0`) that scans the *entire* file. On a
multi-GB DB that is minutes of disk I/O — and it ran on **every** boot, not just
the upgrade. The old build had no such check, so it booted in ~5 s; the new one
looked dead.

## Principles

1. **Control-first.** The control loop + API must come up in seconds. Nothing
   that scales with DB size may block the path to "serving". Heavy DB work is
   lazy or backgrounded.
2. **Observable, never silent.** Every startup phase is timed and logged. A slow
   phase must look like *progress*, not a freeze — to both an operator reading
   logs and a user watching a screen.
3. **Only check when there's reason to.** Like `fsck`, the integrity check
   belongs after an *unclean* shutdown, not on every boot.
4. **Keep the database small.** The cheapest fast check is the one with little to
   scan. A lean SQLite file makes every startup cost (check, migrate, snapshot)
   small by construction.

## Shipped (this change)

- **A persistent "verified-good" marker.** `Open` arms a `<db>.clean` marker once
  `state.db` has opened + migrated successfully; `openChecked` skips the boot
  `quick_check` whenever the marker is present. On the 1.3 GB field DB this took
  the integrity gate from **>5 min → ~40 ms** (measured).
  - **It is NOT a clean-shutdown flag — and that's deliberate.** The first design
    wrote the marker in `Close` and consumed it on boot. That tied fast restarts
    to a clean exit, and it broke immediately: the background scan (below) holds
    the DB busy for minutes, so a redeploy inside that window got the close
    SIGKILLed before the marker was written → the next boot was slow again. The
    marker now *persists* across both clean shutdowns and crashes (a crash doesn't
    corrupt a WAL DB, so it must never force a slow re-scan). Nothing at shutdown
    touches it.
- **Background verify is the one thing that removes it.**
  `Store.VerifyInBackground` runs the full check *off* the hot path after the app
  is serving. On a clean result the marker stays; on detected corruption it
  removes the marker, so the *next* boot runs the full check and heals from
  snapshot. At-rest SD-card rot is still caught — recovery just takes the next
  boot instead of blocking this one. The scan is cancellable (`Close` interrupts
  it via `sqlite3_interrupt`) so a shutdown is never blocked by an in-flight
  multi-minute scan, and a cancelled scan is treated as "didn't finish", never as
  corruption.
- **Phase-timed startup logs.** `state: integrity gate complete elapsed=…` and
  `state: migrations complete elapsed=…` make any slow phase visible instead of a
  silent gap after `config loaded`.

## Planned follow-ups (the real "make it better")

- **Investigate the 1.3 GB DB — the root smell.** If history rolled off to
  Parquet as designed, `state.db` would be ~100 MB and *none* of this would
  matter (`quick_check` would be sub-second). Verify: is `rolloffLoop` actually
  keeping `ts_samples` ≤ 14 days? Are `history_hot/warm/cold` pruned? A 1.3 GB
  file on an SD card is also a latent risk beyond startup — slower queries,
  slower snapshots, more surface to corrupt. Likely action: confirm rolloff
  health, then a one-time `VACUUM`. *(Tracking issue to open.)*
- **Surface startup phase in `/api/health`.** Report `starting | migrating |
  ready` so the dashboard (and the home-route "Reaching home…" screen) can show
  an honest "starting up…" state instead of looking dead. This is the
  user-facing half of "observable, never silent".
- **Restart-budget test.** Seed a large DB in CI and assert
  cold-start-to-serving < N seconds, so "fast restart" is a guaranteed invariant,
  not luck.
- **Migrations stay cheap by rule.** Additive only (`CREATE … IF NOT EXISTS`,
  `ALTER ADD COLUMN` — both O(1)). Never a table rewrite at boot. Any future
  migration that must touch GB-scale data runs in the background with progress
  logging, never on the control path. (`migrateLegacyTierSplit` is fine — it
  only moves the small `prices`/`forecasts` tables.)
