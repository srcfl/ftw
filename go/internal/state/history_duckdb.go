package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
	"github.com/google/uuid"
)

const HistoryFilename = "history.duckdb"

var historyTables = []string{
	"history_hot", "history_warm", "history_cold", "ts_drivers", "ts_metrics", "ts_samples",
	"energy_daily", "energy_ledger_meta", "energy_assets", "energy_ledger_entries", "energy_ledger_cursors",
}

// openHistory runs before writers or hardware start. A failed migration keeps
// SQLite untouched and cannot mark the new database as authoritative.
func (s *Store) openHistory() error {
	s.historyPath = historyDatabasePath(s.mainDBPath)
	active, err := s.historyConfig("history_duckdb_generation")
	if err != nil {
		return err
	}
	restore, err := s.historyConfig("history_restore_generation")
	if err != nil {
		return err
	}
	intent, err := s.historyConfig("history_migration_generation")
	if err != nil {
		return err
	}
	if _, err := os.Stat(s.historyPath); errors.Is(err, os.ErrNotExist) && active != "" && restore == "" {
		return errors.New("primary DuckDB history is missing; restore a full backup")
	}
	connector, err := newHistoryConnector(s.historyPath + "?threads=2&memory_limit=256MB&max_temp_directory_size=512MB&autoload_known_extensions=false&autoinstall_known_extensions=false")
	if err != nil {
		return fmt.Errorf("open DuckDB history: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(0)
	s.historyConnector = connector
	s.history = db
	ok := false
	defer func() {
		if !ok {
			db.Close()
			s.history = nil
		}
	}()
	for _, stmt := range historySchema {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("history schema: %w", err)
		}
	}
	var complete int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history_migrations WHERE name='sqlite-v1'`).Scan(&complete); err != nil {
		return err
	}
	var seeded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history_migrations WHERE name='sqlite-seed-v1'`).Scan(&seeded); err != nil {
		return err
	}
	if seeded != 0 && complete == 0 && s.historyMigration == nil {
		return errors.New("raw history migration is pending; start Core to resume it before using offline history tools")
	}
	var generation string
	if complete != 0 || seeded != 0 {
		if err := db.QueryRow(`SELECT name FROM history_migrations WHERE name LIKE 'generation:%'`).Scan(&generation); err != nil {
			return err
		}
		generation = strings.TrimPrefix(generation, "generation:")
	}
	if restore != "" && (complete != 0 || seeded != 0) && generation != restore {
		// A restore explicitly selects its SQLite snapshot as the source.
		// Preserve the previous DuckDB files; never silently reuse old history.
		db.Close()
		suffix := ".before-restore-" + uuid.NewString()
		for _, path := range []string{s.historyPath, s.historyPath + ".wal"} {
			if err := os.Rename(path, path+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		s.history = nil
		ok = true
		return s.openHistory()
	}
	if (complete != 0 || seeded != 0) && active == "" && restore == "" && intent != generation {
		return errors.New("unbound DuckDB history beside SQLite; restore a full backup before starting")
	}
	if complete == 0 && seeded == 0 {
		if active != "" && restore == "" {
			return errors.New("primary DuckDB history is incomplete; restore a full backup")
		}
		generation = restore
		if generation == "" {
			generation = intent
		}
		if generation == "" {
			generation = uuid.NewString()
		}
		if intent != generation {
			if err := s.SaveConfig("history_migration_generation", generation); err != nil {
				return err
			}
		}
		if err := s.migrateSQLiteHistory(context.Background(), generation); err != nil {
			return err
		}
	}
	if active != "" && restore == "" && active != generation {
		return errors.New("SQLite and DuckDB history generations differ; restore a full backup")
	}
	if active != generation {
		if err := s.SaveConfig("history_duckdb_generation", generation); err != nil {
			return err
		}
	}
	if restore != "" || intent != "" {
		if _, err := s.db.Exec(`DELETE FROM config WHERE key IN ('history_restore_generation','history_migration_generation')`); err != nil {
			return err
		}
	}
	if err := s.ensureEnergyLedgerVersion(); err != nil {
		return err
	}
	// DuckDB creates files using the process umask; history contains site data.
	if err := os.Chmod(s.historyPath, 0600); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *Store) migrateSQLiteHistory(ctx context.Context, generation string) error {
	source, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer source.Rollback()
	conn, err := s.history.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, table := range historyTables {
		if s.historyMigration != nil && table == "ts_samples" {
			continue
		}
		if s.historyMigration != nil {
			s.historyMigration.update(func(st *HistoryMigrationStatus) {
				st.CurrentSource = table
				st.CurrentSourceRowsDone, st.CurrentSourceRowsTotal = 0, 0
			})
		}
		// The destination stays inactive until every table passes verification.
		// Bounded commits keep migration memory independent of source row count.
		if _, err := conn.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return err
		}
		rows, err := source.QueryContext(ctx, `SELECT * FROM `+table+historyOrder(table))
		if err != nil {
			return err
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		var count int64
		expected := sha256.New()
		exhausted := false
		for !exhausted {
			if _, err = conn.ExecContext(ctx, `BEGIN TRANSACTION`); err != nil {
				rows.Close()
				return err
			}
			err = conn.Raw(func(raw any) error {
				app, err := duckdb.NewAppenderFromConn(nativeHistoryConn(raw), "", table)
				if err != nil {
					return err
				}
				values := make([]any, len(columns))
				pointers := make([]any, len(columns))
				appendValues := make([]driver.Value, len(columns))
				for i := range values {
					pointers[i] = &values[i]
				}
				var appendErr error
				for n := 0; n < 8192; n++ {
					if !rows.Next() {
						exhausted = true
						break
					}
					if appendErr = ctx.Err(); appendErr != nil {
						break
					}
					if appendErr = rows.Scan(pointers...); appendErr != nil {
						break
					}
					if appendErr = hashHistoryRow(expected, values); appendErr != nil {
						break
					}
					for i := range values {
						appendValues[i] = values[i]
						if f, ok := values[i].(float64); ok {
							appendValues[i] = canonicalHistoryFloat(f)
						}
					}
					if appendErr = app.AppendRow(appendValues...); appendErr != nil {
						break
					}
					count++
				}
				return errors.Join(appendErr, rows.Err(), app.Close())
			})
			if err == nil {
				_, err = conn.ExecContext(ctx, `COMMIT`)
			}
			if err != nil {
				conn.ExecContext(context.Background(), `ROLLBACK`)
				rows.Close()
				return fmt.Errorf("migrate history table %s: %w", table, err)
			}
			if s.historyMigration != nil {
				s.historyMigration.update(func(st *HistoryMigrationStatus) { st.CurrentSourceRowsDone = count })
			}
		}
		rows.Close()
		actualHash := sha256.New()
		gotCount, err := scanHistoryTable(ctx, conn, table, func(values []any) error { return hashHistoryRow(actualHash, values) })
		got := fmt.Sprintf("%x", actualHash.Sum(nil))
		if err != nil {
			return err
		}
		if got != fmt.Sprintf("%x", expected.Sum(nil)) || gotCount != count {
			return fmt.Errorf("history migration verification failed for %s", table)
		}
		slog.Info("history: verified SQLite import", "table", table, "rows", count)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN TRANSACTION`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	// Seed IDs above the imported IDs. Inserts select these sequences explicitly:
	// ALTER COLUMN SET DEFAULT nextval cannot replay safely from DuckDB's WAL.
	// Gaps in IDs have no semantic meaning.
	for _, table := range []string{"ts_drivers", "ts_metrics"} {
		var next int64
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0)+1 FROM `+table).Scan(&next); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`CREATE OR REPLACE SEQUENCE %s_next_id START %d`, table, next)); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO history_migrations(name) VALUES (?)`, "generation:"+generation); err != nil {
		return err
	}
	marker := "sqlite-v1"
	if s.historyMigration != nil {
		marker = "sqlite-seed-v1"
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO history_migrations(name) VALUES (?)`, marker); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	return nil
}

func historyFileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// ImportedHistoryFiles lists only verified legacy sources. Full backups omit
// them because their rows are already in the portable SQLite export; this
// also prevents an older Core from reading each sample twice after restore.
func (s *Store) ImportedHistoryFiles(ctx context.Context) (map[string]bool, error) {
	result := map[string]bool{}
	if s.history == nil {
		return result, nil
	}
	rows, err := s.history.QueryContext(ctx, `SELECT path,sha256 FROM history_parquet_sources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path, digest string
		if err := rows.Scan(&path, &digest); err != nil {
			return nil, err
		}
		actual, err := historyFileHash(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if actual != digest {
			return nil, fmt.Errorf("imported history source changed before backup: %s", path)
		}
		result[path] = true
	}
	return result, rows.Err()
}

// Signed zero carries no energy. SQLite normalizes it too; every other finite
// float keeps its bits. Non-finite values fail the persistence contract.
func historyFloatBits(value float64) uint64 {
	if value == 0 {
		return 0
	}
	return math.Float64bits(value)
}

func (s *Store) HistoryBackend() map[string]any {
	info := map[string]any{"engine": "duckdb", "version": "1.5.5", "role": "primary", "file": filepath.Base(s.historyPath), "writer": s.HistoryWriterStatus()}
	info["migration"] = s.HistoryMigrationStatus()
	for key, path := range map[string]string{"file_bytes": s.historyPath, "wal_bytes": s.historyPath + ".wal"} {
		if stat, err := os.Stat(path); err == nil {
			info[key] = stat.Size()
		} else if errors.Is(err, os.ErrNotExist) {
			info[key] = int64(0)
		}
	}
	return info
}

// CheckpointHistory makes completed transactions part of the main DuckDB file.
func (s *Store) CheckpointHistory(ctx context.Context) error {
	if s.history == nil {
		return nil
	}
	if s.historyConnector != nil {
		// A checkpoint alone does not evict all native index/table buffers.
		// Wait for a gap between active connections, then reopen the native
		// instance while retaining the public SQL pool and durable primary.
		return s.rotateHistory(ctx)
	}
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	_, err := s.history.ExecContext(ctx, `CHECKPOINT`)
	return err
}

// quoteDuckDBString is only for fixed administrative paths, never user SQL.
func quoteDuckDBString(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func historyOrder(table string) string {
	switch table {
	case "history_hot", "history_warm", "history_cold":
		return " ORDER BY ts_ms"
	case "ts_drivers", "ts_metrics":
		return " ORDER BY id"
	case "ts_samples":
		return " ORDER BY driver_id, metric_id, ts_ms"
	case "energy_daily":
		return " ORDER BY day"
	case "energy_assets":
		return " ORDER BY asset_id"
	case "energy_ledger_entries":
		return " ORDER BY schema_version, asset_id, flow, bucket_start_ms, bucket_len_ms, source, quality, provenance"
	case "energy_ledger_cursors":
		return " ORDER BY asset_id, flow, cursor_kind"
	default:
		return " ORDER BY key"
	}
}

func hashHistoryRow(h hash.Hash, values []any) error {
	var b [8]byte
	for _, v := range values {
		switch v := v.(type) {
		case nil:
			h.Write([]byte{0})
		case int64:
			h.Write([]byte{1})
			binary.LittleEndian.PutUint64(b[:], uint64(v))
			h.Write(b[:])
		case float64:
			h.Write([]byte{2})
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return errors.New("non-finite history value")
			}
			binary.LittleEndian.PutUint64(b[:], historyFloatBits(v))
			h.Write(b[:])
		case string:
			h.Write([]byte{3})
			binary.LittleEndian.PutUint64(b[:], uint64(len(v)))
			h.Write(b[:])
			h.Write([]byte(v))
		default:
			return fmt.Errorf("unexpected history column type %T", v)
		}
	}
	return nil
}

func hashHistoryRows(rows *sql.Rows) (string, int64, error) {
	columns, err := rows.Columns()
	if err != nil {
		return "", 0, err
	}
	vals := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	h := sha256.New()
	var n int64
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", n, err
		}
		if err := hashHistoryRow(h, vals); err != nil {
			return "", n, err
		}
		n++
	}
	return fmt.Sprintf("%x", h.Sum(nil)), n, rows.Err()
}

// exportHistoryToSQLite puts a coherent DuckDB read snapshot into the existing
// portable full-backup format. Older Core releases can read this snapshot too.
// The live SQLite history remains frozen; this writes only the backup copy.
func (s *Store) exportHistoryToSQLite(path string) error {
	if s.history == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := s.FlushHistory(ctx); err != nil {
		return err
	}
	src, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer src.Rollback()
	var sqliteComplete int
	if err := src.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_migrations WHERE name='sqlite-v1'`).Scan(&sqliteComplete); err != nil {
		return err
	}
	if sqliteComplete == 0 || !s.HistoryMigrationStatus().HistoryComplete {
		return errors.New("finish historical import before exporting a full backup; keep the verified pre-update backup")
	}
	var pending int
	if err := src.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM history_parquet_imports) + (SELECT COUNT(*) FROM history_migrations WHERE name='legacy-import-pending')`).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return errors.New("finish the pending Parquet import before exporting a full backup")
	}
	dest, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer dest.Close()
	tx, err := dest.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range historyTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return err
		}
		expected := sha256.New()
		var stmt *sql.Stmt
		count, err := scanHistoryTable(ctx, src, table, func(vals []any) error {
			if stmt == nil {
				marks := strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",")
				var prepareErr error
				stmt, prepareErr = tx.PrepareContext(ctx, `INSERT INTO `+table+` VALUES (`+marks+`)`)
				if prepareErr != nil {
					return prepareErr
				}
			}
			if err := hashHistoryRow(expected, vals); err != nil {
				return err
			}
			_, err := stmt.ExecContext(ctx, vals...)
			return err
		})
		if stmt != nil {
			err = errors.Join(err, stmt.Close())
		}
		if err != nil {
			return err
		}
		check, err := tx.QueryContext(ctx, `SELECT * FROM `+table+historyOrder(table))
		if err != nil {
			return err
		}
		digest, gotCount, err := hashHistoryRows(check)
		check.Close()
		if err != nil {
			return err
		}
		if gotCount != count || digest != fmt.Sprintf("%x", expected.Sum(nil)) {
			return fmt.Errorf("backup history verification failed for %s", table)
		}

	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM config WHERE key IN ('history_duckdb_generation','history_migration_generation')`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO config VALUES ('history_restore_generation',?)`, uuid.NewString()); err != nil {
		return err
	}
	return tx.Commit()
}

func appendHistoryRows(conn *sql.Conn, points []HistoryPoint) error {
	return conn.Raw(func(raw any) error {
		app, err := duckdb.NewTableAppender(nativeHistoryConn(raw), `INSERT OR REPLACE INTO history_hot SELECT * FROM appended_data`, "", "", "history_hot", nil)
		if err != nil {
			return err
		}
		var writeErr error
		// DuckDB's appender resolves duplicate keys in a vector with the first
		// row. Select the last input row explicitly to preserve history semantics.
		last := make(map[int64]int, len(points))
		for i, p := range points {
			last[p.TsMs] = i
		}
		for i, p := range points {
			if last[p.TsMs] != i {
				continue
			}
			p, writeErr = normalizeHistoryPoint(p)
			if writeErr != nil {
				break
			}
			if writeErr = app.AppendRow(p.TsMs, p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC, p.JSON); writeErr != nil {
				break
			}
		}
		return errors.Join(writeErr, app.Close())
	})
}

func historyDatabasePath(statePath string) string {
	name := filepath.Base(statePath)
	if name == "state.db" {
		return filepath.Join(filepath.Dir(statePath), HistoryFilename)
	}
	return filepath.Join(filepath.Dir(statePath), strings.TrimSuffix(name, filepath.Ext(name))+".history.duckdb")
}

func (s *Store) historyConfig(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM config WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// HistoryDatabasePath identifies the primary file for backup inventory only.
func HistoryDatabasePath(statePath string) string { return historyDatabasePath(statePath) }

func canonicalHistoryFloat(value float64) float64 {
	if value == 0 {
		return 0
	}
	return value
}

func validateHistorySamples(samples []Sample) error {
	for _, sm := range samples {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			return fmt.Errorf("non-finite sample %s/%s", sm.Driver, sm.Metric)
		}
	}
	return nil
}

func normalizeHistoryPoint(p HistoryPoint) (HistoryPoint, error) {
	for _, v := range []*float64{&p.GridW, &p.PVW, &p.BatW, &p.LoadW, &p.BatSoC} {
		if math.IsNaN(*v) || math.IsInf(*v, 0) {
			return p, errors.New("non-finite history point")
		}
		*v = canonicalHistoryFloat(*v)
	}
	return p, nil
}

// The Go driver materializes each result in native memory. Bounded keyset
// queries avoid retaining a whole table outside DuckDB's buffer budget.
// The caller owns the read transaction when concurrent writes are possible.
type historyQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func scanHistoryTable(ctx context.Context, db historyQueryer, table string, visit func([]any) error) (int64, error) {
	if table != "ts_samples" {
		return scanHistoryPages(ctx, db, table, "", nil, strings.Split(strings.TrimPrefix(historyOrder(table), " ORDER BY "), ", "), visit)
	}
	// Samples are physically grouped by series after migration. Equality on
	// driver/metric plus a timestamp bound lets DuckDB prune row groups.
	groups, err := db.QueryContext(ctx, `SELECT DISTINCT driver_id,metric_id FROM ts_samples ORDER BY driver_id,metric_id`)
	if err != nil {
		return 0, err
	}
	var series [][2]int64
	for groups.Next() {
		var pair [2]int64
		if err := groups.Scan(&pair[0], &pair[1]); err != nil {
			groups.Close()
			return 0, err
		}
		series = append(series, pair)
	}
	err = errors.Join(groups.Err(), groups.Close())
	if err != nil {
		return 0, err
	}
	var total int64
	for _, pair := range series {
		n, err := scanHistoryPages(ctx, db, table, "driver_id=? AND metric_id=?", []any{pair[0], pair[1]}, []string{"ts_ms"}, visit)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func scanHistoryPages(ctx context.Context, db historyQueryer, table, filter string, args []any, keys []string, visit func([]any) error) (int64, error) {
	var total int64
	var cursor []any
	for {
		predicate := filter
		queryArgs := append([]any(nil), args...)
		if cursor != nil {
			if predicate != "" {
				predicate += " AND "
			}
			if len(keys) == 1 {
				predicate += keys[0] + " > ?"
			} else {
				predicate += "(" + strings.Join(keys, ",") + ") > (" + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + ")"
			}
			queryArgs = append(queryArgs, cursor...)
		}
		q := `SELECT * FROM ` + table
		if predicate != "" {
			q += " WHERE " + predicate
		}
		q += " ORDER BY " + strings.Join(keys, ",") + " LIMIT 2048"
		rows, err := db.QueryContext(ctx, q, queryArgs...)
		if err != nil {
			return total, err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return total, err
		}
		positions := make([]int, len(keys))
		for k, key := range keys {
			positions[k] = -1
			for i, col := range cols {
				if col == key {
					positions[k] = i
					break
				}
			}
			if positions[k] < 0 {
				rows.Close()
				return total, fmt.Errorf("missing history key %s", key)
			}
		}
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		count := 0
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return total, err
			}
			if err := visit(values); err != nil {
				rows.Close()
				return total, err
			}
			count++
			total++
		}
		if count > 0 {
			cursor = make([]any, len(keys))
			for i, pos := range positions {
				cursor[i] = values[pos]
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return total, err
		}
		if count < 2048 {
			return total, nil
		}
	}
}
