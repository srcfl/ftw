package state

const (
	historyLegacySourcesRetiredKey  = "history_legacy_sources_retired"
	historyLegacySourcesRetiredName = "legacy-sources-retired"
	sampleTimeIndexSQL              = `CREATE INDEX IF NOT EXISTS idx_ts_samples_ts ON ts_samples(ts_ms)`
)

// sqliteLegacyHistoryStmts define the portable history schema shared with
// older SQLite installations. Core selects history.db; source tables remain.
var sqliteLegacyHistoryStmts = []string{
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
	sampleTimeIndexSQL,
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
	`CREATE TABLE IF NOT EXISTS energy_ledger_meta (
			key   TEXT PRIMARY KEY NOT NULL,
			value TEXT NOT NULL
		) STRICT`,
	`INSERT OR IGNORE INTO energy_ledger_meta(key, value)
			VALUES ('schema_version', '1')`,
	`CREATE TABLE IF NOT EXISTS energy_assets (
			asset_id       TEXT PRIMARY KEY NOT NULL,
			device_id      TEXT NOT NULL DEFAULT '',
			kind           TEXT NOT NULL,
			label          TEXT NOT NULL DEFAULT '',
			read_only      INTEGER NOT NULL DEFAULT 0 CHECK(read_only IN (0, 1)),
			first_seen_ms  INTEGER NOT NULL,
			last_seen_ms   INTEGER NOT NULL
		) STRICT`,
	`CREATE INDEX IF NOT EXISTS idx_energy_assets_device
			ON energy_assets(device_id, kind)`,
	`CREATE TABLE IF NOT EXISTS energy_ledger_entries (
			schema_version INTEGER NOT NULL,
			asset_id       TEXT NOT NULL,
			flow           TEXT NOT NULL,
			bucket_start_ms INTEGER NOT NULL,
			bucket_len_ms   INTEGER NOT NULL CHECK(bucket_len_ms > 0),
			energy_wh      REAL NOT NULL CHECK(energy_wh >= 0),
			source         TEXT NOT NULL,
			quality        TEXT NOT NULL,
			provenance     TEXT NOT NULL,
			sample_count   INTEGER NOT NULL DEFAULT 1 CHECK(sample_count > 0),
			observed_at_ms INTEGER NOT NULL,
			PRIMARY KEY (
				schema_version, asset_id, flow, bucket_start_ms,
				bucket_len_ms, source, quality, provenance
			)
		) WITHOUT ROWID, STRICT`,
	`CREATE INDEX IF NOT EXISTS idx_energy_ledger_time
			ON energy_ledger_entries(bucket_start_ms, asset_id, flow)`,
	`CREATE TABLE IF NOT EXISTS energy_ledger_cursors (
			asset_id   TEXT NOT NULL,
			flow       TEXT NOT NULL,
			cursor_kind TEXT NOT NULL,
			value      REAL NOT NULL,
			ts_ms      INTEGER NOT NULL,
			PRIMARY KEY(asset_id, flow, cursor_kind)
		) WITHOUT ROWID, STRICT`,
}

func (s *Store) legacyHistorySourcesRetired() bool {
	value, err := s.historyConfig(historyLegacySourcesRetiredKey)
	return err == nil && value != ""
}
func ensureSqliteLegacyHistory(exec func(string) error) error {
	for _, stmt := range sqliteLegacyHistoryStmts {
		if err := exec(stmt); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) legacyHistoryIdle(coldDir string) (bool, error) {
	var n int
	err := s.history.QueryRow(`SELECT COUNT(*) FROM history_migrations WHERE name='sqlite-v1'`).Scan(&n)
	return n > 0, err
}
