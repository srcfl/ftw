# state — SQLite-backed persistence for config, events, history, TS, devices, prices

## What it does

Opens one SQLite file (WAL journal, small connection pool with `busy_timeout=5000`) and runs all migrations on `Open`. Owns the tiered history (hot → warm → cold via pure SQL aggregation in `Prune`), the long-format time-series tables (`ts_drivers` / `ts_metrics` / `ts_samples`), the `devices` table that anchors hardware identity, and daily Parquet roll-off to `<dataDir>/cold/YYYY/MM/DD.parquet`. Every stored W follows the site convention (see `../../../docs/site-convention.md`).

## Key types

| Type | Purpose |
|---|---|
| `Store` | Wrapper around `*sql.DB`. One per site. |
| `Device` | Hardware-stable identity (`make:serial` / `mac:` / `ep:`). |
| `HistoryPoint` | One tick of grid/pv/bat/load/soc + raw JSON. |
| `Sample` | Long-format `(driver, metric, ts_ms, value)` row. |
| `PricePoint` | One spot-price slot (variable duration). |
| `ForecastPoint` | One weather / PV forecast slot. |
| `Event` | Timestamped operational log entry. |

## Public API surface

- `Open(path) / Close()` — open DB and run migrations; idempotent close.
- `SaveConfig / LoadConfig` — small k/v store for runtime-tunables.
- `RecordEvent / RecentEvents` — append-only op log, ms-keyed.
- `SaveTelemetry / LoadTelemetry` — last-known JSON per DER key (crash recovery).
- `SaveBatteryModel / LoadAllBatteryModels / DeleteBatteryModel / MigrateBatteryModelKeys` — model state keyed by `device_id` (falls back to driver name cold-start).
- `RegisterDevice / LookupDeviceByDriverName / AllDevices` + `ResolveDeviceID` — identity layer that keeps trained state surviving renames.
- `RecordHistory / LoadHistory / HistoryCounts / Prune` — tiered history; `Prune` ages hot→warm (15 min buckets) and warm→cold (1 day buckets) in one transaction.
- `RecordSamples / LoadSeries / LatestSample / MetricNames / DriverNames / PruneRecent / SamplesBefore` — long-format TS with interned driver/metric IDs.
- `RolloffToParquet / LoadSeriesFromParquet` — 14-day-old samples roll off to daily Parquet files, sorted, zstd-compressed.
- `SavePrices / LoadPrices / SaveForecasts / LoadForecasts` — market and weather data slots.

## How it talks to neighbors

**Leaf package.** No imports of other internal packages — `state` is the bottom of the dependency graph. Consumers: `api`, `mpc`, `pvmodel`, `loadmodel`, `prices`, `priceforecast`, `forecast`, `currency`, `battery` (models load/save), and `main.go` for wiring. `telemetry` does NOT import `state` — the control loop drains the telemetry buffer and forwards to `state.RecordSamples` itself.

## What to read first

`store.go` — the migrations at the top are the schema truth; then read `store_ts.go` for the long-format pipeline and `devices.go` for identity resolution. `parquet.go` is only interesting if you touch cold storage.

## What NOT to do

- **Do NOT run multi-second read queries on the same path as writers without thinking.** The pool is small (`SetMaxOpenConns(4)`, `busy_timeout=5000` in store.go:39) — multiple readers run in parallel under WAL, but writes still serialize and a long-running write blocks every other writer for up to 5 s before SQLITE_BUSY surfaces. New analytical queries (the savings/cost-breakdown family) belong in read-only paths that don't sit behind the control-loop's `RecordHistory`/`RecordSamples` writers; `LoadAllBatteryModels` (store.go:286+) shows the pattern of pre-resolving lookups before opening the main scan, which keeps each query short.
- **Do NOT key battery models on driver name in new code.** Keys go through `batteryModelKey` (store.go:317) which prefers `device_id`; a driver rename would otherwise orphan the trained state.
- **Do NOT bypass the interner.** New TS writers must call `RecordSamples`, not insert into `ts_samples` directly — the `(driver_id, metric_id, ts_ms)` PK depends on the intern tables.
- **Do NOT forget site convention.** `HistoryPoint.BatW` is + for charge, − for discharge; same for every W column. If a sign flip is needed, it happens at the driver boundary, never here.
- **Do NOT assume hourly price slots.** `PricePoint.SlotLenMin` varies (NordPool 15 min since 2025, ENTSOE mixed) — honor it in plots and aggregations.
