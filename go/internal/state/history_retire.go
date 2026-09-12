package state

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	historyLegacySourcesRetiredKey  = "history_legacy_sources_retired"
	historyLegacySourcesRetiredName = "legacy-sources-retired"
)

// sqliteLegacyHistoryStmts are the SQLite history tables used only as the
// one-time import source. After a verified DuckDB import they are dropped
// and not recreated. Full backups recreate them as the portable export.
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
	`CREATE INDEX IF NOT EXISTS idx_ts_samples_ts ON ts_samples(ts_ms)`,
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

func (s *Store) legacyImportComplete() (bool, error) {
	if s.history == nil {
		return false, nil
	}
	var complete int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_migrations WHERE name='sqlite-v1'`).Scan(&complete); err != nil {
		return false, err
	}
	if complete == 0 {
		return false, nil
	}
	var pending int
	if err := s.history.QueryRow(`SELECT (SELECT COUNT(*) FROM history_parquet_imports) + (SELECT COUNT(*) FROM history_migrations WHERE name='legacy-import-pending')`).Scan(&pending); err != nil {
		return false, err
	}
	if pending != 0 {
		return false, nil
	}
	active, err := s.historyConfig("history_duckdb_generation")
	if err != nil {
		return false, err
	}
	return active != "", nil
}

func (s *Store) unimportedLegacyParquet(coldDir string) (bool, error) {
	if coldDir == "" || s.history == nil {
		return false, nil
	}
	paths, err := filepath.Glob(filepath.Join(coldDir, "[0-9][0-9][0-9][0-9]", "[0-9][0-9]", "[0-9][0-9].parquet"))
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return false, err
		}
		var n int
		if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_parquet_sources WHERE path=?`, abs).Scan(&n); err != nil {
			return false, err
		}
		if n == 0 {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) legacyHistoryIdle(coldDir string) (bool, error) {
	complete, err := s.legacyImportComplete()
	if err != nil || !complete {
		return false, err
	}
	leftover, err := s.unimportedLegacyParquet(coldDir)
	if err != nil {
		return false, err
	}
	return !leftover, nil
}

func ensureSqliteLegacyHistory(exec func(string) error) error {
	for _, stmt := range sqliteLegacyHistoryStmts {
		if err := exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// retireLegacyHistorySources drops the frozen SQLite history copy and the
// imported Parquet files after DuckDB has verified every source. Incomplete
// imports, failed imports and unbound generations leave the originals in place.
func (s *Store) retireLegacyHistorySources() error {
	complete, err := s.legacyImportComplete()
	if err != nil || !complete {
		return err
	}
	active, err := s.historyConfig("history_duckdb_generation")
	if err != nil {
		return err
	}
	if err := s.SaveConfig(historyLegacySourcesRetiredKey, active); err != nil {
		return err
	}
	for _, table := range historyTables {
		if _, err := s.db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			return fmt.Errorf("drop leftover SQLite %s: %w", table, err)
		}
	}
	if s.history != nil {
		rows, err := s.history.Query(`SELECT path FROM history_parquet_sources`)
		if err != nil {
			return err
		}
		var paths []string
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return err
			}
			paths = append(paths, path)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				slog.Warn("legacy parquet source still on disk", "path", path, "err", err)
				continue
			}
			dir := filepath.Dir(path)
			_ = os.Remove(dir)
			_ = os.Remove(filepath.Dir(dir))
		}
		if _, err := s.history.Exec(`INSERT INTO history_migrations(name) VALUES (?) ON CONFLICT DO NOTHING`, historyLegacySourcesRetiredName); err != nil {
			return err
		}
	}
	slog.Info("legacy history sources retired; DuckDB is the sole history store")
	return nil
}
