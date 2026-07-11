# Resilient storage: tiering + auto-heal

**Date:** 2026-06-03
**Status:** Approved design → implementation
**Branch:** `feat/storage-tiering-autoheal`

## Problem

A field tester's spot prices silently stopped working. Root cause: the
shared `state.db` SQLite file was corrupt — `price save failed err="database
disk image is malformed (11)"` (SQLITE_CORRUPT). The fetch worked; the *save*
failed. Classic Raspberry Pi SD-card corruption (power loss / wear).

Two failures compounded:

1. **No blast-radius isolation.** Disposable, re-fetchable data (prices) lives
   in the same file as precious, hard-to-recreate data (trained battery/PV/load
   models, energy history, device identity). One corrupt file took down
   everything that writes.
2. **Silent failure.** Corruption produced only `WARN` logs and a blank
   dashboard. Nothing surfaced to `/api/health` or the UI. Debugging took hours.

This violates the "Robust Over Feature-Rich" principle: the core persistence
layer fails silently and totally.

## Goals

- A corrupt DB **self-heals** at boot instead of failing silently forever.
- **Disposable** data (re-fetchable from the network) is isolated so its
  corruption (or a deliberate flush) never risks precious data, and recovery is
  "rebuild empty → re-fetch" with zero data loss.
- **Precious** data is protected by a periodic local snapshot it can be
  restored from.
- Any corruption / recovery event is **loudly surfaced** to `/api/health` and
  the dashboard. Never silent again.

## Non-goals

- Defending against a physically dying SD card (hardware — we detect, alert,
  and degrade gracefully, but can't prevent media failure).
- In-process `.recover` salvage of a corrupt `state.db` (the `modernc.org/sqlite`
  driver has no `.recover`; we use snapshot-restore instead — deterministic and
  simpler).
- Changing the public `state.Store` API. Callers stay untouched.

## Architecture

### Two SQLite files

| File | Contents | On corruption |
|---|---|---|
| **`state.db`** (precious) | everything except prices/forecasts: `config` (kv — incl. FX + price-twin model), `events`, `telemetry`, `battery_models`, `history_hot/warm/cold`, `ts_*`, `devices`, `planner_diagnostics`, `notification_log`, `nova_ders`, `energy_daily`, `trusted_devices` | restore from latest snapshot; else quarantine + fresh + **loud alert** |
| **`cache.db`** (disposable) | `prices`, `forecasts` | quarantine + rebuild empty; re-fetch repopulates within the hour |

`cache.db` lives next to `state.db` (e.g. `state.path` = `/data/state.db` →
`/data/cache.db`).

### Store holds both handles

`state.Store` gains a second `*sql.DB`:

```go
type Store struct {
    db    *sql.DB // precious — state.db
    cache *sql.DB // disposable — cache.db
    ts    *internCache
}
```

The public API is unchanged. Internally, the methods/queries that touch the
disposable tables route to `s.cache` instead of `s.db`. The complete set
(verified — all standalone, **no cross-table SQL JOINs**, so the split is clean):

- `SavePrices`, `LoadPrices` (`store.go`)
- `SaveForecasts`, `LoadForecasts` (`store.go`)
- `loadPriceSlotsForRange` (`cost.go:128`)
- `avgSlotPricesForRange` (`cost.go:283`)

Cost calculations already load price slots separately and correlate with energy
samples in Go — there is no SQL JOIN between `prices` and any precious table, so
nothing breaks across the file boundary.

### FX stays in `state.db`

FX rates persist in the shared `config` kv table (`currency.go:231` via
`SaveConfig`), not their own table. Moving them to `cache.db` would require
splitting the kv table or adding a dedicated `fx_rates` table + migration — much
work for negligible benefit: FX is a tiny daily-refetched blob already covered
by the `state.db` snapshot. Decision: **FX stays in `state.db` kv.** (Revisit
only if we later want FX fully disposable.)

## Boot-time integrity + auto-heal

A new helper opens a single DB file with the integrity gate:

```
openChecked(path, tier) -> (*sql.DB, healEvent, error)
  1. open with the existing pragmas (WAL, synchronous(NORMAL), busy_timeout)
  2. run `PRAGMA quick_check` (fast; full-scan only on suspicion)
  3. if result == "ok": return (db, nil)
  4. else (corrupt):
       cache tier  -> close, quarantine-rename file+`-wal`+`-shm`
                      to `<file>.corrupt-<unixMs>`, reopen fresh -> rebuilt
       state tier  -> close, if `<file>.snapshot` exists AND passes quick_check:
                          move corrupt aside, copy snapshot into place, reopen -> restored
                      else: quarantine + reopen fresh -> rebuilt
       return (db, event)   // event drives health surfacing
```

`Open(statePath)` orchestrates: derive `cachePath`, call `openChecked` for each,
run migrations on both, stash any `healEvent`(s) on the Store for the health
endpoint to read.

The heal helper takes the current timestamp as a parameter (used in
quarantine filenames like `<file>.corrupt-<unixMs>`) so tests can assert exact
names; `Open` passes `time.Now().UnixMilli()`.

## Snapshot of state.db (recovery source)

Reuse the existing `SnapshotTo(dstPath)` (`store.go` — VACUUM-INTO-style ATTACH
+ schema replay, already excludes bulky `ts_*` via `snapshotExcludedTables`,
~2 s / ~50 MB).

- A background loop snapshots `state.db` to `state.db.snapshot` **daily** and on
  graceful shutdown. Atomic: write to `state.db.snapshot.tmp`, `quick_check` it,
  then rename over `state.db.snapshot`.
- **Cadence rationale — daily, not 6 h.** The SD card is the failing component;
  each snapshot writes ~50 MB. Daily minimises write-wear while bounding worst-
  case loss to one day of *precious* data (models retrain; energy history loses
  at most a day). `cache.db` gets **no** snapshot — it's re-fetchable.
- Wired in `main.go` next to the existing `rolloffLoop`.

## Health surfacing

Extend `GET /api/health` with a `storage` object:

```json
"storage": {
  "state": "ok",                  // ok | restored | rebuilt
  "cache": "rebuilt",             // ok | rebuilt
  "last_event_ms": 1717430000000,
  "detail": "cache.db was corrupt — rebuilt empty, re-fetching prices"
}
```

The dashboard shows a banner when `state` or `cache` ≠ `ok` for this boot. This
is the single highest-leverage change: it turns "silent for hours" into
"obvious in seconds."

## Migration for existing installs

On first boot of the new version (single `state.db` with `prices`/`forecasts`
inside):

1. Open `state.db`; create/open `cache.db`.
2. If `cache.db` has no `prices` rows and `state.db` does: copy
   `prices` + `forecasts` rows `state.db` → `cache.db` (best-effort; on any read
   error, skip — data is re-fetchable).
3. `DROP TABLE` the now-duplicated `prices`/`forecasts` in `state.db` so there's
   one source of truth and the snapshot shrinks.

New installs simply create both files with the split schema. Migration is
idempotent and safe to re-run.

## Error handling

- `quick_check` failure (not "ok") is the corruption signal; a hard open error
  (file unreadable) is treated as corruption for the same heal path.
- If snapshot-restore itself fails, fall through to quarantine + fresh — never
  leave the process unable to boot.
- All heal paths log at `INFO`/`WARN` **and** record a `healEvent` for health.

## Testing (TDD)

Failing-test-first, per package:

- **Corruption detection:** write a valid DB, truncate / overwrite header bytes,
  assert `openChecked` reports corrupt and rebuilds (cache) / restores (state).
- **Cache rebuild:** corrupt `cache.db`, reopen, assert empty + usable + heal
  event = `rebuilt`.
- **State restore:** snapshot a populated `state.db`, corrupt the live file,
  reopen, assert rows restored from snapshot + event = `restored`.
- **State fresh fallback:** corrupt `state.db` with no snapshot, assert fresh +
  event = `rebuilt` + loud.
- **Routing:** assert `SavePrices`/`LoadPrices`/forecasts hit `cache.db` (e.g.
  corrupt `state.db` after boot, prices still save).
- **Migration:** seed legacy single-DB with prices/forecasts, boot, assert rows
  moved to `cache.db` and dropped from `state.db`.
- **Health:** assert `/api/health` `storage` reflects each event.
- **No-CGo / pure-Go** SQLite throughout; no new deps.

## Conflict awareness

- master is stable at `bba0d1a` (the ENTSO merge).
- Open **PR #414** (home-route passkey) is the only open PR touching
  `store.go`, and only `migrate()` near line 458+ (adds `addColumnIfMissing`).
  It does **not** touch `Open()` or the `Store` struct — where this work lives.
  Keep new migration steps append-only; if #414 lands first, the rebase is
  trivial.

## Rollout

- Changeset: **minor** (resilience feature; new `/api/health` field; new
  `cache.db` file). No breaking API change.
- Backwards compatible: old `state.db` auto-migrates; downgrade would leave a
  `cache.db` the old binary ignores (prices simply wouldn't load — acceptable,
  and we don't support downgrade).
