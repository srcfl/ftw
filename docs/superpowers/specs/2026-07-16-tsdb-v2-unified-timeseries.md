# TSDB v2 — unified time-series pipeline

- **Status:** Draft for review — no implementation started
- **Date:** 2026-07-16
- **Driver:** DB growth strategy ahead of fleet expansion + the 2026-07-16
  upgrade-lock incident (see below)
- **Depends on:** the bounded-maintenance hotfix (PR #566) shipping first
- **Decision needed from:** Fredrik (scope + priority vs other workstreams)

## Why now

Three findings from measuring a real production database (93 days of
operation, 2.35 GB file):

| Store | Measured | Nature |
|---|---|---|
| `ts_samples` (long-format metrics) | 25.2 M rows ≈ 1.1 GB, spans exactly 14 days | Bounded working set — *by design*, not bloat |
| `history_hot` (wide dashboard rows) | 2.13 M rows ≈ 450 MB, spans 93 days | Unbounded until v0.129.0 wired `Prune()` |
| `planner_diagnostics` | 5 789 rows ≈ 485 MB (~85 kB JSON/row) | Retention existed but rows are enormous; inflated every snapshot to ~470 MB |

Conclusions:

1. **The architecture is right.** SQLite (WAL) as the recent tier + daily
   zstd Parquet as the cold tier fits the constraints exactly: one writer,
   local-first, no CGo / single static binary, SD-card endurance. No external
   TSDB (VictoriaMetrics, Influx, Timescale) survives the single-container /
   HA-add-on requirement, and DuckDB requires CGo.
2. **The implementation is fragmented.** We run three parallel time-series
   systems — wide `history_hot/warm/cold` tiers, long-format `ts_samples`,
   and `planner_diagnostics` — each with its own retention, its own rolloff,
   its own failure modes, and partially overlapping data. The incident lived
   precisely in that fragmentation: `Prune()` belonged to the wide tiers only,
   was never wired, and when wired it locked the DB for hours.
3. **Volume is dominated by design choices, not waste**: raw 2 s cadence ×
   ~40 metrics × 14 days, and one ~85 kB planner snapshot per replan. Growth
   strategy = explicit budgets, not heroic compression.

## Goals

- **One ingestion pipeline, one retention engine.** Every time-series fact
  flows through the long-format store. The wide history tiers disappear as
  *tables* and survive only as *queries*.
- **Charts never scan raw data.** Stored, incrementally-maintained
  aggregates serve every zoomed-out view.
- **Disk budget as the contract.** One knob (`state.disk_budget_mb`) drives
  every retention decision; the system reports whether it can honor the
  budget instead of silently growing.
- **Maintenance is bounded and observable.** No maintenance transaction may
  exceed ~1 s of lock time; every job logs its result and surfaces in
  `/api/health`.

## Non-goals

- No external database or sidecar process.
- No change to the driver-facing API (`host.emit_metric`, `host.emit`) —
  drivers must not notice.
- No change to the site sign convention or `device_id` identity model.
- Cold Parquet format stays as-is (schema already proven; readers exist).

## Design

### 1. Single source of truth: `ts_samples`

The control loop already emits grid/pv/bat/load/soc per driver into
`ts_samples` (via `host.emit` structured telemetry) *and* writes the same
values into `history_hot` with a per-driver JSON blob. v2 keeps only the
long-format write. The site-level dashboard series (`grid_w`, `pv_w`,
`bat_w`, `load_w`, `bat_soc`) are written as first-class metrics under a
reserved `site` driver name, replacing `HistoryPoint`.

The JSON blob's remaining consumers (per-driver breakdown in the live
chart) read the per-driver metrics that already exist in `ts_samples`.

### 2. Stored continuous aggregates

Two aggregate tables, maintained incrementally at write time (cheap: the
control loop already batches per tick) or by a minute-cadence sweeper:

```
ts_agg_5m (driver_id, metric_id, bucket_ts, avg, min, max, n)   -- 90 d retention
ts_agg_1h (driver_id, metric_id, bucket_ts, avg, min, max, n)   -- 5 y retention
```

- `LoadSeriesBuckets` picks raw / 5 m / 1 h automatically from the request
  resolution — same API, no UI change.
- The warm/cold history tiers map 1:1 onto these (15-min warm ≈ 5 m agg,
  daily cold ≈ 1 h agg rolled up at query time), so `LoadHistory` becomes a
  view over aggregates + `energy_daily` stays as-is.
- Aggregates are *derived* data: excluded from snapshots, rebuildable from
  raw + Parquet.

### 3. Budget-driven retention

```yaml
state:
  disk_budget_mb: 1500   # default; 0 = unbounded (current behavior)
```

Priority order when the budget is exceeded (evict first):

1. Raw `ts_samples` beyond a floor of 48 h (rolls to Parquet earlier than
   the 14-day default — the cold fall-through in `/api/series` makes this
   invisible to users)
2. Cold Parquet days beyond `cold_retention_days` (already implemented)
3. `ts_agg_5m` beyond 90 d
4. Diagnostics beyond 48 h (Parquet keeps all)

The budget check runs in the hourly maintenance loop; `/api/health.storage`
gains `budget_mb`, `used_mb`, `headroom_mb`, and a `boundable: false` flag
when even maximal eviction cannot meet the budget (that is an operator
alarm, wired to the notification system).

### 4. Maintenance discipline (rules, enforced by review + tests)

- One maintenance goroutine; jobs run sequentially, never concurrently.
- Every job works in bounded transactions (≤ ~1 s of lock time; the chunk
  helpers from PR #566 are the template).
- Every job logs a completion line with counts + duration, and failure
  states surface in `/api/health`.
- Every job has a volume test: seeded at realistic row counts *with a
  concurrent writer asserting zero failed writes* (see
  `TestPruneLargeBacklogWithConcurrentWriter`).

### 5. Migration

Ships behind one boot-time migration, tested against a copy of a real
production DB (not synthetic 20-row fixtures — that lesson is paid for):

1. v2 schema created alongside v1; new writes go to v2 immediately.
2. Backfill `ts_agg_*` from existing `ts_samples` + Parquet in chunked
   background batches (same bounded-transaction rules).
3. `history_hot/warm/cold` aged into aggregates via the (now linear) prune,
   then the tables are dropped.
4. One-time `VACUUM` via the existing `CompactIfBloated` boot path.
5. Rollback: v1 tables are not dropped until the release *after* v2 ships
   clean fleet telemetry (mirrors ADR-0003's rollout caution).

## Sizing (target steady state, current cadence)

| Store | Today | v2 target |
|---|---|---|
| Raw `ts_samples` | ~1.1 GB (14 d) | ~550 MB (7 d default) |
| Wide history tiers | ~450 MB unpruned | 0 (dropped) |
| Aggregates 5 m + 1 h | — | ~120 MB (90 d) + ~30 MB (5 y) |
| Diagnostics | 485 MB (30 d) | ~115 MB (7 d, post-hotfix) → 48 h under budget pressure |
| **SQLite total** | **2.35 GB** | **~0.8 GB** (headroom under a 1.5 GB budget) |
| Snapshots | ~470 MB | ~20 MB (post-hotfix already) |

## Open questions (for review)

1. Aggregate maintenance at write time vs. minute-sweeper — write-time is
   simpler and the tick already batches, but adds ~2× write amplification
   on the hot path. Sweeper preferred?
2. Reserved `site` driver name vs. a separate `site_series` table for the
   dashboard metrics — reserved name keeps one pipeline but leaks into the
   catalog API.
3. Is 48 h the right raw floor under budget pressure, given self-tune and
   battery-model training read recent raw series?
4. Do we fold `events` / `notification_log` retention into the same budget
   engine (tiny today, but "one engine" argues yes)?

## Relationship to the 2026-07-16 incident

The hotfix (PR #566) makes v1 maintenance safe; this spec removes the
fragmentation that made the incident possible. Sequencing: hotfix ships and
soaks on the fleet first; v2 implementation starts only after spec review.
