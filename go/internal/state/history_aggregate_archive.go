package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
)

const bucketStageSchema = `CREATE TABLE IF NOT EXISTS buckets(driver TEXT NOT NULL,metric TEXT NOT NULL,start_ms INTEGER NOT NULL,resolution_ms INTEGER NOT NULL,first_ms INTEGER NOT NULL,last_ms INTEGER NOT NULL,n INTEGER NOT NULL,sum_value REAL NOT NULL,min_value REAL NOT NULL,max_value REAL NOT NULL,last_value REAL NOT NULL,PRIMARY KEY(driver,metric,start_ms,resolution_ms)) WITHOUT ROWID`
const bucketColumns = `start_ms,resolution_ms,first_ms,last_ms,n,sum_value,min_value,max_value,last_value`

func aggregatePaths(cold string, since, until int64) ([]string, error) {
	if cold == "" {
		return nil, nil
	}
	paths, err := filepath.Glob(filepath.Join(cold, "[0-9][0-9][0-9][0-9]", "[0-9][0-9]", "[0-9][0-9].buckets.parquet"))
	if err != nil {
		return nil, err
	}
	legacy, err := filepath.Glob(filepath.Join(cold, "[0-9][0-9][0-9][0-9]", "[0-9][0-9]", "[0-9][0-9].legacy-buckets.parquet"))
	if err != nil {
		return nil, err
	}
	paths = append(paths, legacy...)
	out := paths[:0]
	for _, p := range paths {
		day, err := aggregateDay(p)
		if err != nil {
			return nil, err
		}
		if day.UnixMilli() <= until && day.Add(24*time.Hour).UnixMilli() > since {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func aggregateDay(path string) (time.Time, error) {
	base := filepath.Base(path)
	if len(base) < 2 {
		return time.Time{}, errors.New("invalid aggregate filename")
	}
	return time.Parse("2006/01/02", filepath.Base(filepath.Dir(filepath.Dir(path)))+"/"+filepath.Base(filepath.Dir(path))+"/"+base[:2])
}

func walkBucketFile(ctx context.Context, path string, visit func(metricBucket) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	pf, err := parquet.OpenFile(f, info.Size())
	if err != nil {
		return err
	}
	r := parquet.NewGenericReader[metricBucket](pf)
	defer r.Close()
	rows := make([]metricBucket, 256)
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(rows)
		for _, b := range rows[:n] {
			if b.Driver == "" || b.Metric == "" {
				return errors.New("missing aggregate identity")
			}
			if e := b.validate(); e != nil {
				return e
			}
			if e := visit(b); e != nil {
				return e
			}
		}
		count += int64(n)
		if errors.Is(err, io.EOF) {
			if count != pf.NumRows() {
				return errors.New("aggregate row count differs")
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

func stageBuckets(ctx context.Context, db *sql.DB, rows []metricBucket, merge bool) error {
	return stageBucketsProgress(ctx, db, rows, merge, -1)
}

func stageBucketsProgress(ctx context.Context, db *sql.DB, rows []metricBucket, merge bool, cursor int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	suffix := `DO UPDATE SET first_ms=excluded.first_ms,last_ms=excluded.last_ms,n=excluded.n,sum_value=excluded.sum_value,min_value=excluded.min_value,max_value=excluded.max_value,last_value=excluded.last_value`
	if merge {
		suffix = `DO UPDATE SET first_ms=MIN(first_ms,excluded.first_ms),last_ms=MAX(last_ms,excluded.last_ms),last_value=CASE WHEN excluded.last_ms>=last_ms THEN excluded.last_value ELSE last_value END,n=n+excluded.n,sum_value=sum_value+excluded.sum_value,min_value=MIN(min_value,excluded.min_value),max_value=MAX(max_value,excluded.max_value)`
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO buckets VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(driver,metric,start_ms,resolution_ms) `+suffix)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, b := range rows {
		if err := b.validate(); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, b.Driver, b.Metric, b.StartMS, b.ResolutionMS, b.FirstMS, b.LastMS, b.N, b.Sum, b.Min, b.Max, b.Last); err != nil {
			return err
		}
	}
	if cursor >= 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE bucket_progress SET rows_done=? WHERE id=1`, cursor); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanStagedBuckets(ctx context.Context, db *sql.DB, visit func(metricBucket) error) error {
	rows, err := db.QueryContext(ctx, `SELECT driver,metric,`+bucketColumns+` FROM buckets ORDER BY driver,metric,start_ms,resolution_ms`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b metricBucket
		if err := rows.Scan(&b.Driver, &b.Metric, &b.StartMS, &b.ResolutionMS, &b.FirstMS, &b.LastMS, &b.N, &b.Sum, &b.Min, &b.Max, &b.Last); err != nil {
			return err
		}
		if err := visit(b); err != nil {
			return err
		}
	}
	return rows.Err()
}

func newBucketStage(dir string) (*sql.DB, string, error) {
	f, err := os.CreateTemp(dir, ".ftw-buckets-*.db")
	if err != nil {
		return nil, "", err
	}
	path := f.Name()
	f.Close()
	db, err := openBackupDestination(path)
	if err == nil {
		_, err = db.Exec(bucketStageSchema)
	}
	if err != nil {
		if db != nil {
			db.Close()
		}
		os.Remove(path)
		return nil, "", err
	}
	db.SetMaxOpenConns(1)
	return db, path, nil
}
func removeBucketStage(db *sql.DB, path string) {
	db.Close()
	os.Remove(path)
	os.Remove(path + "-journal")
}

// Compare the complete ordered summary stream after reading the written file
// back, then publish by durable rename. Source pruning is a separate step.
func publishBucketStage(ctx context.Context, db *sql.DB, path string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".ftw-buckets-*.parquet.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	var width, maxWidth int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MIN(resolution_ms),0),COALESCE(MAX(resolution_ms),0) FROM buckets`).Scan(&width, &maxWidth); err != nil {
		return err
	}
	if width == 0 || width != maxWidth {
		return errors.New("archive requires one nonempty resolution")
	}
	writer := parquet.NewGenericWriter[metricBucket](NewMaintenanceWriter(ctx, f), parquet.Compression(&zstd.Codec{}), parquet.MaxRowsPerRowGroup(1024), parquet.KeyValueMetadata("ftw_resolution_ms", strconv.FormatInt(width, 10)))
	expected := sha256.New()
	enc := json.NewEncoder(expected)
	batch := make([]metricBucket, 0, 256)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := writer.Write(batch)
		batch = batch[:0]
		return err
	}
	err = scanStagedBuckets(ctx, db, func(b metricBucket) error {
		if err := enc.Encode(b); err != nil {
			return err
		}
		batch = append(batch, b)
		if len(batch) == cap(batch) {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	err = errors.Join(err, writer.Close())
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	actual := sha256.New()
	out := json.NewEncoder(actual)
	if err := walkBucketFile(ctx, tmp, func(b metricBucket) error { return out.Encode(b) }); err != nil {
		return err
	}
	if fmt.Sprintf("%x", actual.Sum(nil)) != fmt.Sprintf("%x", expected.Sum(nil)) {
		return errors.New("aggregate verification differs from source summaries")
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (s *Store) pruneBucketMinutes(ctx context.Context, rows []metricBucket) error {
	limit := len(rows)
	for len(rows) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(limit, len(rows))
		err := s.tryArchiveBatch(ctx, nil, func(ctx context.Context, tx *sql.Tx) error {
			for _, b := range rows[:n] {
				var d, m int64
				if err := tx.QueryRowContext(ctx, `SELECT d.id,m.id FROM ts_drivers d,ts_metrics m WHERE d.name=? AND m.name=?`, b.Driver, b.Metric).Scan(&d, &m); err != nil {
					return err
				}
				r, err := tx.ExecContext(ctx, `DELETE FROM ts_buckets WHERE driver_id=? AND metric_id=? AND start_ms=? AND resolution_ms=? AND first_ms=? AND last_ms=? AND n=? AND sum_value=? AND min_value=? AND max_value=? AND last_value=?`, d, m, b.StartMS, b.ResolutionMS, b.FirstMS, b.LastMS, b.N, b.Sum, b.Min, b.Max, b.Last)
				if err != nil {
					return err
				}
				deleted, err := r.RowsAffected()
				if err != nil {
					return err
				}
				if deleted > 0 {
					if _, err := tx.ExecContext(ctx, `DELETE FROM ts_buckets WHERE driver_id=? AND metric_id=? AND resolution_ms=? AND start_ms>=? AND start_ms<?`, d, m, HistoryResolutionMS, b.StartMS, b.StartMS+b.ResolutionMS); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err == nil {
			rows = rows[n:]
		} else if errors.Is(err, context.DeadlineExceeded) {
			limit = max(1, limit/2)
		} else if !historyWriteBusy(err) {
			return err
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) MaintainAggregateHistory(ctx context.Context, cold string, now time.Time) error {
	if cold == "" {
		return nil
	}
	if !s.HistoryMigrationStatus().HistoryComplete {
		return nil
	}
	if err := s.lockArchive(ctx); err != nil {
		return err
	}
	defer s.archiveMu.Unlock()
	cutoff := s.reserveAggregateCutoff(now.Add(-AggregateRecentRetention).UTC().Truncate(time.Minute).UnixMilli())
	for {
		var first sql.NullInt64
		if err := s.history.QueryRowContext(ctx, `SELECT MIN(start_ms) FROM ts_buckets WHERE resolution_ms=? AND start_ms<?`, ArchiveResolutionMS, cutoff).Scan(&first); err != nil {
			return err
		}
		if !first.Valid {
			break
		}
		if err := s.archiveBucketDay(ctx, cold, time.UnixMilli(first.Int64).UTC().Truncate(24*time.Hour), cutoff); err != nil {
			return err
		}
	}
	paths, err := aggregatePaths(cold, 0, now.UnixMilli())
	if err != nil {
		return err
	}
	for _, path := range paths {
		day, err := aggregateDay(path)
		if err != nil {
			return err
		}
		if day.Add(24 * time.Hour).After(now.Add(-AggregateMinuteRetention)) {
			continue
		}
		if err := s.compactBucketFile(ctx, path, OldArchiveResolutionMS); err != nil {
			return err
		}
		// Energy totals live in the independent ledger. Hourly gauge summaries
		// remain available after the fixed two-year detailed history expires.
		if !day.Add(24 * time.Hour).After(now.Add(-AggregateRetention)) {
			if err := lockContext(ctx, s.archiveViewMu.TryLock); err != nil {
				return err
			}
			err = os.Remove(path)
			if err == nil {
				err = syncDir(filepath.Dir(path))
			}
			s.archiveViewMu.Unlock()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) compactBucketFile(ctx context.Context, path string, width int64) error {
	current, err := bucketFileResolution(path)
	if err != nil {
		return err
	}
	if current == width {
		return nil
	}
	if current > width {
		return errors.New("cannot refine compacted archive")
	}
	stage, tmp, err := newBucketStage(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer removeBucketStage(stage, tmp)
	rows := make([]metricBucket, 0, 256)
	changed := false
	evidence := bucketEvidenceSet{}
	flush := func() error {
		if len(rows) == 0 {
			return nil
		}
		err := stageBuckets(ctx, stage, rows, true)
		rows = rows[:0]
		if err != nil {
			return err
		}
		return pauseMaintenance(ctx)
	}
	err = walkBucketFile(ctx, path, func(b metricBucket) error {
		if b.ResolutionMS > width {
			return errors.New("cannot reconstruct finer history from an aggregate")
		}
		if err := evidence.add(b); err != nil {
			return err
		}
		changed = changed || b.ResolutionMS != width
		b.StartMS = bucketStart(b.StartMS, width)
		b.ResolutionMS = width
		rows = append(rows, b)
		if len(rows) == cap(rows) {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := flush(); err != nil {
		return err
	}
	if err := evidence.verifyStage(ctx, stage); err != nil {
		return err
	}
	if err := publishBucketStage(ctx, stage, path); err != nil {
		return err
	}
	digest, err := historyFileHashContext(ctx, path)
	if err != nil {
		return err
	}
	day, err := aggregateDay(path)
	if err != nil {
		return err
	}
	return s.writeArchiveBatch(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if strings.Contains(path, ".legacy-") {
			_, err := tx.ExecContext(ctx, `UPDATE ts_legacy_bucket_days SET sha256=? WHERE day_ms=?`, digest, day.UnixMilli())
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE ts_bucket_days SET sha256=?,resolution_ms=? WHERE day_ms=?`, digest, width, day.UnixMilli())
		return err
	})
}

func bucketFileResolution(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	pf, err := parquet.OpenFile(f, info.Size())
	if err != nil {
		return 0, err
	}
	value, ok := pf.Lookup("ftw_resolution_ms")
	if !ok {
		return 0, errors.New("aggregate resolution metadata is missing")
	}
	return strconv.ParseInt(value, 10, 64)
}
