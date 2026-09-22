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

func (s *Store) publishStagedSamples(ctx context.Context, path string, stage *sql.DB) error {
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
	if err := s.replaceArchive(ctx, tmp, path); err != nil {
		return err
	}
	return verify(path)
}

func (s *Store) pruneArchivedSamples(ctx context.Context, batch []resolvedSample) (int64, error) {
	var deleted int64
	limit := 1024
	for len(batch) > 0 {
		n := min(len(batch), limit)
		var removed int64
		err := s.writeArchiveBatch(ctx, func(ctx context.Context, tx *sql.Tx) error {
			removed = 0 // A busy attempt rolls back and retries this same prefix.
			stmt, err := tx.PrepareContext(ctx, `DELETE FROM ts_samples WHERE driver_id=? AND metric_id=? AND ts_ms=? AND value=?`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, r := range batch[:n] {
				res, err := stmt.ExecContext(ctx, r.dID, r.mID, r.ts, r.v)
				if err != nil {
					return err
				}
				count, err := res.RowsAffected()
				if err != nil {
					return err
				}
				removed += count
			}
			return nil
		})
		if err != nil {
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				// Even one row can meet a busy reader or a slow sync. Retain
				// this verified prefix instead of staging the whole day again.
				limit = max(1, n/2)
				if err := pauseMaintenance(ctx); err != nil {
					return deleted, err
				}
				continue
			}
			return deleted, err
		}
		deleted += removed
		batch = batch[n:]
		if len(batch) > 0 && s.HistoryWriterStatus().Pending >= historyCommitMaxTicks/2 {
			if err := pauseMaintenance(ctx); err != nil {
				return deleted, err
			}
		}
	}
	return deleted, nil
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
	var a seriesBucketAcc
	return s.archiveTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// A retry must reread raw values instead of retaining an earlier snapshot.
		merged := make(map[int64]float64, len(values))
		for ts, v := range values {
			merged[ts] = v
		}
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
			if _, exists := merged[ts]; !exists && len(merged) >= maxRawSeriesPoints {
				rows.Close()
				return ErrHistoryQueryLimit
			}
			merged[ts] = v
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		a = seriesBucketAcc{}
		for ts, v := range merged {
			a.add(1, v, v, v, ts)
		}
		// Raw archive rebuilding must retain the separate contributions that
		// Core has already stored as aggregates, including after their archive.
		var n, last int64
		var sum, lo, hi float64
		err = tx.QueryRowContext(ctx, `SELECT n,sum_value,min_value,max_value,last_ts_ms FROM ts_aggregate_hours WHERE driver_id=? AND metric_id=? AND hour_ms=?`, d, m, hour).Scan(&n, &sum, &lo, &hi, &last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			a.add(n, sum, lo, hi, last)
		}

		return nil
	}, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO ts_series_hour VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(driver_id,metric_id,hour_ms) DO UPDATE SET sum_value=excluded.sum_value,min_value=excluded.min_value,max_value=excluded.max_value,n=excluded.n,last_ts_ms=excluded.last_ts_ms`, d, m, hour, a.sum, a.min, a.max, a.n, a.last)
		if err != nil {
			return err
		}
		return nil
	})
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
