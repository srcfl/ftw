package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/parquet-go/parquet-go"
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
