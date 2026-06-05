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

- **Skip-on-clean-shutdown.** `Store.Close` drops a `<db>.clean` marker on a
  clean close; `openChecked` consumes it and skips the boot `quick_check`. On the
  1.3 GB field DB this took the integrity gate from **>5 min → 48 ms** (measured).
  The marker is single-use, so a crash before the next clean `Close` forces a
  real check next boot.
- **Background verify keeps the safety net.** `Store.VerifyInBackground` runs the
  full check *off* the hot path after the app is serving. On detected corruption
  it arms a heal for the next boot (leaves no clean marker) — so at-rest SD-card
  rot is still caught, control just isn't held hostage to it. Recovery becomes
  two boots instead of one, and never blocks.
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
