package state

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/parquet-go/parquet-go"
)

func writeBucketJob(ctx context.Context, db *sql.DB, job archiveJob, batch []metricBucket) error {
	return commitArchiveJob(ctx, db, job, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO buckets VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, b := range batch {
			if err := b.validate(); err != nil {
				return err
			}
			if _, err := stmt.ExecContext(ctx, b.Driver, b.Metric, b.StartMS, b.ResolutionMS, b.FirstMS, b.LastMS, b.N, b.Sum, b.Min, b.Max, b.Last); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) archiveBucketDay(ctx context.Context, cold string, day time.Time, cutoff int64) error {
	dir := filepath.Join(cold, day.Format("2006/01"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, day.Format("02.buckets.parquet"))
	stagePath := filepath.Join(dir, day.Format(".ftw-archive-02.buckets.db"))
	stage, job, err := openArchiveJob(ctx, stagePath, bucketStageSchema)
	if err != nil {
		return err
	}
	defer stage.Close()
	job, err = checkArchiveSource(ctx, stage, path, "buckets", job)
	if err != nil {
		return err
	}
	yield := s.maintenanceYield(ctx)
	if job.ThroughMS == 0 {
		job.ThroughMS = min(day.Add(24*time.Hour).UnixMilli(), cutoff)
		var first sql.NullInt64
		if err := s.history.QueryRowContext(ctx, `SELECT MIN(start_ms) FROM ts_buckets WHERE resolution_ms=? AND start_ms>=? AND start_ms<?`, ArchiveResolutionMS, day.UnixMilli(), job.ThroughMS).Scan(&first); err != nil {
			return err
		}
		if !first.Valid {
			return removeArchiveJob(stage, stagePath)
		}
		job.FromMS = first.Int64
		if err := commitArchiveJob(ctx, stage, job, nil); err != nil {
			return err
		}
	}
	if job.Phase == "existing" {
		err := walkBucketFileFrom(ctx, path, job.ExistingRows, func(batch []metricBucket, total int64) error {
			for _, b := range batch {
				if b.ResolutionMS != ArchiveResolutionMS {
					return errors.New("minute archive was compacted before SQLite pruning")
				}
			}
			job.ExistingRows += int64(len(batch))
			if err := writeBucketJob(ctx, stage, job, batch); err != nil {
				return err
			}
			s.archiveProgress(day.Format("2006-01-02"), "read_archive", job.ExistingRows, total)
			return archiveCheckpoint(ctx)
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		job.Phase = "copy"
		if err := commitArchiveJob(ctx, stage, job, nil); err != nil {
			return err
		}
	}
	for job.Phase == "copy" {
		q := `SELECT d.name,m.name,b.start_ms,b.resolution_ms,b.first_ms,b.last_ms,b.n,b.sum_value,b.min_value,b.max_value,b.last_value,b.driver_id,b.metric_id FROM ts_buckets b JOIN ts_drivers d ON d.id=b.driver_id JOIN ts_metrics m ON m.id=b.metric_id WHERE b.resolution_ms=? AND b.start_ms>=? AND b.start_ms<?`
		args := []any{ArchiveResolutionMS, max(job.FromMS, job.Ts), job.ThroughMS}
		if job.CopiedRows > 0 {
			q += ` AND (b.start_ms,b.driver_id,b.metric_id)>(?,?,?)`
			args = append(args, job.Ts, job.DriverID, job.MetricID)
		}
		q += ` ORDER BY b.start_ms,b.driver_id,b.metric_id LIMIT 1024`
		rows, err := s.history.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		var batch []metricBucket
		var d, m int64
		for rows.Next() {
			var b metricBucket
			if err = rows.Scan(&b.Driver, &b.Metric, &b.StartMS, &b.ResolutionMS, &b.FirstMS, &b.LastMS, &b.N, &b.Sum, &b.Min, &b.Max, &b.Last, &d, &m); err != nil {
				break
			}
			batch = append(batch, b)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			job.Phase = "publish"
			if err := commitArchiveJob(ctx, stage, job, nil); err != nil {
				return err
			}
			break
		}
		job.Ts, job.DriverID, job.MetricID = batch[len(batch)-1].StartMS, d, m
		job.CopiedRows += int64(len(batch))
		if err := writeBucketJob(ctx, stage, job, batch); err != nil {
			return err
		}
		s.archiveProgress(day.Format("2006-01-02"), "copy_buckets", job.CopiedRows, 0)
		if err := yield(); err != nil {
			return err
		}
	}
	if job.Phase == "publish" {
		s.archiveProgress(day.Format("2006-01-02"), "verify_buckets", job.CopiedRows, job.CopiedRows)
		if err := publishBucketStage(ctx, stage, path); err != nil {
			return err
		}
		job.ArchiveSHA, err = historyFileHashContext(ctx, path)
		if err != nil {
			return err
		}
		if err := s.writeArchiveBatch(ctx, func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO ts_bucket_days(day_ms,sha256,resolution_ms,through_ms) VALUES(?,?,?,?) ON CONFLICT(day_ms) DO UPDATE SET sha256=excluded.sha256,resolution_ms=excluded.resolution_ms,through_ms=MAX(through_ms,excluded.through_ms)`, day.UnixMilli(), job.ArchiveSHA, ArchiveResolutionMS, job.ThroughMS)
			return err
		}); err != nil {
			return err
		}
		job.Phase = "prune"
		job.Driver, job.Metric = "", ""
		job.Ts = 0
		if err := commitArchiveJob(ctx, stage, job, nil); err != nil {
			return err
		}
	}
	// A missing or replaced published file never authorizes resumed deletion.
	digest, err := historyFileHashContext(ctx, path)
	if err != nil {
		return err
	}
	if digest != job.ArchiveSHA {
		return errors.New("published bucket archive changed before pruning")
	}
	for {
		rows, err := stage.QueryContext(ctx, `SELECT driver,metric,`+bucketColumns+` FROM buckets WHERE (driver,metric,start_ms)>(?,?,?) AND start_ms>=? AND start_ms<? ORDER BY driver,metric,start_ms,resolution_ms LIMIT 256`, job.Driver, job.Metric, job.Ts, job.FromMS, job.ThroughMS)
		if err != nil {
			return err
		}
		var batch []metricBucket
		for rows.Next() {
			var b metricBucket
			if err = rows.Scan(&b.Driver, &b.Metric, &b.StartMS, &b.ResolutionMS, &b.FirstMS, &b.LastMS, &b.N, &b.Sum, &b.Min, &b.Max, &b.Last); err != nil {
				break
			}
			batch = append(batch, b)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		if err := s.pruneBucketMinutes(ctx, batch); err != nil {
			return err
		}
		last := batch[len(batch)-1]
		job.Driver, job.Metric, job.Ts = last.Driver, last.Metric, last.StartMS
		job.PrunedRows += int64(len(batch))
		if err := commitArchiveJob(ctx, stage, job, nil); err != nil {
			return err
		}
		s.archiveProgress(day.Format("2006-01-02"), "prune_buckets", job.PrunedRows, job.CopiedRows)
		if err := archiveCheckpoint(ctx); err != nil {
			return err
		}
	}
	return removeArchiveJob(stage, stagePath)
}

func walkBucketFileFrom(ctx context.Context, path string, skip int64, visit func([]metricBucket, int64) error) error {
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
	if skip > pf.NumRows() {
		return errors.New("archive cursor exceeds source rows")
	}
	if err := r.SeekToRow(skip); err != nil {
		return err
	}
	buf := make([]metricBucket, 1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if e := visit(buf[:n], pf.NumRows()); e != nil {
				return e
			}
		}
		if errors.Is(err, io.EOF) {
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
