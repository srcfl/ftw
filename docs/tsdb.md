# Time-series database

Reference for the long-format time-series storage used for per-driver diagnostics in FTW. The hot "recent" tier lives in SQLite; >14-day data rolls off into daily Parquet files under `cold/`.

## 1. Why long-format

The original wide-row history (`grid_w` / `pv_w` / `bat_w` / `load_w` / `bat_soc` + a JSON blob per timestamp) was a poor fit for per-driver diagnostics:

- Hover tooltips in the dashboard lost values after a page reload because the JSON blob wasn't normalised
- Exposing battery temperature, DC voltage or phase currents required a schema change every time a driver wanted to add a new signal
- Cross-driver comparison (e.g. "grid_hz as reported by Ferroamp vs Sungrow") was awkward

Long-format means one row per `(driver, metric, ts)` tuple. Drivers can register arbitrary new metrics at runtime via `host.emit_metric(name, value)` without touching Go code. See `go/internal/state/store.go` lines 133-154 for the on-disk migration and `go/internal/state/store_ts.go` for the write/read path.

## 2. Schema

The SQLite migration block (go/internal/state/store.go:138-154) creates three tables and one secondary index:

```sql
CREATE TABLE ts_drivers (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE ts_metrics (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    unit TEXT
);

CREATE TABLE ts_samples (
    driver_id INTEGER NOT NULL,
    metric_id INTEGER NOT NULL,
    ts_ms     INTEGER NOT NULL,
    value     REAL NOT NULL,
    PRIMARY KEY (driver_id, metric_id, ts_ms)
) WITHOUT ROWID, STRICT;

CREATE INDEX idx_ts_samples_ts ON ts_samples(ts_ms);
```

Why each design choice:

- **`WITHOUT ROWID`** -- no implicit rowid column; saves space and clusters storage on `(driver_id, metric_id, ts_ms)`, which matches the dominant access pattern "series for one metric over a time window"
- **`STRICT`** -- rejects type mismatches at write time; gives us SQLite's strongest type safety (no silent coercions from a bad driver)
- **Driver + metric names interned to integer ids** -- typical TS row is ~28 bytes payload plus B-tree overhead, vs ~80+ if names were repeated on every row. The intern caches live in memory and hydrate on first use -- see `hydrateIntern` in go/internal/state/store_ts.go:44
- **Composite PK = clustered access; secondary index on `ts_ms`** -- supports ad-hoc cross-metric scans (e.g. the Parquet roll-off, which reads everything older than cutoff in ts-order)

## 3. Storage estimate

At 5 s cadence:

```
5 s × 720/h × 24 h × 30 d × ~50 metrics ≈ 26M rows
26M × ~35 B ≈ 900 MB
```

A month at full resolution fits comfortably on a 32 GB SD card in a Pi. The recent tier is capped at 14 days (`RecentRetention` -- go/internal/state/store_ts.go:20) so the working set is more like 430 MB.

## 4. Write path

The path from driver to disk:

1. A Lua driver calls `host.emit_metric("inverter_temp_c", 43.6)`
2. The Lua host (go/internal/drivers/lua.go:211-216) forwards to `HostEnv.emitMetric` (go/internal/drivers/host.go:143-146), which calls `telemetry.Store.EmitMetric`
3. `telemetry.Store.EmitMetric` (go/internal/telemetry/store.go:215-221) appends a `MetricSample` to the in-memory `pending` slice
4. Standard `emit("pv"|"battery"|"meter", …)` calls auto-buffer the `<derType>_w` and `_soc` fields -- see `telemetry.Store.Update` (go/internal/telemetry/store.go:196-208). Raw, not smoothed values are stored; consumers smooth client-side
5. Once per control cycle (~5 s), the main loop drains the buffer via `tel.FlushSamples()` and forwards it to `state.Store.RecordSamples(stSamples)` (go/cmd/ftw/main.go:550-559)
6. `RecordSamples` pre-resolves driver/metric IDs **outside** the transaction, then opens a tx and bulk-inserts via a prepared `INSERT OR IGNORE`

### Deadlock note -- read this before changing RecordSamples

`state.Open` sets `SetMaxOpenConns(1)`. ID interning uses `s.db.Exec` to allocate a new id when it sees a name for the first time. If interning ran **inside** a transaction, that Exec would block forever waiting for a second connection.

The fix, documented at go/internal/state/store_ts.go:111-114:

> Pre-resolve all driver/metric IDs first, then run the tx using only `stmt.Exec`.

Don't move the `driverID` / `metricID` calls inside the tx. If you add a new write path, follow the same two-phase pattern.

## 5. Read path

Four Go functions back the HTTP API, all in `go/internal/state/store_ts.go`:

- `LoadSeries(driver, metric, sinceMs, untilMs, maxPoints) ([]Sample, error)` -- one metric over a time window. `maxPoints > 0` evenly downsamples the result
- `LatestSample(driver, metric) (Sample, error)` -- most recent value, or `sql.ErrNoRows`
- `MetricNames() ([]string, error)` -- the metric catalog
- `DriverNames() ([]string, error)` -- the driver catalog

The HTTP API (go/internal/api/api.go:123-124, 721-771) exposes:

- `GET /api/series?driver=&metric=&range=&points=` -- one metric over a time window. `range` accepts `1h`, `24h`, `7d`, etc.; `points` caps the row count
- `GET /api/series/catalog` -- list of registered drivers + metrics

For ranges that cross the 14-day boundary, callers can read from cold storage:

- `state.LoadSeriesFromParquet(coldDir, driver, metric, sinceMs, untilMs)` -- scans every daily Parquet file whose UTC day overlaps the window

## 6. Cold storage (Parquet)

Defined in `go/internal/state/parquet.go`:

- **Entrypoints**: `RolloffToParquet(ctx, coldDir)` and `LoadSeriesFromParquet(coldDir, driver, metric, since, until)`
- **File layout**: `<coldDir>/YYYY/MM/DD.parquet`. Default `coldDir` is `cold/` next to `state.db`, configurable via `state.cold_dir` in `config.yaml` (go/internal/config/config.go:148-156)
- **Schema** (column-oriented, via `parquetSampleRow` at go/internal/state/parquet.go:18-23):

  | column  | type    | encoding        |
  | ------- | ------- | --------------- |
  | ts_ms   | int64   | plain           |
  | driver  | string  | dictionary, zstd |
  | metric  | string  | dictionary, zstd |
  | value   | float64 | zstd            |

- **Compression**: zstd at default level. A daily file is typically 5-15 MB, which works out to ~100-200 MB/year
- **Idempotent + durable**: re-running a day merges into the existing file through a `.tmp` + fsync + atomic rename + directory fsync. No partial files even across power loss, and re-running never loses data — the durability matters because the SQLite rows are deleted right after the write returns
- **Streamed**: the rolloff flushes one UTC day at a time (samples stream out of SQLite in ts order), so peak memory is one day (~30 MB) regardless of backlog size
- **Triggered** by the hourly `rolloffLoop` goroutine in `go/cmd/ftw/main.go`, with one run at startup so a fresh boot catches any backlog. After durable writes, rolled rows are deleted from `ts_samples` in a single `DELETE WHERE ts_ms < cutoff`. The same loop then runs a truncating WAL checkpoint, applies `state.cold_retention_days` (deletes expired day files; 0 = keep forever), and warns when free disk is below 500 MB
- **Compaction**: SQLite never shrinks its file on its own; `state.Open` runs a one-time `VACUUM` at boot when the freelist is both >64 MB and >20% of the file (typically the first boot after a previously unpruned DB got pruned)

## 7. Driver-side API

The only contract a driver needs to know:

```lua
function driver_poll()
    -- ... read modbus or mqtt ...
    host.emit("battery", { w = bat_w, soc = bat_soc })
    -- Diagnostics: long-format TS DB, no schema migration needed
    host.emit_metric("battery_dc_v", bat_v)
    host.emit_metric("battery_dc_a", bat_a)
    host.emit_metric("inverter_temp_c", heatsink_c)
    host.emit_metric("grid_hz", hz)
end
```

`host.emit_metric(name, value)`:

- `name` -- snake_case identifier with the unit as a suffix. Existing conventions: `_w` (power), `_v` (voltage), `_a` (current), `_hz` (frequency), `_c` (Celsius), `_pct` (percent), `_kwh` / `_wh` (energy)
- `value` -- any Lua number. Buffered in process memory and flushed to SQLite once per control cycle

The standard `host.emit("pv"|"battery"|"meter", { w=…, soc=… })` call auto-records `<type>_w` and `<type>_soc` (when present) into the same long-format table. Drivers don't need to duplicate those with `emit_metric`.

## 8. Existing metrics

Emitted by the bundled drivers:

**Sungrow** (`drivers/sungrow.lua`):

- `pv_mppt1_v`, `pv_mppt1_a`, `pv_mppt2_v`, `pv_mppt2_a`
- `inverter_temp_c`, `grid_hz`
- `battery_dc_v`, `battery_dc_a`
- `meter_l1_w`, `meter_l2_w`, `meter_l3_w`, `meter_l1_v`, `meter_l2_v`, `meter_l3_v`, `meter_l1_a`, `meter_l2_a`, `meter_l3_a`

**Ferroamp** (`drivers/ferroamp.lua`):

- `meter_l1_w`, `meter_l2_w`, `meter_l3_w`, `meter_l1_v`, `meter_l2_v`, `meter_l3_v`, `meter_l1_a`, `meter_l2_a`, `meter_l3_a`
- `grid_hz`
- `battery_dc_v`, `battery_dc_a`

**Auto-buffered by `host.emit(type, …)`** for both drivers:

- `pv_w`, `battery_w`, `meter_w`
- `battery_soc`

## 9. Retention + tradeoffs

| Tier | Storage | Resolution | Window |
| ---- | ------- | ---------- | ------ |
| recent | SQLite `ts_samples` | full 5 s | ≤14 days |
| cold | Parquet daily files | full 5 s (preserved) | >14 days |

Both tiers keep full resolution -- the roll-off is a storage-format change, not a downsampling step. The 14-day cutoff is `RecentRetention` (go/internal/state/store_ts.go:20); bump it if you want a larger hot window at the cost of SQLite bloat.

The **legacy wide-row tables** (`history_hot` / `history_warm` / `history_cold`, declared in go/internal/state/store.go:84-102) remain in place for the existing dashboard wide-snapshot view. They age in place via `state.Store.Prune` (hot → 15-min warm buckets → daily cold buckets). The new long-format TSDB runs alongside and is the future canonical path -- don't add new signals to the legacy tables.

## 10. Common operations

```bash
# What metrics are flowing?
curl localhost:8080/api/series/catalog

# Pull one series for the last hour
curl 'localhost:8080/api/series?driver=sungrow&metric=inverter_temp_c&range=1h'

# Downsample a 7-day window to 600 points
curl 'localhost:8080/api/series?driver=ferroamp&metric=battery_dc_v&range=7d&points=600'

# Roll-off runs hourly automatically; inspect via service log
journalctl -u ftw | grep "parquet rolloff"
```

Related docs: [host-api.md](host-api.md), [writing-a-driver.md](writing-a-driver.md), [configuration.md](configuration.md)
