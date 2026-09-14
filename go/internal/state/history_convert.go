package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ConvertBetaHistory is called by the separate, temporary beta conversion
// executable. Core never loads the old engine. The caller must stop Core and
// hold the source engine's file lock until conversion finishes.
// Original databases and Parquet files are never removed or rewritten here.
func ConvertBetaHistory(ctx context.Context, statePath string, source *sql.DB, report func(string)) error {
	cfg, err := sql.Open("sqlite", statePath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(1000)")
	if err != nil {
		return err
	}
	defer cfg.Close()
	cfg.SetMaxOpenConns(1)
	var generation string
	if err := cfg.QueryRowContext(ctx, `SELECT value FROM config WHERE key='history_duckdb_generation'`).Scan(&generation); err != nil {
		return fmt.Errorf("read beta history generation: %w", err)
	}
	var bound string
	if err := source.QueryRowContext(ctx, `SELECT name FROM history_migrations WHERE name=?`, "generation:"+generation).Scan(&bound); err != nil {
		return errors.New("beta history does not match this site's saved generation")
	}
	destPath := historyDatabasePath(statePath)
	var selected string
	err = cfg.QueryRowContext(ctx, `SELECT value FROM config WHERE key='history_sqlite_generation'`).Scan(&selected)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if selected != "" && selected != generation {
		return errors.New("another SQLite history generation is already selected")
	}
	// A publish followed by a crash before binding is recoverable. Read back
	// the conversion receipt before selecting the already-published file.
	if _, err := os.Stat(destPath); err == nil {
		done, err := sql.Open("sqlite", ReadOnlyDatabaseURI(destPath))
		if err != nil {
			return err
		}
		var receipt string
		err = done.QueryRowContext(ctx, `SELECT name FROM history_migrations WHERE name=?`, "beta-converted:"+generation).Scan(&receipt)
		if err == nil {
			var ok bool
			ok, err = quickCheckContext(ctx, done)
			if err == nil && !ok {
				err = errors.New("published history failed integrity check")
			}
		}
		done.Close()
		if err != nil {
			return errors.New("history.db already exists without a matching conversion receipt; preserve it and investigate")
		}
		if selected == generation {
			return nil
		}
		if err := verifyPublishedConversion(ctx, statePath, destPath, generation); err != nil {
			return err
		}
		return bindConvertedHistory(ctx, cfg, generation)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A source fingerprint rejects restart against a different beta dataset.
	// Hashes stream from disk; file contents and household values stay local.
	if report != nil {
		report("fingerprint beta sources")
	}
	sourceHash, err := betaSourceHash(ctx, statePath)
	if err != nil {
		return err
	}
	tmp := destPath + ".converting"
	dest, err := openRaw(tmp)
	if err != nil {
		return err
	}
	defer dest.Close()
	dest.SetMaxOpenConns(1)
	if err := ensureHistorySchema(func(stmt string) error { _, err := dest.ExecContext(ctx, stmt); return err }); err != nil {
		return err
	}
	if _, err := dest.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS conversion_progress(name TEXT PRIMARY KEY,cursor TEXT NOT NULL); CREATE TABLE IF NOT EXISTS conversion_source(digest TEXT NOT NULL)`); err != nil {
		return err
	}
	var previous string
	err = dest.QueryRowContext(ctx, `SELECT digest FROM conversion_source`).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = dest.ExecContext(ctx, `INSERT INTO conversion_source VALUES(?)`, sourceHash)
	} else if err == nil && previous != sourceHash {
		return errors.New("beta source changed since the interrupted conversion; preserve the partial copy and restart with a new destination")
	}
	if err != nil {
		return err
	}
	for _, table := range historyTables {
		if report != nil {
			report("copy and verify " + table)
		}
		if err := convertHistoryTable(ctx, source, dest, table); err != nil {
			return err
		}
	}
	// Preserve beta summaries even when their original raw rows have expired.
	columns, err := source.QueryContext(ctx, `SELECT * FROM ts_series_hour LIMIT 0`)
	if err == nil {
		columns.Close()
		if report != nil {
			report("copy and verify ts_series_hour")
		}
		if err := convertHistoryTable(ctx, source, dest, "ts_series_hour"); err != nil {
			return err
		}
	} else if !strings.Contains(err.Error(), "does not exist") && !strings.Contains(err.Error(), "no such table") {
		return err
	}
	// Incomplete old imports still have frozen SQLite sources. Missing keys
	// supplement the beta copy; the selected beta value wins on overlap.
	for _, table := range historyTables {
		var exists int
		if err := cfg.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		}
		if err := mergeLegacyConversionTable(ctx, cfg, dest, table); err != nil {
			return err
		}
	}
	hotPath := hotHistoryPath(statePath)
	if _, err := os.Stat(hotPath); err == nil {
		hot, err := sql.Open("sqlite", ReadOnlyDatabaseURI(hotPath))
		if err != nil {
			return err
		}
		err = mergeBetaHotHistory(ctx, hot, dest)
		hot.Close()
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	afterHash, err := betaSourceHash(ctx, statePath)
	if err != nil {
		return err
	}
	if afterHash != sourceHash {
		return errors.New("beta source changed during conversion; stop all writers and preserve the partial copy")
	}
	// Hourly aggregates are rebuilt from all samples in Core. This does not
	// reinterpret counters as energy; the copied ledger retains its provenance.
	if _, err := dest.ExecContext(ctx, `INSERT OR IGNORE INTO history_migrations(name) VALUES (?),('sqlite-v1'),(?)`, "generation:"+generation, "beta-converted:"+generation); err != nil {
		return err
	}
	if ok, err := quickCheckContext(ctx, dest); err != nil || !ok {
		return fmt.Errorf("converted history integrity check: ok=%v: %w", ok, err)
	}
	if _, err := dest.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	if err := dest.Close(); err != nil {
		return err
	}
	// Windows FlushFileBuffers requires a writable handle. This is the new
	// destination, never one of the read-only conversion sources.
	f, err := os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		return err
	}
	if err := writeConversionReceipt(ctx, tmp, destPath, sourceHash, generation); err != nil {
		return err
	}
	if err := os.Rename(tmp, destPath); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(destPath)); err != nil {
		return err
	}
	if report != nil {
		report("published verified history")
	}
	return bindConvertedHistory(ctx, cfg, generation)
}

func bindConvertedHistory(ctx context.Context, cfg *sql.DB, generation string) error {
	_, err := cfg.ExecContext(ctx, `INSERT INTO config(key,value) VALUES('history_sqlite_generation',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, generation)
	return err
}

func hotHistoryPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), HotHistoryFilename)
}

// Each progress key and its rows commit together. A retry starts after the
// last copied primary key. Final source/destination hashes check all rows,
// including those copied by previous attempts.
func convertHistoryTable(ctx context.Context, src, dst *sql.DB, table string) error {

	var verified int
	if err := dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversion_progress WHERE name=?`, table+":verified").Scan(&verified); err != nil {
		return err
	}
	if verified > 0 {
		return nil
	}

	var engineVersion string
	if err := src.QueryRowContext(ctx, `SELECT version()`).Scan(&engineVersion); err == nil && strings.HasPrefix(engineVersion, "v") {
		return convertPhysicalBetaTable(ctx, src, dst, table)
	}
	keys := strings.Split(strings.TrimPrefix(historyOrder(table), " ORDER BY "), ", ")
	if table == "ts_samples" {
		groups, err := src.QueryContext(ctx, `SELECT DISTINCT driver_id,metric_id FROM ts_samples ORDER BY driver_id,metric_id`)
		if err != nil {
			return err
		}
		var pairs [][2]int64
		for groups.Next() {
			var pair [2]int64
			if err := groups.Scan(&pair[0], &pair[1]); err != nil {
				groups.Close()
				return err
			}
			pairs = append(pairs, pair)
		}
		err = errors.Join(groups.Err(), groups.Close())
		if err != nil {
			return err
		}
		for _, pair := range pairs {
			name := fmt.Sprintf("%s:%d:%d", table, pair[0], pair[1])
			if err := convertHistoryRange(ctx, src, dst, table, name, "driver_id=? AND metric_id=?", []any{pair[0], pair[1]}, []string{"ts_ms"}); err != nil {
				return err
			}
		}
	} else if err := convertHistoryRange(ctx, src, dst, table, table, "", nil, keys); err != nil {
		return err
	}
	expected, actual := sha256.New(), sha256.New()
	n, err := scanHistoryTable(ctx, src, table, func(v []any) error { return hashHistoryRow(expected, v) })
	if err != nil {
		return err
	}
	m, err := scanHistoryTable(ctx, dst, table, func(v []any) error { return hashHistoryRow(actual, v) })
	if err != nil {
		return err
	}
	if n != m || fmt.Sprintf("%x", expected.Sum(nil)) != fmt.Sprintf("%x", actual.Sum(nil)) {
		return fmt.Errorf("beta readback verification differs for %s", table)
	}
	_, err = dst.ExecContext(ctx, `INSERT OR REPLACE INTO conversion_progress VALUES(?,?)`, table+":verified", "true")
	return err
}

func mergeLegacyConversionTable(ctx context.Context, src, dst *sql.DB, table string) error {
	// Catalog IDs in the frozen SQLite source are the beta seed. Verify names
	// before copying samples so a mismatched catalog cannot relabel telemetry.
	if table == "ts_drivers" || table == "ts_metrics" {
		rows, err := src.QueryContext(ctx, `SELECT id,name FROM `+table)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var name, got string
			if err := rows.Scan(&id, &name); err != nil {
				return err
			}
			err := dst.QueryRowContext(ctx, `SELECT name FROM `+table+` WHERE id=?`, id).Scan(&got)
			if err == nil && got != name {
				return errors.New("legacy and beta metric catalogs conflict")
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	w := newConversionWriter(ctx, dst, "legacy:"+table)
	defer w.close()
	if w.complete() {
		return nil
	}
	_, err := scanHistoryTable(ctx, src, table, func(v []any) error {
		var sample resolvedSample
		if table == "ts_samples" {
			if len(v) != 4 {
				return errors.New("legacy sample has an unexpected column count")
			}
			driver, driverOK := v[0].(int64)
			metric, metricOK := v[1].(int64)
			ts, tsOK := v[2].(int64)
			value, valueOK := v[3].(float64)
			if !driverOK || !metricOK || !tsOK || !valueOK {
				return errors.New("legacy sample has invalid value types")
			}
			sample = resolvedSample{dID: driver, mID: metric, ts: ts, v: value}
		}
		return w.write(func(tx *sql.Tx) error {
			if table == "ts_samples" {
				var present int
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ts_samples WHERE driver_id=? AND metric_id=? AND ts_ms=?)`, v[0], v[1], v[2]).Scan(&present); err != nil {
					return err
				}
				if present > 0 {
					return nil
				}
				store := &Store{}
				if err := store.insertSamplesAndHours(ctx, tx, []resolvedSample{sample}); err != nil {
					return err
				}
				return verifyConversionRow(ctx, tx, table, v)
			}
			marks := strings.TrimSuffix(strings.Repeat("?,", len(v)), ",")
			res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO `+table+` VALUES (`+marks+`)`, v...)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
			return verifyConversionRow(ctx, tx, table, v)
		}, conversionRowBytes(v))
	})
	if err != nil {
		return err
	}
	return w.finish()
}

func mergeBetaHotHistory(ctx context.Context, hot, dst *sql.DB) error {
	// Beta's hot catalog uses different integer IDs. Names, never IDs, join it.
	for _, table := range []string{"ts_drivers", "ts_metrics"} {
		var q string
		if table == "ts_drivers" {
			q = `SELECT name FROM ts_drivers`
		} else {
			q = `SELECT name,COALESCE(unit,'') FROM ts_metrics`
		}
		rows, err := hot.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name, unit string
			if table == "ts_drivers" {
				err = rows.Scan(&name)
			} else {
				err = rows.Scan(&name, &unit)
			}
			if err != nil {
				rows.Close()
				return err
			}
			if table == "ts_drivers" {
				_, err = dst.ExecContext(ctx, `INSERT OR IGNORE INTO ts_drivers(name) VALUES(?)`, name)
			} else {
				_, err = dst.ExecContext(ctx, `INSERT INTO ts_metrics(name,unit) VALUES(?,NULLIF(?,'')) ON CONFLICT(name) DO UPDATE SET unit=COALESCE(excluded.unit,ts_metrics.unit)`, name, unit)
			}
			if err != nil {
				rows.Close()
				return err
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
	}
	for _, table := range []string{"history_hot", "ts_samples"} {
		w := newConversionWriter(ctx, dst, "hot:"+table)
		defer w.close()
		if w.complete() {
			continue
		}
		query := `SELECT ts_ms,grid_w,pv_w,bat_w,load_w,bat_soc,json FROM history_hot ORDER BY ts_ms`
		if table == "ts_samples" {
			query = `SELECT s.ts_ms,d.name,m.name,s.value FROM ts_samples s JOIN ts_drivers d ON d.id=s.driver_id JOIN ts_metrics m ON m.id=s.metric_id ORDER BY s.ts_ms`
		}
		rows, err := hot.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		for rows.Next() {
			if table == "history_hot" {
				var ts int64
				var grid, pv, bat, load, soc sql.NullFloat64
				var jsonText string
				err = rows.Scan(&ts, &grid, &pv, &bat, &load, &soc, &jsonText)
				if err == nil {
					err = w.write(func(tx *sql.Tx) error {
						_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO history_hot VALUES(?,?,?,?,?,?,?)`, ts, grid, pv, bat, load, soc, jsonText)
						if err != nil {
							return err
						}
						var got string
						if err := tx.QueryRowContext(ctx, `SELECT json FROM history_hot WHERE ts_ms=?`, ts).Scan(&got); err != nil {
							return err
						}
						if got != jsonText {
							return errors.New("hot snapshot readback differs")
						}
						return nil
					}, len(jsonText)+64)
				}
			} else {
				var ts int64
				var d, m string
				var v float64
				err = rows.Scan(&ts, &d, &m, &v)
				if err == nil {
					err = w.write(func(tx *sql.Tx) error {
						var dID, mID int64
						if err := tx.QueryRowContext(ctx, `SELECT d.id,m.id FROM ts_drivers d,ts_metrics m WHERE d.name=? AND m.name=?`, d, m).Scan(&dID, &mID); err != nil {
							return err
						}
						var present int
						if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ts_samples WHERE driver_id=? AND metric_id=? AND ts_ms=?)`, dID, mID, ts).Scan(&present); err != nil {
							return err
						}
						store := &Store{}
						if err := store.insertSamplesAndHours(ctx, tx, []resolvedSample{{dID: dID, mID: mID, ts: ts, v: v}}); err != nil {
							return err
						}
						var got float64
						if err := tx.QueryRowContext(ctx, `SELECT value FROM ts_samples WHERE driver_id=? AND metric_id=? AND ts_ms=?`, dID, mID, ts).Scan(&got); err != nil {
							return err
						}
						if present == 0 && got != v {
							return errors.New("hot sample readback differs")
						}

						return nil
					}, 64)
				}
			}
			if err != nil {
				rows.Close()
				return err
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		if err := w.finish(); err != nil {
			return err
		}
	}
	w := newConversionWriter(ctx, dst, "hot:ledger")
	defer w.close()
	if w.complete() {
		return nil
	}
	rows, err := hot.QueryContext(ctx, `SELECT payload FROM hot_ticks ORDER BY ts_ms,id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return err
		}
		var p historyPayload
		if err := json.Unmarshal([]byte(text), &p); err != nil {
			return err
		}
		if err := w.write(func(tx *sql.Tx) error { return recordEnergyObservationsTx(tx, p.Observations) }, len(text)); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return w.finish()
}

func convertHistoryRange(ctx context.Context, src, dst *sql.DB, table, progressName, predicate string, fixedArgs []any, keys []string) error {
	var textCursor string
	err := dst.QueryRowContext(ctx, `SELECT cursor FROM conversion_progress WHERE name=?`, progressName).Scan(&textCursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var cursor []any
	if textCursor != "" {
		decoder := json.NewDecoder(strings.NewReader(textCursor))
		decoder.UseNumber()
		if err := decoder.Decode(&cursor); err != nil {
			return err
		}
		for i, v := range cursor {
			if n, ok := v.(json.Number); ok {
				v, err := n.Int64()
				if err != nil {
					return err
				}
				cursor[i] = v
			}
		}
	}
	for {
		q := `SELECT * FROM ` + table
		where := predicate
		args := append([]any{}, fixedArgs...)
		if cursor != nil {
			if where != "" {
				where += " AND "
			}
			where += `(` + strings.Join(keys, ",") + `) > (` + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + `)`
			args = append(args, cursor...)
		}
		if where != "" {
			q += " WHERE " + where
		}
		q += ` ORDER BY ` + strings.Join(keys, ",") + ` LIMIT 1024`
		rows, err := src.QueryContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("read beta %s: %w", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		var batch [][]any
		for rows.Next() {
			v := make([]any, len(columns))
			ptr := make([]any, len(v))
			for i := range v {
				ptr[i] = &v[i]
			}
			if err := rows.Scan(ptr...); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, v)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		tx, err := dst.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		// Meta tables contain their schema seed before conversion.
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO `+table+` VALUES (`+strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")+`)`)
		if err == nil {
			for _, v := range batch {
				if _, err = stmt.ExecContext(ctx, v...); err != nil {
					break
				}
			}
			err = errors.Join(err, stmt.Close())
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		last := batch[len(batch)-1]
		cursor = make([]any, len(keys))
		for k, key := range keys {
			found := false
			for i, col := range columns {
				if col == key {
					cursor[k] = last[i]
					found = true
					break
				}
			}
			if !found {
				tx.Rollback()
				return fmt.Errorf("missing conversion key %s", key)
			}
		}
		encoded, err := json.Marshal(cursor)
		if err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversion_progress VALUES(?,?) ON CONFLICT(name) DO UPDATE SET cursor=excluded.cursor`, progressName, string(encoded)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	return nil
}

// Completed phases are skipped on retry. Within a phase, bounded transactions
// retain copied data; replay is idempotent after interruption.
type conversionWriter struct {
	ctx         context.Context
	db          *sql.DB
	tx          *sql.Tx
	name        string
	rows, bytes int
}

func newConversionWriter(ctx context.Context, db *sql.DB, name string) *conversionWriter {
	return &conversionWriter{ctx: ctx, db: db, name: name}
}
func (w *conversionWriter) complete() bool {
	var n int
	return w.db.QueryRowContext(w.ctx, `SELECT COUNT(*) FROM conversion_progress WHERE name=?`, w.name+":complete").Scan(&n) == nil && n > 0
}
func (w *conversionWriter) close() {
	if w.tx != nil {
		w.tx.Rollback()
		w.tx = nil
	}
}
func (w *conversionWriter) write(apply func(*sql.Tx) error, bytes int) error {
	if w.tx == nil {
		var err error
		w.tx, err = w.db.BeginTx(w.ctx, nil)
		if err != nil {
			return err
		}
	}
	if err := apply(w.tx); err != nil {
		return err
	}
	w.rows++
	w.bytes += bytes
	if w.rows >= 1024 || w.bytes >= 4<<20 {
		return w.flush()
	}
	return nil
}
func (w *conversionWriter) flush() error {
	if w.tx == nil {
		return nil
	}
	err := w.tx.Commit()
	w.tx = nil
	w.rows = 0
	w.bytes = 0
	return err
}
func (w *conversionWriter) finish() error {
	if err := w.flush(); err != nil {
		return err
	}
	_, err := w.db.ExecContext(w.ctx, `INSERT OR REPLACE INTO conversion_progress VALUES(?,'true')`, w.name+":complete")
	return err
}
func conversionRowBytes(v []any) int {
	n := 0
	for _, value := range v {
		switch x := value.(type) {
		case string:
			n += len(x)
		case []byte:
			n += len(x)
		default:
			n += 8
		}
	}
	return n
}
func verifyConversionRow(ctx context.Context, tx *sql.Tx, table string, values []any) error {
	cols, err := tx.QueryContext(ctx, `SELECT * FROM `+table+` LIMIT 0`)
	if err != nil {
		return err
	}
	names, err := cols.Columns()
	cols.Close()
	if err != nil {
		return err
	}
	keys := strings.Split(strings.TrimPrefix(historyOrder(table), " ORDER BY "), ", ")
	var where []string
	var args []any
	for _, key := range keys {
		for i, name := range names {
			if name == key {
				where = append(where, key+"=?")
				args = append(args, values[i])
				break
			}
		}
	}
	got := make([]any, len(values))
	ptr := make([]any, len(values))
	for i := range got {
		ptr[i] = &got[i]
	}
	if err := tx.QueryRowContext(ctx, `SELECT * FROM `+table+` WHERE `+strings.Join(where, " AND "), args...).Scan(ptr...); err != nil {
		return err
	}
	a, b := sha256.New(), sha256.New()
	if err := hashHistoryRow(a, values); err != nil {
		return err
	}
	if err := hashHistoryRow(b, got); err != nil {
		return err
	}
	if fmt.Sprintf("%x", a.Sum(nil)) != fmt.Sprintf("%x", b.Sum(nil)) {
		return fmt.Errorf("merged %s readback differs", table)
	}
	return nil
}
func BetaHistoryDatabasePath(statePath string) string {
	if filepath.Base(statePath) == "state.db" {
		return filepath.Join(filepath.Dir(statePath), "history.duckdb")
	}
	return strings.TrimSuffix(statePath, filepath.Ext(statePath)) + ".history.duckdb"
}
func betaSourceHash(ctx context.Context, statePath string) (string, error) {
	oldPath := BetaHistoryDatabasePath(statePath)
	fingerprint := sha256.New()
	for _, path := range []string{statePath, statePath + "-wal", oldPath, oldPath + ".wal", hotHistoryPath(statePath), hotHistoryPath(statePath) + "-wal"} {
		digest, err := historyFileHashContext(ctx, path)
		if errors.Is(err, os.ErrNotExist) {
			digest = "absent"
		} else if err != nil {
			return "", err
		}
		fmt.Fprintf(fingerprint, "%s:%s\n", filepath.Base(path), digest)
	}
	return fmt.Sprintf("%x", fingerprint.Sum(nil)), nil
}

type conversionReceipt struct {
	Generation    string `json:"generation"`
	SourceSHA256  string `json:"source_sha256"`
	HistorySHA256 string `json:"history_sha256"`
}

func writeConversionReceipt(ctx context.Context, tmp, dest, sourceHash, generation string) error {
	digest, err := historyFileHashContext(ctx, tmp)
	if err != nil {
		return err
	}
	data, err := json.Marshal(conversionReceipt{generation, sourceHash, digest})
	if err != nil {
		return err
	}
	path := dest + ".conversion.json"
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(path + ".tmp")
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func verifyPublishedConversion(ctx context.Context, statePath, dest, generation string) error {
	data, err := os.ReadFile(dest + ".conversion.json")
	if err != nil {
		return fmt.Errorf("read conversion receipt: %w", err)
	}
	var receipt conversionReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return err
	}
	sourceHash, err := betaSourceHash(ctx, statePath)
	if err != nil {
		return err
	}
	digest, err := historyFileHashContext(ctx, dest)
	if err != nil {
		return err
	}
	if receipt.Generation != generation || receipt.SourceSHA256 != sourceHash || receipt.HistorySHA256 != digest {
		return errors.New("published conversion differs from its verified source or destination; preserve both copies")
	}
	return nil
}

// A row-ID range scans the frozen beta file once without repeatedly sorting
// its full interleaved series. The physical cursor and destination rows commit
// together. Source fingerprints prevent resuming against different row IDs.
func convertPhysicalBetaTable(ctx context.Context, src, dst *sql.DB, table string) error {
	var start, end sql.NullInt64
	if err := src.QueryRowContext(ctx, `SELECT MIN(rowid),MAX(rowid) FROM `+quoteHistoryIdentifier(table)).Scan(&start, &end); err != nil {
		return err
	}
	name := table + ":physical"
	var saved string
	err := dst.QueryRowContext(ctx, `SELECT cursor FROM conversion_progress WHERE name=?`, name).Scan(&saved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	cursor := start.Int64
	if saved != "" {
		cursor, err = strconv.ParseInt(saved, 10, 64)
		if err != nil {
			return err
		}
	}
	if start.Valid {
		for cursor <= end.Int64 {
			until := min(cursor+1024, end.Int64+1)
			rows, err := src.QueryContext(ctx, `SELECT * FROM `+quoteHistoryIdentifier(table)+` WHERE rowid>=? AND rowid<?`, cursor, until)
			if err != nil {
				return err
			}
			columns, err := rows.Columns()
			if err != nil {
				rows.Close()
				return err
			}
			tx, err := dst.BeginTx(ctx, nil)
			if err != nil {
				rows.Close()
				return err
			}
			stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO `+quoteHistoryIdentifier(table)+` VALUES (`+strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")+`)`)
			if err != nil {
				rows.Close()
				tx.Rollback()
				return err
			}
			values := make([]any, len(columns))
			ptr := make([]any, len(columns))
			for i := range values {
				ptr[i] = &values[i]
			}
			for rows.Next() {
				if err = rows.Scan(ptr...); err != nil {
					break
				}
				if _, err = stmt.ExecContext(ctx, values...); err != nil {
					break
				}
			}
			err = errors.Join(err, rows.Err(), rows.Close(), stmt.Close())
			if err == nil {
				_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO conversion_progress VALUES(?,?)`, name, strconv.FormatInt(until, 10))
			}
			if err != nil {
				tx.Rollback()
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			cursor = until
		}
	}
	// A multiset digest lets the sequential physical scan and SQLite's
	// primary-key scan differ in order. Each typed row includes its full key;
	// counts and a 256-bit sum of SHA-256 row hashes must both match.
	var expected, actual historyMultisetDigest
	if start.Valid {
		for from := start.Int64; from <= end.Int64; from += 1024 {
			rows, err := src.QueryContext(ctx, `SELECT * FROM `+quoteHistoryIdentifier(table)+` WHERE rowid>=? AND rowid<?`, from, from+1024)
			if err != nil {
				return err
			}
			columns, err := rows.Columns()
			if err != nil {
				rows.Close()
				return err
			}
			values := make([]any, len(columns))
			ptr := make([]any, len(columns))
			for i := range values {
				ptr[i] = &values[i]
			}
			for rows.Next() {
				if err = rows.Scan(ptr...); err != nil {
					break
				}
				if err = expected.add(values); err != nil {
					break
				}
			}
			err = errors.Join(err, rows.Err(), rows.Close())
			if err != nil {
				return err
			}
		}
	}
	if _, err := scanHistoryTable(ctx, dst, table, actual.add); err != nil {
		return err
	}
	if expected != actual {
		return fmt.Errorf("beta readback verification differs for %s", table)
	}
	_, err = dst.ExecContext(ctx, `INSERT OR REPLACE INTO conversion_progress VALUES(?,'true')`, table+":verified")
	return err
}

type historyMultisetDigest struct {
	count uint64
	sum   [4]uint64
}

func (d *historyMultisetDigest) add(row []any) error {
	h := sha256.New()
	if err := hashHistoryRow(h, row); err != nil {
		return err
	}
	value := h.Sum(nil)
	var carry uint64
	for i := range d.sum {
		d.sum[i], carry = bits.Add64(d.sum[i], binary.LittleEndian.Uint64(value[i*8:]), carry)
	}
	d.count++
	return nil
}
