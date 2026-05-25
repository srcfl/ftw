// Package state is SQLite-backed persistent storage for config overrides,
// event log, history snapshots, and battery models.
//
// History uses one table per tier (hot/warm/cold) like the Rust version, but
// the aggregation from hot → warm → cold is pure SQL instead of custom
// bucketing code. See Prune() for the aggregation queries.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// HotRetention = 30 days at 5s resolution
	HotRetention = 30 * 24 * time.Hour
	// WarmRetention = 12 months at 15-min buckets
	WarmRetention = 365 * 24 * time.Hour
	// WarmBucketMS = 15-minute bucket size for warm tier
	WarmBucketMS = 15 * 60 * 1000
	// ColdBucketMS = daily bucket size for cold tier
	ColdBucketMS = 24 * 60 * 60 * 1000
)

// Store is the persistent state DB (thin wrapper around *sql.DB).
type Store struct {
	db *sql.DB
	ts *internCache
}

// Open initializes (or creates) the DB at path. Runs all migrations.
func Open(path string) (*Store, error) {
	// busy_timeout(5000) is mandatory once we allow >1 connection — without
	// it, concurrent writers race for the WAL lock and SQLITE_BUSY surfaces
	// to callers immediately. With it, contenders wait up to 5 s for the
	// lock, which is well within HTTP request budgets and longer than any
	// real write batch in this codebase.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// WAL mode allows many concurrent readers + one writer. With a single
	// connection any expensive read (e.g. DailyCostBreakdown) serializes
	// the entire app on the DB mutex, which is what produced the post-v0.76
	// "dashboard locked up + control loop stalled" reports. A small pool
	// lets reads run in parallel while writers still queue safely behind
	// busy_timeout.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	s := &Store{db: db, ts: newInternCache()}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the DB file. Safe to call multiple times.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SnapshotTo writes a self-contained, defragmented copy of the database
// to dstPath using SQLite's VACUUM INTO. The source DB remains open for
// the duration; readers and writers continue unimpeded. Used by the
// self-update flow to capture state before pulling a new image, so
// operators can roll back if the new version misbehaves.
//
// dstPath must not exist — SQLite refuses to overwrite an existing file.
// Safe to call while the Store is serving live traffic.
func (s *Store) SnapshotTo(dstPath string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: snapshot on nil store")
	}
	// VACUUM INTO takes a literal path string — bind parameters aren't
	// honoured by the parser. We construct dstPath ourselves (timestamped
	// snapshots dir), but escape single quotes defensively so a caller
	// passing an unexpected path can't inject SQL.
	escaped := strings.ReplaceAll(dstPath, "'", "''")
	if _, err := s.db.Exec(fmt.Sprintf("VACUUM INTO '%s'", escaped)); err != nil {
		return fmt.Errorf("snapshot to %s: %w", dstPath, err)
	}
	return nil
}

func (s *Store) migrate() error {
	stmts := []string{
		// config: small string key-value for mode, grid_target etc.
		`CREATE TABLE IF NOT EXISTS config (
			key TEXT PRIMARY KEY NOT NULL,
			value TEXT NOT NULL
		)`,
		// events: operational log, ms-precision key (seconds collided)
		`CREATE TABLE IF NOT EXISTS events (
			ts_ms INTEGER PRIMARY KEY NOT NULL,
			event TEXT NOT NULL
		)`,
		// telemetry snapshots for crash recovery
		`CREATE TABLE IF NOT EXISTS telemetry (
			key TEXT PRIMARY KEY NOT NULL,
			json TEXT NOT NULL
		)`,
		// battery models (JSON-serialized), keyed by driver name
		`CREATE TABLE IF NOT EXISTS battery_models (
			name TEXT PRIMARY KEY NOT NULL,
			json TEXT NOT NULL
		)`,
		// History tiers — hot/warm/cold, all keyed by ms timestamp
		`CREATE TABLE IF NOT EXISTS history_hot (
			ts_ms INTEGER PRIMARY KEY NOT NULL,
			grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL,
			json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS history_warm (
			ts_ms INTEGER PRIMARY KEY NOT NULL,
			grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL,
			json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS history_cold (
			ts_ms INTEGER PRIMARY KEY NOT NULL,
			grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL,
			json TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_hot_ts ON history_hot(ts_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_warm_ts ON history_warm(ts_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_cold_ts ON history_cold(ts_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts_ms DESC)`,

		// Spot prices — one row per time slot per zone.
		// Slot duration is provider-dependent: NordPool went to 15-min PTU
		// in late 2025; ENTSOE is mixed. The table just stores timestamps —
		// slot_len_min tells consumers what duration each row represents.
		`CREATE TABLE IF NOT EXISTS prices (
			zone TEXT NOT NULL,
			slot_ts_ms INTEGER NOT NULL,
			slot_len_min INTEGER NOT NULL DEFAULT 60,
			spot_ore_kwh REAL NOT NULL,
			total_ore_kwh REAL NOT NULL,
			source TEXT NOT NULL,
			fetched_at_ms INTEGER NOT NULL,
			PRIMARY KEY (zone, slot_ts_ms)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_prices_slot ON prices(slot_ts_ms)`,

		// Weather + PV forecasts — one row per hour (met.no/openweather
		// both default to hourly; can downsample to 15-min if needed later).
		`CREATE TABLE IF NOT EXISTS forecasts (
			slot_ts_ms INTEGER PRIMARY KEY,
			slot_len_min INTEGER NOT NULL DEFAULT 60,
			cloud_cover_pct REAL,
			temp_c REAL,
			solar_wm2 REAL,
			pv_w_estimated REAL,
			source TEXT NOT NULL,
			fetched_at_ms INTEGER NOT NULL
		)`,

		// ---- Long-format time-series ("recent" tier, last 14 days) ----
		// Drivers + metrics are interned to integer ids to keep rows small.
		// Composite PK is (driver_id, metric_id, ts) WITHOUT ROWID so storage
		// is clustered by driver+metric — typical access pattern is "give me
		// metric X for driver Y over time range Z".
		`CREATE TABLE IF NOT EXISTS ts_drivers (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE
		)`,
		`CREATE TABLE IF NOT EXISTS ts_metrics (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			unit TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS ts_samples (
			driver_id INTEGER NOT NULL,
			metric_id INTEGER NOT NULL,
			ts_ms     INTEGER NOT NULL,
			value     REAL NOT NULL,
			PRIMARY KEY (driver_id, metric_id, ts_ms)
		) WITHOUT ROWID, STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_ts_samples_ts ON ts_samples(ts_ms)`,

		// ---- Devices: hardware-stable identity for each driver ----
		// device_id resolution priority:
		//   1. make + ":" + serial          (canonical, set via host.set_sn)
		//   2. "mac:" + arp_lookup(host)    (L2-stable for TCP devices)
		//   3. "ep:" + protocol + ":" + endpoint  (last resort)
		// Persisted state (battery_models, etc.) is keyed on device_id, so
		// renaming a driver in config or removing/re-adding it doesn't
		// orphan the trained model.
		`CREATE TABLE IF NOT EXISTS devices (
			device_id     TEXT PRIMARY KEY NOT NULL,
			driver_name   TEXT NOT NULL,
			make          TEXT,
			serial        TEXT,
			mac           TEXT,
			endpoint      TEXT,
			first_seen_ms INTEGER NOT NULL,
			last_seen_ms  INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_name ON devices(driver_name)`,

		// ---- Planner diagnostics (one snapshot per replan) ----
		// Persists the full structured output of mpc.Service.Diagnose() so
		// operators can time-travel: load any past moment and see what the
		// DP saw + what it decided + why. Denormalized total_cost_ore +
		// horizon_slots so the timeline UI can render summary rows without
		// unmarshalling every JSON blob.
		//
		// Retention: DiagnosticsRecentRetention (30 d) in SQLite; older
		// rows roll off to <coldDir>/diagnostics/YYYY/MM/DD.parquet via
		// RolloffDiagnosticsToParquet.
		`CREATE TABLE IF NOT EXISTS planner_diagnostics (
			ts_ms          INTEGER PRIMARY KEY NOT NULL,
			reason         TEXT    NOT NULL,
			zone           TEXT    NOT NULL,
			total_cost_ore REAL    NOT NULL,
			horizon_slots  INTEGER NOT NULL,
			json           TEXT    NOT NULL
		) STRICT`,

		// Nova federation: one row per local DER we've provisioned in Nova.
		// Keyed on (device_id, der_type) so a hybrid inverter with multiple
		// DERs (battery + pv + meter on the same device_id) has one row per
		// DER. The Nova-generated der_id is stored purely for diagnostics
		// and future control-topic subscriptions; the publish path uses
		// (hardware_id, der_name) which are client-owned.
		// Notification history — one row per attempted push. Populated by
		// a bus subscriber in main.go (see events.NotificationDispatched)
		// so the notifications service itself stays free of storage logic.
		// Retention is unbounded for now; volumes are small (operators
		// configure a threshold + cooldown, not per-tick events) so a
		// house would take years to accumulate even 100k rows.
		`CREATE TABLE IF NOT EXISTS notification_log (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			ts_ms      INTEGER NOT NULL,
			event_type TEXT NOT NULL,
			driver     TEXT NOT NULL DEFAULT '',
			title      TEXT NOT NULL DEFAULT '',
			body       TEXT NOT NULL DEFAULT '',
			priority   INTEGER NOT NULL DEFAULT 0,
			status     TEXT NOT NULL,
			error      TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notification_log_ts ON notification_log(ts_ms DESC)`,

		`CREATE TABLE IF NOT EXISTS nova_ders (
			device_id   TEXT NOT NULL,
			der_type    TEXT NOT NULL,
			der_name    TEXT NOT NULL,
			der_id      TEXT NOT NULL,
			synced_ms   INTEGER NOT NULL,
			PRIMARY KEY (device_id, der_type)
		) STRICT`,

		// Persistent daily-energy aggregate cache.
		//
		// 2026-05-25 measurement: /api/energy/daily?days=30 took ~25 s
		// on the live Pi cold-start because the handler did one
		// DailyEnergy SQL call per day and the in-memory cache was
		// empty after every restart. Each call walked history_hot +
		// warm + cold for that day's window — slow on a 1 GB state.db.
		//
		// This table stores the integration result so closed days never
		// have to be re-computed. The handler writes a row on first
		// compute and reads it back forever — days are immutable once
		// past the local-midnight rollover, so cache invalidation is
		// trivially "always valid".
		//
		// Today's row is never persisted (the day is in progress); the
		// handler still computes it on every request. Tomorrow's
		// midnight rollover the previous day's final value lands here
		// once, lazily, on the next /api/energy/daily request.
		`CREATE TABLE IF NOT EXISTS energy_daily (
			day               TEXT PRIMARY KEY,
			import_wh         REAL NOT NULL,
			export_wh         REAL NOT NULL,
			pv_wh             REAL NOT NULL,
			bat_charged_wh    REAL NOT NULL,
			bat_discharged_wh REAL NOT NULL,
			load_wh           REAL NOT NULL,
			computed_at_ms    INTEGER NOT NULL
		) STRICT`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migration %q: %w", stmt[:40]+"…", err)
		}
	}
	return nil
}

// ---- Config key-value ----

// SaveConfig writes a config k/v. Upserts on conflict.
func (s *Store) SaveConfig(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO config (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// LoadConfig returns the value for key, or ok=false if missing.
func (s *Store) LoadConfig(key string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		return "", false
	}
	return v, true
}

// ---- Events ----

// RecordEvent appends an event at the current ms timestamp. Collision-safe up to 1 per ms.
func (s *Store) RecordEvent(event string) error {
	ts := time.Now().UnixMilli()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO events (ts_ms, event) VALUES (?, ?)`, ts, event)
	return err
}

// RecentEvents returns the N most recent events (most recent first).
func (s *Store) RecentEvents(limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT ts_ms, event FROM events ORDER BY ts_ms DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Event, 0, limit)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.TsMs, &e.Event); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Event is one entry from the events log.
type Event struct {
	TsMs  int64
	Event string
}

// ---- Telemetry snapshots ----

// SaveTelemetry stores the latest known state of one DER key for crash recovery.
func (s *Store) SaveTelemetry(key, json string) error {
	_, err := s.db.Exec(`INSERT INTO telemetry (key, json) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET json = excluded.json`, key, json)
	return err
}

// LoadTelemetry returns the most recent saved JSON blob for a key.
func (s *Store) LoadTelemetry(key string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT json FROM telemetry WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		return "", false
	}
	return v, true
}

// ---- Battery models ----

// SaveBatteryModel stores the JSON-serialized model state for a driver.
// The storage key is the resolved device_id when known (so renames don't
// orphan trained state); falls back to the driver name during cold-start
// before any device has reported its identity.
func (s *Store) SaveBatteryModel(name, json string) error {
	key := s.batteryModelKey(name)
	_, err := s.db.Exec(`INSERT INTO battery_models (name, json) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET json = excluded.json`, key, json)
	return err
}

// LoadAllBatteryModels returns all stored model states keyed by the
// CURRENT driver_name (looked up via the devices table). Rows whose
// device_id has no matching driver in this config are skipped silently —
// they belong to drivers the operator has removed from the YAML.
//
// Pulls all device rows BEFORE opening the battery_models query. Originally
// required because the pool was capped at 1 connection (overlapping Rows on
// the same goroutine deadlocked); now harmless under the larger pool but
// the pattern stays — pre-resolving lookups before the main scan still
// produces simpler, faster code.
func (s *Store) LoadAllBatteryModels() (map[string]string, error) {
	// Phase 1: build device_id → driver_name reverse map.
	rev := make(map[string]string)
	if drows, err := s.db.Query(`SELECT device_id, driver_name FROM devices`); err == nil {
		for drows.Next() {
			var id, n string
			if err := drows.Scan(&id, &n); err == nil { rev[id] = n }
		}
		drows.Close()
	}
	// Phase 2: read battery_models, translating keys via rev.
	rows, err := s.db.Query(`SELECT name, json FROM battery_models`)
	if err != nil { return nil, err }
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, js string
		if err := rows.Scan(&name, &js); err != nil { return out, err }
		if n, ok := rev[name]; ok {
			out[n] = js
		} else if !strings.Contains(name, ":") {
			// Legacy driver-name key — pass through (migration covers this on next tick).
			out[name] = js
		}
	}
	return out, rows.Err()
}

// DeleteBatteryModel removes a stored model (used when resetting).
func (s *Store) DeleteBatteryModel(name string) error {
	key := s.batteryModelKey(name)
	_, err := s.db.Exec(`DELETE FROM battery_models WHERE name = ?`, key)
	return err
}

// batteryModelKey resolves a driver name to its canonical storage key:
// device_id when known, otherwise the raw driver name (legacy / cold
// start before identity has been registered).
func (s *Store) batteryModelKey(driverName string) string {
	if dev := s.LookupDeviceByDriverName(driverName); dev != nil && dev.DeviceID != "" {
		return dev.DeviceID
	}
	return driverName
}

// ---- History tiers ----

// HistoryPoint is one row of the history table.
type HistoryPoint struct {
	TsMs   int64
	GridW  float64
	PVW    float64
	BatW   float64
	LoadW  float64
	BatSoC float64
	JSON   string
}

// RecordHistory inserts a new hot-tier entry.
func (s *Store) RecordHistory(p HistoryPoint) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO history_hot (ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.TsMs, p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC, p.JSON,
	)
	return err
}

// BulkRecordHistory writes many HistoryPoints in a single transaction.
// Used by backfill / migration tooling where per-row implicit-commit
// overhead dominates (SQLite on slow filesystems).
func (s *Store) BulkRecordHistory(pts []HistoryPoint) error {
	if len(pts) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO history_hot (ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range pts {
		if _, err := stmt.Exec(p.TsMs, p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC, p.JSON); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadHistory returns points from ALL tiers in [sinceMs, untilMs], merged + sorted.
// maxPoints=0 means no limit. With a limit, we return at most that many evenly-spaced rows.
func (s *Store) LoadHistory(sinceMs, untilMs int64, maxPoints int) ([]HistoryPoint, error) {
	// Union across all three tiers. Dedupe on ts_ms preferring hot over warm over cold.
	// COALESCE to 0 so NULL columns (from partial aggregations) scan cleanly.
	query := `
		WITH all_rows AS (
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json, 0 AS tier FROM history_hot
			WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json, 1 FROM history_warm
			WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json, 2 FROM history_cold
			WHERE ts_ms BETWEEN ? AND ?
		),
		deduped AS (
			SELECT * FROM all_rows
			GROUP BY ts_ms
			HAVING tier = MIN(tier)
		)
		SELECT ts_ms,
		       COALESCE(grid_w, 0), COALESCE(pv_w, 0), COALESCE(bat_w, 0),
		       COALESCE(load_w, 0), COALESCE(bat_soc, 0), json
		FROM deduped
		ORDER BY ts_ms ASC
	`
	rows, err := s.db.Query(query, sinceMs, untilMs, sinceMs, untilMs, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	all := make([]HistoryPoint, 0)
	for rows.Next() {
		var p HistoryPoint
		if err := rows.Scan(&p.TsMs, &p.GridW, &p.PVW, &p.BatW, &p.LoadW, &p.BatSoC, &p.JSON); err != nil {
			return all, err
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return all, err
	}

	// Downsample by evenly picking maxPoints rows
	if maxPoints > 0 && len(all) > maxPoints {
		if maxPoints == 1 {
			return []HistoryPoint{all[len(all)-1]}, nil
		}
		out := make([]HistoryPoint, 0, maxPoints)
		for i := 0; i < maxPoints; i++ {
			idx := i * (len(all) - 1) / (maxPoints - 1)
			out = append(out, all[idx])
		}
		return out, nil
	}
	return all, nil
}

// DayEnergy is the set of Wh totals over a time range, in site convention:
// Import/Export are grid-boundary W split on sign; PV and LoadWh are always
// positive accumulations; BatCharged/BatDischarged split bat_w on sign.
type DayEnergy struct {
	ImportWh        float64
	ExportWh        float64
	PVWh            float64
	BatChargedWh    float64
	BatDischargedWh float64
	LoadWh          float64
}

// DailyEnergy integrates history W columns over [sinceMs, untilMs] and returns
// Wh totals in a single round-trip. The integration is a left-Riemann sum
// (W[j] * (ts[j]-ts[j-1])), matching the previous Go loop in handleEnergyDaily.
// Pushing the sums into SQL avoids shipping ~17k hot-tier rows per day back to
// the application — month-view dashboards got slow once hot retention grew.
func (s *Store) DailyEnergy(sinceMs, untilMs int64) (DayEnergy, error) {
	const q = `
		WITH all_rows AS (
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w FROM history_hot  WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w FROM history_warm WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w FROM history_cold WHERE ts_ms BETWEEN ? AND ?
		),
		lagged AS (
			SELECT ts_ms,
			       COALESCE(grid_w, 0) AS grid_w,
			       COALESCE(pv_w,   0) AS pv_w,
			       COALESCE(bat_w,  0) AS bat_w,
			       COALESCE(load_w, 0) AS load_w,
			       LAG(ts_ms) OVER (ORDER BY ts_ms) AS prev_ts
			FROM all_rows
		)
		SELECT
			COALESCE(SUM((CASE WHEN grid_w > 0 THEN  grid_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((CASE WHEN grid_w < 0 THEN -grid_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((-pv_w) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((CASE WHEN bat_w > 0 THEN  bat_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((CASE WHEN bat_w < 0 THEN -bat_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM(load_w * (ts_ms - prev_ts)) / 3600000.0, 0)
		FROM lagged
		WHERE prev_ts IS NOT NULL
	`
	var d DayEnergy
	err := s.db.QueryRow(q,
		sinceMs, untilMs,
		sinceMs, untilMs,
		sinceMs, untilMs,
	).Scan(
		&d.ImportWh, &d.ExportWh, &d.PVWh,
		&d.BatChargedWh, &d.BatDischargedWh, &d.LoadWh,
	)
	if err != nil {
		return DayEnergy{}, err
	}
	return d, nil
}

// LoadDailyEnergy returns the persisted aggregate for `day` (YYYY-MM-DD).
// Second return is false when the day isn't cached yet. The caller
// recomputes via DailyEnergy on miss and writes back via SaveDailyEnergy.
//
// Closed days are immutable, so callers can treat hit-rows as
// authoritative — no TTL, no staleness check needed.
func (s *Store) LoadDailyEnergy(day string) (DayEnergy, bool, error) {
	const q = `
		SELECT import_wh, export_wh, pv_wh, bat_charged_wh, bat_discharged_wh, load_wh
		FROM energy_daily WHERE day = ?
	`
	var d DayEnergy
	err := s.db.QueryRow(q, day).Scan(
		&d.ImportWh, &d.ExportWh, &d.PVWh,
		&d.BatChargedWh, &d.BatDischargedWh, &d.LoadWh,
	)
	if err == sql.ErrNoRows {
		return DayEnergy{}, false, nil
	}
	if err != nil {
		return DayEnergy{}, false, err
	}
	return d, true, nil
}

// SaveDailyEnergy persists `de` for `day`. Upserts on conflict — the
// row is the latest authoritative aggregate. Today's row should NOT be
// persisted via this method (the day is still accumulating); callers
// should gate on "is closed day" before saving.
func (s *Store) SaveDailyEnergy(day string, de DayEnergy) error {
	const q = `
		INSERT INTO energy_daily(
			day, import_wh, export_wh, pv_wh, bat_charged_wh, bat_discharged_wh, load_wh, computed_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day) DO UPDATE SET
			import_wh         = excluded.import_wh,
			export_wh         = excluded.export_wh,
			pv_wh             = excluded.pv_wh,
			bat_charged_wh    = excluded.bat_charged_wh,
			bat_discharged_wh = excluded.bat_discharged_wh,
			load_wh           = excluded.load_wh,
			computed_at_ms    = excluded.computed_at_ms
	`
	_, err := s.db.Exec(q, day,
		de.ImportWh, de.ExportWh, de.PVWh,
		de.BatChargedWh, de.BatDischargedWh, de.LoadWh,
		time.Now().UnixMilli(),
	)
	return err
}

// CountNonSyntheticHistory returns the number of history rows across all
// three tiers whose JSON payload is NOT the backfill marker — i.e. rows
// that look like real recorded data. Used by the dev-backfill safety
// gate so pointing the seeder at a production DB aborts cleanly.
func (s *Store) CountNonSyntheticHistory() (int, error) {
	const marker = `{"source":"backfill"}`
	const q = `
		SELECT
			(SELECT COUNT(*) FROM history_hot  WHERE json IS NOT ?) +
			(SELECT COUNT(*) FROM history_warm WHERE json IS NOT ?) +
			(SELECT COUNT(*) FROM history_cold WHERE json IS NOT ?)
	`
	var n int
	if err := s.db.QueryRow(q, marker, marker, marker).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// HistoryCounts returns the number of rows in (hot, warm, cold) tiers.
func (s *Store) HistoryCounts() (hot, warm, cold int, err error) {
	row := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM history_hot),
		(SELECT COUNT(*) FROM history_warm),
		(SELECT COUNT(*) FROM history_cold)`)
	err = row.Scan(&hot, &warm, &cold)
	return
}

// Prune ages old hot rows into warm buckets, old warm into cold daily buckets.
// This is pure SQL — no custom Go bucketing needed. Idempotent; safe to call often.
func (s *Store) Prune(ctx context.Context) error {
	nowMs := time.Now().UnixMilli()
	hotCutoff := nowMs - int64(HotRetention.Milliseconds())
	warmCutoff := nowMs - int64(WarmRetention.Milliseconds())

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. hot → warm (15-min buckets)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT OR REPLACE INTO history_warm (ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json)
		SELECT
			(ts_ms / %d) * %d + %d AS bucket_ts,
			AVG(grid_w), AVG(pv_w), AVG(bat_w), AVG(load_w), AVG(bat_soc),
			-- Pick any JSON from the bucket; aggregation via SQL is too fiddly.
			(SELECT json FROM history_hot h2 WHERE h2.ts_ms / %d = h.ts_ms / %d LIMIT 1)
		FROM history_hot h
		WHERE ts_ms < ?
		GROUP BY ts_ms / %d
	`, WarmBucketMS, WarmBucketMS, WarmBucketMS/2, WarmBucketMS, WarmBucketMS, WarmBucketMS), hotCutoff); err != nil {
		return fmt.Errorf("aggregate hot→warm: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM history_hot WHERE ts_ms < ?`, hotCutoff); err != nil {
		return fmt.Errorf("delete old hot: %w", err)
	}

	// 2. warm → cold (1-day buckets)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT OR REPLACE INTO history_cold (ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json)
		SELECT
			(ts_ms / %d) * %d + %d AS bucket_ts,
			AVG(grid_w), AVG(pv_w), AVG(bat_w), AVG(load_w), AVG(bat_soc),
			(SELECT json FROM history_warm w2 WHERE w2.ts_ms / %d = w.ts_ms / %d LIMIT 1)
		FROM history_warm w
		WHERE ts_ms < ?
		GROUP BY ts_ms / %d
	`, ColdBucketMS, ColdBucketMS, ColdBucketMS/2, ColdBucketMS, ColdBucketMS, ColdBucketMS), warmCutoff); err != nil {
		return fmt.Errorf("aggregate warm→cold: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM history_warm WHERE ts_ms < ?`, warmCutoff); err != nil {
		return fmt.Errorf("delete old warm: %w", err)
	}

	return tx.Commit()
}

// ---- Prices ----

// PricePoint is one time-slot's spot price row. Slot length varies by source:
// NordPool/elprisetjustnu is 15 min since late 2025; ENTSOE is mostly still
// hourly. Consumers should honor SlotLenMin when plotting or aggregating.
type PricePoint struct {
	Zone        string  `json:"zone"`
	SlotTsMs    int64   `json:"slot_ts_ms"`
	SlotLenMin  int     `json:"slot_len_min"`
	SpotOreKwh  float64 `json:"spot_ore_kwh"`
	TotalOreKwh float64 `json:"total_ore_kwh"`
	Source      string  `json:"source"`
	FetchedAtMs int64   `json:"fetched_at_ms"`
}

// SavePrices upserts a batch of price rows (slot duration per-row).
func (s *Store) SavePrices(pts []PricePoint) error {
	if len(pts) == 0 { return nil }
	tx, err := s.db.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO prices
		(zone, slot_ts_ms, slot_len_min, spot_ore_kwh, total_ore_kwh, source, fetched_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (zone, slot_ts_ms) DO UPDATE SET
			slot_len_min = excluded.slot_len_min,
			spot_ore_kwh = excluded.spot_ore_kwh,
			total_ore_kwh = excluded.total_ore_kwh,
			source = excluded.source,
			fetched_at_ms = excluded.fetched_at_ms`)
	if err != nil { return err }
	defer stmt.Close()
	for _, p := range pts {
		slot := p.SlotLenMin
		if slot <= 0 { slot = 60 }
		if _, err := stmt.Exec(p.Zone, p.SlotTsMs, slot, p.SpotOreKwh, p.TotalOreKwh, p.Source, p.FetchedAtMs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadPrices returns prices for zone in [sinceMs, untilMs], ordered ascending.
func (s *Store) LoadPrices(zone string, sinceMs, untilMs int64) ([]PricePoint, error) {
	rows, err := s.db.Query(`SELECT zone, slot_ts_ms, slot_len_min, spot_ore_kwh, total_ore_kwh, source, fetched_at_ms
		FROM prices
		WHERE zone = ? AND slot_ts_ms BETWEEN ? AND ?
		ORDER BY slot_ts_ms ASC`, zone, sinceMs, untilMs)
	if err != nil { return nil, err }
	defer rows.Close()
	out := []PricePoint{}
	for rows.Next() {
		var p PricePoint
		if err := rows.Scan(&p.Zone, &p.SlotTsMs, &p.SlotLenMin, &p.SpotOreKwh, &p.TotalOreKwh, &p.Source, &p.FetchedAtMs); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- Forecasts ----

// ForecastPoint is one slot's weather + derived PV estimate.
type ForecastPoint struct {
	SlotTsMs      int64    `json:"slot_ts_ms"`
	SlotLenMin    int      `json:"slot_len_min"`
	CloudCoverPct *float64 `json:"cloud_cover_pct,omitempty"`
	TempC         *float64 `json:"temp_c,omitempty"`
	SolarWm2      *float64 `json:"solar_wm2,omitempty"`
	PVWEstimated  *float64 `json:"pv_w_estimated,omitempty"`
	Source        string   `json:"source"`
	FetchedAtMs   int64    `json:"fetched_at_ms"`
}

// SaveForecasts upserts a batch of forecast rows.
func (s *Store) SaveForecasts(pts []ForecastPoint) error {
	if len(pts) == 0 { return nil }
	tx, err := s.db.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO forecasts
		(slot_ts_ms, slot_len_min, cloud_cover_pct, temp_c, solar_wm2, pv_w_estimated, source, fetched_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (slot_ts_ms) DO UPDATE SET
			slot_len_min = excluded.slot_len_min,
			cloud_cover_pct = excluded.cloud_cover_pct,
			temp_c = excluded.temp_c,
			solar_wm2 = excluded.solar_wm2,
			pv_w_estimated = excluded.pv_w_estimated,
			source = excluded.source,
			fetched_at_ms = excluded.fetched_at_ms`)
	if err != nil { return err }
	defer stmt.Close()
	for _, p := range pts {
		slot := p.SlotLenMin
		if slot <= 0 { slot = 60 }
		if _, err := stmt.Exec(p.SlotTsMs, slot, p.CloudCoverPct, p.TempC, p.SolarWm2, p.PVWEstimated, p.Source, p.FetchedAtMs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadForecasts returns forecasts in [sinceMs, untilMs], ordered ascending.
func (s *Store) LoadForecasts(sinceMs, untilMs int64) ([]ForecastPoint, error) {
	rows, err := s.db.Query(`SELECT slot_ts_ms, slot_len_min, cloud_cover_pct, temp_c, solar_wm2, pv_w_estimated, source, fetched_at_ms
		FROM forecasts
		WHERE slot_ts_ms BETWEEN ? AND ?
		ORDER BY slot_ts_ms ASC`, sinceMs, untilMs)
	if err != nil { return nil, err }
	defer rows.Close()
	out := []ForecastPoint{}
	for rows.Next() {
		var p ForecastPoint
		if err := rows.Scan(&p.SlotTsMs, &p.SlotLenMin, &p.CloudCoverPct, &p.TempC, &p.SolarWm2, &p.PVWEstimated, &p.Source, &p.FetchedAtMs); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
