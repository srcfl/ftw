package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const HistoryFilename = "history.db"

var historyTables = []string{
	"history_hot", "history_warm", "history_cold", "ts_drivers", "ts_metrics", "ts_samples",
	"energy_daily", "energy_ledger_meta", "energy_assets", "energy_ledger_entries", "energy_ledger_cursors",
}
var sqliteHistoryTables = append(append([]string{}, historyTables...), "ts_series_hour", "ts_archive_days")

func ensureHistorySchema(exec func(string) error) error {
	for _, stmt := range historySchema {
		if err := exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// The beta converter publishes a verified SQLite file before selecting its
// generation. Never reopen the frozen pre-beta tables as current history.
func (s *Store) openHistory() error {
	s.historyPath = historyDatabasePath(s.mainDBPath)
	active, err := s.historyConfig("history_sqlite_generation")
	if err != nil {
		return err
	}
	beta, err := s.historyConfig("history_duckdb_generation")
	if err != nil {
		return err
	}
	restore, err := s.historyConfig("history_restore_generation")
	if err != nil {
		return err
	}
	pending, err := s.historyConfig("history_sqlite_pending_generation")
	if err != nil {
		return err
	}
	if beta != "" && active == "" && restore == "" {
		return errors.New("beta history needs conversion: stop Core and run ftw-history-migrate; keep history.duckdb and history-hot.db")
	}
	if _, err := os.Stat(s.historyPath); errors.Is(err, os.ErrNotExist) && active != "" && restore == "" {
		return errors.New("primary SQLite history is missing; restore a full backup")
	}
	db, err := openDurableHistory(s.historyPath)
	if err != nil {
		return err
	}
	s.history = db
	ok := false
	defer func() {
		if !ok {
			db.Close()
			s.history = nil
		}
	}()
	if err := ensureHistorySchema(func(stmt string) error { _, err := db.Exec(stmt); return err }); err != nil {
		return fmt.Errorf("history schema: %w", err)
	}
	var generation string
	err = db.QueryRow(`SELECT name FROM history_migrations WHERE name LIKE 'generation:%'`).Scan(&generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	generation = strings.TrimPrefix(generation, "generation:")
	if restore != "" && generation != "" && generation != restore {
		db.Close()
		suffix := ".before-restore-" + uuid.NewString()
		for _, path := range []string{s.historyPath, s.historyPath + "-wal", s.historyPath + "-shm"} {
			if err := os.Rename(path, path+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		s.history = nil
		ok = true
		return s.openHistory()
	}
	if active != "" && restore == "" && active != generation {
		return errors.New("state and history generations differ; restore a full backup")
	}
	if generation == "" {
		generation = restore
		if generation == "" {
			generation = pending
			if generation == "" {
				generation = uuid.NewString()
			}
			// Persist ownership before the destination can commit its marker.
			// A restart may bind only this generation, never an unrelated file.
			if err := s.SaveConfig("history_sqlite_pending_generation", generation); err != nil {
				return err
			}
		}
		if err := s.migrateSQLiteHistory(context.Background(), generation); err != nil {
			return err
		}
	} else if active == "" && restore == "" && pending != generation {
		return errors.New("unbound SQLite history; complete its migration before starting")
	}
	var complete int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history_migrations WHERE name='sqlite-v1'`).Scan(&complete); err != nil {
		return err
	}
	if complete == 0 && s.historyMigration == nil {
		return errors.New("history import is incomplete; start Core to resume")
	}

	if err := s.SaveConfig("history_sqlite_generation", generation); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM config WHERE key IN ('history_restore_generation','history_migration_generation','history_sqlite_pending_generation')`); err != nil {
		return err
	}
	if err := s.ensureEnergyLedgerVersion(); err != nil {
		return err
	}
	if err := os.Chmod(s.historyPath, 0600); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *Store) migrateSQLiteHistory(ctx context.Context, generation string) error {
	for _, table := range sqliteHistoryTables {
		if s.historyMigration != nil && table == "ts_samples" {
			continue
		}
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		}
		if _, err := s.history.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return err
		}
		if err := copyHistoryTable(ctx, s.db, s.history, table); err != nil {
			return err
		}
	}
	marker := "sqlite-v1"
	if s.historyMigration != nil {
		marker = "sqlite-seed-v1"
	}
	_, err := s.history.ExecContext(ctx, `INSERT INTO history_migrations(name) VALUES (?), (?)`, "generation:"+generation, marker)
	return err
}

// Source is frozen or a read snapshot. The destination is not selected until
// every row has passed a type-aware digest comparison. Commits are bounded.
func copyHistoryTable(ctx context.Context, source historyQueryer, dest *sql.DB, table string) error {
	return copyHistoryTablePaced(ctx, source, dest, table, nil)
}

func copyHistoryTablePaced(ctx context.Context, source historyQueryer, dest *sql.DB, table string, yield func() error) error {
	return copyVerifiedTable(ctx, source, dest, table, scanHistoryTable, yield)
}

type historyScanner func(context.Context, historyQueryer, string, func([]any) error) (int64, error)

func copyVerifiedTable(ctx context.Context, source historyQueryer, dest *sql.DB, table string, scan historyScanner, yield func() error) error {
	expected := sha256.New()
	batchBytes := 1 << 20
	if yield != nil {
		batchBytes = 256 << 10
	}
	var tx *sql.Tx
	var stmt *sql.Stmt
	var pending, pendingBytes int
	defer func() {
		if stmt != nil {
			stmt.Close()
		}
		if tx != nil {
			tx.Rollback()
		}
	}()
	count, err := scan(ctx, source, table, func(values []any) error {
		if tx == nil {
			var err error
			tx, err = dest.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			marks := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
			stmt, err = tx.PrepareContext(ctx, `INSERT INTO `+quoteHistoryIdentifier(table)+` VALUES (`+marks+`)`)
			if err != nil {
				return err
			}
		}
		if err := hashHistoryRow(expected, values); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, values...); err != nil {
			return err
		}
		pending++
		pendingBytes += conversionRowBytes(values)
		if pending == 1024 || pendingBytes >= batchBytes {
			stmt.Close()
			stmt = nil
			if err := tx.Commit(); err != nil {
				return err
			}
			tx = nil
			pending, pendingBytes = 0, 0
			if yield != nil {
				if err := yield(); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("copy %s: %w", table, err)
	}
	if tx != nil {
		stmt.Close()
		stmt = nil
		if err := tx.Commit(); err != nil {
			return err
		}
		tx = nil
	}
	actual := sha256.New()
	got, err := scan(ctx, dest, table, func(v []any) error { return hashHistoryRow(actual, v) })
	if err != nil {
		return err
	}
	if got != count || fmt.Sprintf("%x", actual.Sum(nil)) != fmt.Sprintf("%x", expected.Sum(nil)) {
		return fmt.Errorf("history verification failed for %s", table)
	}
	return nil
}

func appendHistoryRows(conn *sql.Conn, points []HistoryPoint) error {
	stmt, err := conn.PrepareContext(context.Background(), `INSERT OR REPLACE INTO history_hot VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range points {
		p, err = normalizeHistoryPoint(p)
		if err != nil {
			return err
		}
		if _, err = stmt.Exec(p.TsMs, p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC, p.JSON); err != nil {
			return err
		}
	}
	return nil
}

func historyFileHash(path string) (string, error) {
	return historyFileHashContext(context.Background(), path)
}
func historyFileHashContext(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// ImportedHistoryFiles lists only verified legacy sources. Full backups omit
// them because their rows are already in the portable SQLite export; this
// also prevents an older Core from reading each sample twice after restore.
func (s *Store) ImportedHistoryFiles(ctx context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
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
	info := map[string]any{"engine": "sqlite", "archive": "parquet", "file": filepath.Base(s.historyPath), "writer": s.HistoryWriterStatus(), "migration": s.HistoryMigrationStatus(), "series_hour": s.SeriesHourBackfillStatus()}
	for key, path := range map[string]string{"file_bytes": s.historyPath, "wal_bytes": s.historyPath + "-wal"} {
		if stat, err := os.Stat(path); err == nil {
			info[key] = stat.Size()
		}
	}
	return info
}

func (s *Store) CheckpointHistory(ctx context.Context) error {
	if s.history == nil {
		return nil
	}
	_, err := s.history.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	return err
}

func (s *Store) RotateHistory(ctx context.Context) error { return s.CheckpointHistory(ctx) }

func historyOrder(table string) string {
	switch table {
	case "history_hot", "history_warm", "history_cold":
		return " ORDER BY ts_ms"
	case "ts_drivers", "ts_metrics":
		return " ORDER BY id"
	case "ts_archive_days":
		return " ORDER BY path"
	case "ts_series_hour":
		return " ORDER BY driver_id, metric_id, hour_ms"
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
		case []byte:
			h.Write([]byte{4})
			binary.LittleEndian.PutUint64(b[:], uint64(len(v)))
			h.Write(b[:])
			h.Write(v)
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

func (s *Store) exportHistoryToSQLite(ctx context.Context, path string) error {
	if s.history == nil {
		return nil
	}
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
	// This is an unpublished scratch file. Flush each small commit, then
	// pause, so the next goal/session sync does not inherit a dirty backlog.
	dest, err := openBackupDestination(path)
	if err != nil {
		return err
	}
	defer dest.Close()
	dest.SetMaxOpenConns(1)
	if err := ensureHistorySchema(func(q string) error { _, err := dest.ExecContext(ctx, q); return err }); err != nil {
		return err
	}
	for _, table := range sqliteHistoryTables {
		if _, err := dest.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return err
		}
		if err := copyHistoryTablePaced(ctx, src, dest, table, s.backupCopyYield(ctx)); err != nil {
			return err
		}
	}
	tx, err := dest.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM config WHERE key IN ('history_duckdb_generation','history_sqlite_generation','history_sqlite_pending_generation','history_migration_generation',?)`, historyLegacySourcesRetiredKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO config VALUES ('history_restore_generation',?)`, uuid.NewString()); err != nil {
		return err
	}
	return tx.Commit()
}

func historyDatabasePath(statePath string) string {
	name := filepath.Base(statePath)
	if name == "state.db" {
		return filepath.Join(filepath.Dir(statePath), HistoryFilename)
	}
	return filepath.Join(filepath.Dir(statePath), strings.TrimSuffix(name, filepath.Ext(name))+".history.db")
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

// Bounded keyset queries keep backup and migration memory independent of history size.
// The caller owns the read transaction when concurrent writes are possible.
type historyQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func scanHistoryTable(ctx context.Context, db historyQueryer, table string, visit func([]any) error) (int64, error) {
	if table != "ts_samples" {
		return scanHistoryPages(ctx, db, table, "", nil, strings.Split(strings.TrimPrefix(historyOrder(table), " ORDER BY "), ", "), visit)
	}
	// Samples are physically grouped by series after migration. Equality on
	// driver/metric plus a timestamp bound uses the SQLite primary key and
	// permits the offline beta reader to prune its source row groups.
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

// A beta with an unconverted archive must fail before migration or healing
// can change its frozen source. Corruption still goes through the usual gate.
func requireConvertedBeta(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", ReadOnlyDatabaseURI(path))
	if err != nil {
		return nil
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var beta, selected, restore string
	err = db.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM config WHERE key='history_duckdb_generation'),''),COALESCE((SELECT value FROM config WHERE key='history_sqlite_generation'),''),COALESCE((SELECT value FROM config WHERE key='history_restore_generation'),'')`).Scan(&beta, &selected, &restore)
	if err == nil && beta != "" && selected == "" && restore == "" {
		return errors.New("beta history needs conversion: stop Core and run ftw-history-migrate; original sources remain unchanged")
	}
	return nil
}
