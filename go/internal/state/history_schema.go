package state

var historySchema = append(append([]string{}, sqliteLegacyHistoryStmts...),
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
)
