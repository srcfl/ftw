package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
)

const archiveBatchRows = 1024
const maintenancePause = 100 * time.Millisecond

// VerifyParquetFile reads every page without retaining the archive in memory.
// It accepts both sample and diagnostic schemas and checks the footer row count.
func VerifyParquetFile(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	pf, err := parquet.OpenFile(f, st.Size())
	if err != nil {
		return err
	}
	r := parquet.NewReader(pf)
	defer r.Close()
	buf := make([]parquet.Row, 64)
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.ReadRows(buf)
		count += int64(n)
		if errors.Is(err, io.EOF) {
			if count != pf.NumRows() {
				return fmt.Errorf("Parquet row count differs: read %d, expected %d", count, pf.NumRows())
			}
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}

// walkParquetRows reads row groups incrementally. Neither callers nor readers
// retain a full day, including days with a large number of device metrics.
func walkParquetRows(ctx context.Context, path string, visit func([]parquetSampleRow) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	pf, err := parquet.OpenFile(f, st.Size())
	if err != nil {
		return err
	}
	r := parquet.NewGenericReader[parquetSampleRow](pf)
	defer r.Close()
	buf := make([]parquetSampleRow, archiveBatchRows)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if e := visit(buf[:n]); e != nil {
				return e
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// A disposable SQLite merge file handles old day files whose equal-timestamp
// rows were not sorted. It bounds both RAM and transaction size, and records
// precisely which live rows may be pruned after verified publication.
func (s *Store) archiveSampleDay(ctx context.Context, coldDir string, from, to int64) (int64, string, error) {
	day := time.UnixMilli(from).UTC()
	dir := filepath.Join(coldDir, day.Format("2006/01"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, "", err
	}
	f, err := os.CreateTemp(dir, ".ftw-samples-*.db")
	if err != nil {
		return 0, "", err
	}
	stagePath := f.Name()
	f.Close()
	defer os.Remove(stagePath)
	defer os.Remove(stagePath + "-journal")
	stage, err := openArchiveStage(stagePath)
	if err != nil {
		return 0, "", err
	}
	defer stage.Close()
	path := filepath.Join(dir, day.Format("02.parquet"))
	if _, err := os.Stat(path); err == nil {
		if err := s.summarizeParquetDay(ctx, path); err != nil {
			return 0, "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, "", err
	}
	err = walkParquetRows(ctx, path, func(rows []parquetSampleRow) error { return insertArchiveRows(ctx, stage, rows) })
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, "", fmt.Errorf("read existing archive: %w", err)
	}
	var lastTs, lastD, lastM int64
	started := false
	var copied int64
	for {
		q := `SELECT s.ts_ms,d.name,m.name,s.value,s.driver_id,s.metric_id FROM ts_samples s JOIN ts_drivers d ON d.id=s.driver_id JOIN ts_metrics m ON m.id=s.metric_id WHERE s.ts_ms>=? AND s.ts_ms<?`
		args := []any{from, to}
		if started {
			q += ` AND (s.ts_ms,s.driver_id,s.metric_id)>(?,?,?)`
			args = append(args, lastTs, lastD, lastM)
		}
		q += ` ORDER BY s.ts_ms,s.driver_id,s.metric_id LIMIT 1024`
		rows, err := s.history.QueryContext(ctx, q, args...)
		if err != nil {
			return copied, "", err
		}
		type row struct {
			parquetSampleRow
			d, m int64
		}
		batch := make([]row, 0, archiveBatchRows)
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.TsMs, &r.Driver, &r.Metric, &r.Value, &r.d, &r.m); err != nil {
				rows.Close()
				return copied, "", err
			}
			batch = append(batch, r)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return copied, "", err
		}
		if len(batch) == 0 {
			break
		}
		tx, err := stage.BeginTx(ctx, nil)
		if err != nil {
			return copied, "", err
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO samples VALUES(?,?,?,?,?,?) ON CONFLICT(ts_ms,driver,metric) DO UPDATE SET value=excluded.value,driver_id=excluded.driver_id,metric_id=excluded.metric_id`)
		if err == nil {
			for _, r := range batch {
				_, err = stmt.ExecContext(ctx, r.TsMs, r.Driver, r.Metric, r.Value, r.d, r.m)
				if err != nil {
					break
				}
			}
			err = errors.Join(err, stmt.Close())
		}
		if err != nil {
			tx.Rollback()
			return copied, "", err
		}
		if err = tx.Commit(); err != nil {
			return copied, "", err
		}
		copied += int64(len(batch))
		last := batch[len(batch)-1]
		lastTs, lastD, lastM, started = last.TsMs, last.d, last.m, true
	}
	if copied == 0 {
		return 0, "", nil
	}
	if err := publishStagedSamples(ctx, path, stage); err != nil {
		return copied, "", err
	}
	// Hourly totals already include live samples and any prior archive. Do
	// not rebuild from this file: a late day can outlive its old raw retention.
	if err := s.markParquetSummary(ctx, path); err != nil {
		return copied, "", err
	}
	var deleted int64
	started = false
	for {
		q := `SELECT ts_ms,driver_id,metric_id,value FROM samples WHERE driver_id IS NOT NULL`
		var args []any
		if started {
			q += ` AND (ts_ms,driver_id,metric_id)>(?,?,?)`
			args = []any{lastTs, lastD, lastM}
		}
		q += ` ORDER BY ts_ms,driver_id,metric_id LIMIT 1024`
		rows, err := stage.QueryContext(ctx, q, args...)
		if err != nil {
			return deleted, "", err
		}
		var batch []resolvedSample
		for rows.Next() {
			var r resolvedSample
			if err := rows.Scan(&r.ts, &r.dID, &r.mID, &r.v); err != nil {
				rows.Close()
				return deleted, "", err
			}
			batch = append(batch, r)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return deleted, "", err
		}
		if len(batch) == 0 {
			break
		}
		// Deleting only copied keys and values preserves late inserts/corrections
		// that arrive while the file is being compressed and verified.
		n, err := s.pruneArchivedSamples(ctx, batch)
		deleted += n
		if err != nil {
			return deleted, "", err
		}
		last := batch[len(batch)-1]
		lastTs, lastD, lastM, started = last.ts, last.dID, last.mID, true
		if err := pauseMaintenance(ctx); err != nil {
			return deleted, "", err
		}
	}
	return deleted, path, nil
}

func publishStagedSamples(ctx context.Context, path string, stage *sql.DB) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".ftw-parquet-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	w := parquet.NewGenericWriter[parquetSampleRow](NewMaintenanceWriter(ctx, f), parquet.Compression(&zstd.Codec{Level: zstd.DefaultLevel}), parquet.MaxRowsPerRowGroup(8192), parquet.PageBufferSize(64<<10))
	rows, err := stage.QueryContext(ctx, `SELECT ts_ms,driver,metric,value FROM samples ORDER BY ts_ms,driver,metric`)
	if err != nil {
		return err
	}
	expected := sha256.New()
	var count int64
	buf := make([]parquetSampleRow, 0, archiveBatchRows)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		_, err := w.Write(buf)
		buf = buf[:0]
		return err
	}
	for rows.Next() {
		var r parquetSampleRow
		if err := rows.Scan(&r.TsMs, &r.Driver, &r.Metric, &r.Value); err != nil {
			rows.Close()
			w.Close()
			return err
		}
		if err := hashHistoryRow(expected, []any{r.TsMs, r.Driver, r.Metric, r.Value}); err != nil {
			rows.Close()
			w.Close()
			return err
		}
		count++
		buf = append(buf, r)
		if len(buf) == cap(buf) {
			if err := flush(); err != nil {
				rows.Close()
				w.Close()
				return err
			}
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		w.Close()
		return err
	}
	if err := flush(); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	verify := func(file string) error {
		actual := sha256.New()
		var n int64
		err := walkParquetRows(ctx, file, func(batch []parquetSampleRow) error {
			for _, r := range batch {
				if err := hashHistoryRow(actual, []any{r.TsMs, r.Driver, r.Metric, r.Value}); err != nil {
					return err
				}
				n++
			}
			return nil
		})
		if err != nil {
			return err
		}
		if n != count || fmt.Sprintf("%x", actual.Sum(nil)) != fmt.Sprintf("%x", expected.Sum(nil)) {
			return errors.New("Parquet readback differs from source")
		}
		return nil
	}
	if err := verify(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return verify(path)
}

func (s *Store) pruneArchivedSamples(ctx context.Context, batch []resolvedSample) (int64, error) {
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	tx, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM ts_samples WHERE driver_id=? AND metric_id=? AND ts_ms=? AND value=?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	var n int64
	for _, r := range batch {
		res, err := stmt.ExecContext(ctx, r.dID, r.mID, r.ts, r.v)
		if err != nil {
			return n, err
		}
		v, err := res.RowsAffected()
		if err != nil {
			return n, err
		}
		n += v
	}
	return n, tx.Commit()
}

func (s *Store) archiveDayHours(ctx context.Context, stage *sql.DB) error {
	rows, err := stage.QueryContext(ctx, `SELECT DISTINCT driver,metric,(ts_ms/3600000)*3600000 FROM samples ORDER BY driver,metric,3`)
	if err != nil {
		return err
	}
	// Release the staging connection before reading the individual hours.
	type key struct {
		driver, metric string
		hour           int64
	}
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.driver, &k.metric, &k.hour); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, k := range keys {
		d, err := s.driverID(k.driver)
		if err != nil {
			return err
		}
		m, err := s.metricID(k.metric, "")
		if err != nil {
			return err
		}
		// One series/hour bounds memory and the writer lock. Read the archive
		// first; under the lock, raw SQLite wins on overlap and adds late rows.
		archived := make(map[int64]float64)
		rows, err := stage.QueryContext(ctx, `SELECT ts_ms,value FROM samples WHERE driver=? AND metric=? AND ts_ms>=? AND ts_ms<?`, k.driver, k.metric, k.hour, k.hour+seriesHourMs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ts int64
			var v float64
			if err := rows.Scan(&ts, &v); err != nil {
				rows.Close()
				return err
			}
			if len(archived) >= maxRawSeriesPoints {
				rows.Close()
				return ErrHistoryQueryLimit
			}
			archived[ts] = v
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		if err := s.mergeArchivedHour(ctx, d, m, k.hour, archived); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) mergeArchivedHour(ctx context.Context, d, m, hour int64, values map[int64]float64) error {
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	tx, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT ts_ms,value FROM ts_samples WHERE driver_id=? AND metric_id=? AND ts_ms>=? AND ts_ms<?`, d, m, hour, hour+seriesHourMs)
	if err != nil {
		return err
	}
	for rows.Next() {
		var ts int64
		var v float64
		if err := rows.Scan(&ts, &v); err != nil {
			rows.Close()
			return err
		}
		if _, exists := values[ts]; !exists && len(values) >= maxRawSeriesPoints {
			rows.Close()
			return ErrHistoryQueryLimit
		}
		values[ts] = v
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	var a seriesBucketAcc
	for ts, v := range values {
		a.add(1, v, v, v, ts)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ts_series_hour VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(driver_id,metric_id,hour_ms) DO UPDATE SET sum_value=excluded.sum_value,min_value=excluded.min_value,max_value=excluded.max_value,n=excluded.n,last_ts_ms=excluded.last_ts_ms`, d, m, hour, a.sum, a.min, a.max, a.n, a.last)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func pauseMaintenance(ctx context.Context) error {
	timer := time.NewTimer(maintenancePause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func openArchiveStage(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(DELETE)&_pragma=synchronous(OFF)&_pragma=cache_size(-2048)&_pragma=temp_store(FILE)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE samples(ts_ms INTEGER NOT NULL,driver TEXT NOT NULL,metric TEXT NOT NULL,value REAL NOT NULL,driver_id INTEGER,metric_id INTEGER,PRIMARY KEY(ts_ms,driver,metric)) WITHOUT ROWID`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
func insertArchiveRows(ctx context.Context, stage *sql.DB, rows []parquetSampleRow) error {
	tx, err := stage.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO samples(ts_ms,driver,metric,value) VALUES(?,?,?,?) ON CONFLICT(ts_ms,driver,metric) DO UPDATE SET value=excluded.value`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.TsMs, r.Driver, r.Metric, r.Value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Temporary merge files are disposable. Keep a full day of grace so a slow
// active operation cannot be mistaken for an interrupted earlier run.
func cleanupArchiveTemps(ctx context.Context, coldDir string, now time.Time) error {
	for _, pattern := range []string{".ftw-samples-*.db*", ".ftw-summary-*.db*", ".ftw-parquet-*.tmp"} {
		paths, err := filepath.Glob(filepath.Join(coldDir, "[0-9][0-9][0-9][0-9]", "[0-9][0-9]", pattern))
		if err != nil {
			return err
		}
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return err
			}
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() && info.ModTime().Before(now.Add(-24*time.Hour)) {
				if err := os.Remove(path); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
