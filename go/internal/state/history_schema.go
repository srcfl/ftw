package state

// HistorySchema is separate from SQLite configuration and model state.
var historySchema = []string{
	`CREATE TABLE IF NOT EXISTS history_parquet_imports (path VARCHAR PRIMARY KEY, sha256 VARCHAR NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS history_parquet_sources (path VARCHAR PRIMARY KEY, sha256 VARCHAR NOT NULL, rows BIGINT NOT NULL, imported_at TIMESTAMP DEFAULT current_timestamp)`,
	`CREATE SEQUENCE IF NOT EXISTS history_commit_sequence START 1`,
	`CREATE TABLE IF NOT EXISTS history_receipts (batch_id VARCHAR PRIMARY KEY, payload_hash VARCHAR NOT NULL, sequence BIGINT NOT NULL DEFAULT nextval('history_commit_sequence'), committed_at TIMESTAMP NOT NULL DEFAULT current_timestamp)`,
	`CREATE SEQUENCE IF NOT EXISTS ts_drivers_id START 1`,
	`CREATE SEQUENCE IF NOT EXISTS ts_metrics_id START 1`,
	`CREATE TABLE IF NOT EXISTS history_hot (
			ts_ms BIGINT PRIMARY KEY NOT NULL,
			grid_w DOUBLE CHECK (grid_w IS NULL OR isfinite(grid_w)), pv_w DOUBLE CHECK (pv_w IS NULL OR isfinite(pv_w)), bat_w DOUBLE CHECK (bat_w IS NULL OR isfinite(bat_w)), load_w DOUBLE CHECK (load_w IS NULL OR isfinite(load_w)), bat_soc DOUBLE CHECK (bat_soc IS NULL OR isfinite(bat_soc)),
			json TEXT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS history_warm (
			ts_ms BIGINT PRIMARY KEY NOT NULL,
			grid_w DOUBLE CHECK (grid_w IS NULL OR isfinite(grid_w)), pv_w DOUBLE CHECK (pv_w IS NULL OR isfinite(pv_w)), bat_w DOUBLE CHECK (bat_w IS NULL OR isfinite(bat_w)), load_w DOUBLE CHECK (load_w IS NULL OR isfinite(load_w)), bat_soc DOUBLE CHECK (bat_soc IS NULL OR isfinite(bat_soc)),
			json TEXT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS history_cold (
			ts_ms BIGINT PRIMARY KEY NOT NULL,
			grid_w DOUBLE CHECK (grid_w IS NULL OR isfinite(grid_w)), pv_w DOUBLE CHECK (pv_w IS NULL OR isfinite(pv_w)), bat_w DOUBLE CHECK (bat_w IS NULL OR isfinite(bat_w)), load_w DOUBLE CHECK (load_w IS NULL OR isfinite(load_w)), bat_soc DOUBLE CHECK (bat_soc IS NULL OR isfinite(bat_soc)),
			json TEXT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS ts_drivers (
			id BIGINT PRIMARY KEY DEFAULT nextval('ts_drivers_id'),
			name TEXT NOT NULL UNIQUE
		)`,
	`CREATE TABLE IF NOT EXISTS ts_metrics (
			id BIGINT PRIMARY KEY DEFAULT nextval('ts_metrics_id'),
			name TEXT NOT NULL UNIQUE,
			unit TEXT
		)`,
	`CREATE TABLE IF NOT EXISTS ts_samples (
			driver_id BIGINT NOT NULL,
			metric_id BIGINT NOT NULL,
			ts_ms     BIGINT NOT NULL,
			value     DOUBLE NOT NULL,
			PRIMARY KEY (driver_id, metric_id, ts_ms)
		)`,
	`CREATE TABLE IF NOT EXISTS energy_daily (
			day               TEXT PRIMARY KEY,
			import_wh         DOUBLE NOT NULL,
			export_wh         DOUBLE NOT NULL,
			pv_wh             DOUBLE NOT NULL,
			bat_charged_wh    DOUBLE NOT NULL,
			bat_discharged_wh DOUBLE NOT NULL CHECK (bat_discharged_wh IS NULL OR isfinite(bat_discharged_wh)),
			load_wh           DOUBLE NOT NULL,
			computed_at_ms    BIGINT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS energy_ledger_meta (
			key   TEXT PRIMARY KEY NOT NULL,
			value TEXT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS energy_assets (
			asset_id       TEXT PRIMARY KEY NOT NULL,
			device_id      TEXT NOT NULL DEFAULT '',
			kind           TEXT NOT NULL,
			label          TEXT NOT NULL DEFAULT '',
			read_only      BIGINT NOT NULL DEFAULT 0 CHECK(read_only IN (0, 1)),
			first_seen_ms  BIGINT NOT NULL,
			last_seen_ms   BIGINT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS energy_ledger_entries (
			schema_version BIGINT NOT NULL,
			asset_id       TEXT NOT NULL,
			flow           TEXT NOT NULL,
			bucket_start_ms BIGINT NOT NULL,
			bucket_len_ms   BIGINT NOT NULL CHECK(bucket_len_ms > 0),
			energy_wh      DOUBLE NOT NULL CHECK(energy_wh >= 0),
			source         TEXT NOT NULL,
			quality        TEXT NOT NULL,
			provenance     TEXT NOT NULL,
			sample_count   BIGINT NOT NULL DEFAULT 1 CHECK(sample_count > 0),
			observed_at_ms BIGINT NOT NULL,
			PRIMARY KEY (
				schema_version, asset_id, flow, bucket_start_ms,
				bucket_len_ms, source, quality, provenance
			)
		)`,
	`CREATE TABLE IF NOT EXISTS energy_ledger_cursors (
			asset_id   TEXT NOT NULL,
			flow       TEXT NOT NULL,
			cursor_kind TEXT NOT NULL,
			value      DOUBLE NOT NULL,
			ts_ms      BIGINT NOT NULL,
			PRIMARY KEY(asset_id, flow, cursor_kind)
		)`,
	`INSERT OR IGNORE INTO energy_ledger_meta VALUES ('schema_version', '1')`,
	`CREATE TABLE IF NOT EXISTS history_migrations (name VARCHAR PRIMARY KEY, completed_at TIMESTAMP DEFAULT current_timestamp)`,
}
