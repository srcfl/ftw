package state

var historySchema = append(append([]string{}, sqliteLegacyHistoryStmts...),
	`CREATE TABLE IF NOT EXISTS ts_latest (driver_id INTEGER NOT NULL,metric_id INTEGER NOT NULL,ts_ms INTEGER NOT NULL,value REAL NOT NULL,PRIMARY KEY(driver_id,metric_id)) WITHOUT ROWID`,
	dashboardSchema, siteEnergySchema, siteEnergyCursorSchema,
	`CREATE INDEX IF NOT EXISTS idx_dashboard_last ON history_dashboard(last_ms)`,
	`CREATE INDEX IF NOT EXISTS idx_dashboard_age ON history_dashboard(resolution_ms,start_ms)`,
	`CREATE INDEX IF NOT EXISTS idx_site_energy_age ON history_site_energy(resolution_ms,start_ms)`,
	`CREATE TABLE IF NOT EXISTS ts_archive_days(path TEXT PRIMARY KEY,sha256 TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS history_receipts (sequence INTEGER PRIMARY KEY AUTOINCREMENT, batch_id TEXT NOT NULL UNIQUE, payload_hash TEXT NOT NULL, committed_at TEXT NOT NULL DEFAULT current_timestamp)`,
	`CREATE TABLE IF NOT EXISTS history_sqlite_progress (source TEXT PRIMARY KEY, rows_done INTEGER NOT NULL, driver_id INTEGER NOT NULL, metric_id INTEGER NOT NULL, ts_ms INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS history_migrations (name TEXT PRIMARY KEY, completed_at TEXT DEFAULT current_timestamp)`,
	`CREATE TABLE IF NOT EXISTS ts_series_hour (
			driver_id BIGINT NOT NULL,
			metric_id BIGINT NOT NULL,
			hour_ms   BIGINT NOT NULL,
			sum_value DOUBLE NOT NULL,
			min_value DOUBLE NOT NULL,
			max_value DOUBLE NOT NULL,
			n         BIGINT NOT NULL,
			last_ts_ms BIGINT NOT NULL,
			PRIMARY KEY (driver_id, metric_id, hour_ms)
		)`,
	`CREATE TABLE IF NOT EXISTS ts_aggregate_hours (
 driver_id INTEGER NOT NULL,metric_id INTEGER NOT NULL,hour_ms INTEGER NOT NULL,
 sum_value REAL NOT NULL,min_value REAL NOT NULL,max_value REAL NOT NULL,n INTEGER NOT NULL,last_ts_ms INTEGER NOT NULL,
 PRIMARY KEY(driver_id,metric_id,hour_ms)) WITHOUT ROWID`,
	`CREATE TABLE IF NOT EXISTS ts_buckets (
 driver_id INTEGER NOT NULL,metric_id INTEGER NOT NULL,start_ms INTEGER NOT NULL,resolution_ms INTEGER NOT NULL,
 first_ms INTEGER NOT NULL,last_ms INTEGER NOT NULL,n INTEGER NOT NULL,sum_value REAL NOT NULL,min_value REAL NOT NULL,max_value REAL NOT NULL,last_value REAL NOT NULL, seen_ms BLOB NOT NULL DEFAULT X'',
 PRIMARY KEY(driver_id,metric_id,start_ms,resolution_ms)) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS idx_bucket_age ON ts_buckets(resolution_ms,start_ms)`,
	`CREATE TABLE IF NOT EXISTS ts_legacy_bucket_days(day_ms INTEGER PRIMARY KEY,source_sha256 TEXT NOT NULL,sha256 TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS ts_bucket_days(day_ms INTEGER PRIMARY KEY,sha256 TEXT NOT NULL,resolution_ms INTEGER NOT NULL,through_ms INTEGER NOT NULL)`,
)
