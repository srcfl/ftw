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

const sampleStageSchema = `CREATE TABLE IF NOT EXISTS samples(ts_ms INTEGER NOT NULL,driver TEXT NOT NULL,metric TEXT NOT NULL,value REAL NOT NULL,driver_id INTEGER,metric_id INTEGER,PRIMARY KEY(ts_ms,driver,metric)) WITHOUT ROWID`

type archiveSample struct {
	parquetSampleRow
	d, m sql.NullInt64
}

func writeSampleJob(ctx context.Context, db *sql.DB, job archiveJob, batch []archiveSample) error {
	return commitArchiveJob(ctx, db, job, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO samples VALUES(?,?,?,?,?,?) ON CONFLICT(ts_ms,driver,metric) DO UPDATE SET value=excluded.value,driver_id=excluded.driver_id,metric_id=excluded.metric_id`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range batch {
			if _, err := stmt.ExecContext(ctx, r.TsMs, r.Driver, r.Metric, r.Value, r.d, r.m); err != nil {
				return err
			}
		}
		return nil
	})
}

// Resume copying and pruning at committed keys, including after Core restarts.
// Only the published, read-back-verified file authorizes a value-matched prune.
func (s *Store) archiveSampleDay(ctx context.Context, cold string, from, to int64) (int64, string, error) {
	day := time.UnixMilli(from).UTC()
	dir := filepath.Join(cold, day.Format("2006/01"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, "", err
	}
	path := filepath.Join(dir, day.Format("02.parquet"))
	stagePath := filepath.Join(dir, day.Format(".ftw-archive-02.samples.db"))
	stage, job, err := openArchiveJob(ctx, stagePath, sampleStageSchema)
	if err != nil {
		return 0, "", err
	}
	defer stage.Close()
	job, err = checkArchiveSource(ctx, stage, path, "samples", job)
	if err != nil {
		return 0, "", err
	}
	yield := s.maintenanceYield(ctx)
	if job.Phase == "existing" {
		if _, err := os.Stat(path); err == nil {
			if err := s.summarizeParquetDay(ctx, path); err != nil {
				return 0, "", err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, "", err
		}
		err := walkSampleFileFrom(ctx, path, job.ExistingRows, func(rows []parquetSampleRow, total int64) error {
			batch := make([]archiveSample, len(rows))
			for i, r := range rows {
				batch[i].parquetSampleRow = r
			}
			job.ExistingRows += int64(len(rows))
			if err := writeSampleJob(ctx, stage, job, batch); err != nil {
				return err
			}
			s.archiveProgress(day.Format("2006-01-02"), "read_archive", job.ExistingRows, total)
			return archiveCheckpoint(ctx)
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, "", err
		}
		job.Phase = "copy"
		if err := commitArchiveJob(ctx, stage, job, nil); err != nil {
			return 0, "", err
		}
	}
	for job.Phase == "copy" {
		// Put the cursor's timestamp in the indexed scalar bound too. The
		// tuple alone can leave SQLite scanning the day again for every page.
		q := `SELECT s.ts_ms,d.name,m.name,s.value,s.driver_id,s.metric_id FROM ts_samples s JOIN ts_drivers d ON d.id=s.driver_id JOIN ts_metrics m ON m.id=s.metric_id WHERE s.ts_ms>=? AND s.ts_ms<?`
		args := []any{max(from, job.Ts), to}
		if job.CopiedRows > 0 {
			q += ` AND (s.ts_ms,s.driver_id,s.metric_id)>(?,?,?)`
			args = append(args, job.Ts, job.DriverID, job.MetricID)
		}
		q += ` ORDER BY s.ts_ms,s.driver_id,s.metric_id LIMIT 1024`
		rows, err := s.history.QueryContext(ctx, q, args...)
		if err != nil {
			return 0, "", err
		}
		var batch []archiveSample
		for rows.Next() {
			var r archiveSample
			if err = rows.Scan(&r.TsMs, &r.Driver, &r.Metric, &r.Value, &r.d, &r.m); err != nil {
				break
			}
			batch = append(batch, r)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			return 0, "", err
		}
		if len(batch) == 0 {
			job.Phase = "publish"
			if err := commitArchiveJob(ctx, stage, job, nil); err != nil {
				return 0, "", err
			}
			break
		}
		last := batch[len(batch)-1]
		job.Ts, job.DriverID, job.MetricID = last.TsMs, last.d.Int64, last.m.Int64
		job.CopiedRows += int64(len(batch))
		if err := writeSampleJob(ctx, stage, job, batch); err != nil {
			return 0, "", err
		}
		s.archiveProgress(day.Format("2006-01-02"), "copy_samples", job.CopiedRows, 0)
		if err := yield(); err != nil {
			return 0, "", err
		}
	}
	if job.Phase == "publish" {
		s.archiveProgress(day.Format("2006-01-02"), "verify_samples", job.CopiedRows, job.CopiedRows)
		if err := s.publishStagedSamples(ctx, path, stage); err != nil {
			return 0, "", err
		}
		if err := s.markParquetSummary(ctx, path); err != nil {
			return 0, "", err
		}
		job.ArchiveSHA, err = historyFileHashContext(ctx, path)
		if err != nil {
			return 0, "", err
		}
		job.Phase = "prune"
		job.Ts = 0
		job.Driver, job.Metric = "", ""
		if err := commitArchiveJob(ctx, stage, job, nil); err != nil {
			return 0, "", err
		}
	}
	var deleted int64
	digest, err := historyFileHashContext(ctx, path)
	if err != nil {
		return 0, "", err
	}
	if digest != job.ArchiveSHA {
		return 0, "", errors.New("published sample archive changed before pruning")
	}
	for job.Phase == "prune" {
		// This is the staging PRIMARY KEY, not the numeric source IDs. Sorting
		// on source IDs instead rescanned and sorted the file for every page.
		rows, err := stage.QueryContext(ctx, `SELECT ts_ms,driver,metric,value,driver_id,metric_id FROM samples WHERE ts_ms>=? AND (ts_ms,driver,metric)>(?,?,?) AND driver_id IS NOT NULL ORDER BY ts_ms,driver,metric LIMIT 1024`, job.Ts, job.Ts, job.Driver, job.Metric)
		if err != nil {
			return deleted, "", err
		}
		var batch []resolvedSample
		var last archiveSample
		for rows.Next() {
			var r archiveSample
			if err = rows.Scan(&r.TsMs, &r.Driver, &r.Metric, &r.Value, &r.d, &r.m); err != nil {
				break
			}
			batch = append(batch, resolvedSample{ts: r.TsMs, dID: r.d.Int64, mID: r.m.Int64, v: r.Value})
			last = r
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			return deleted, "", err
		}
		if len(batch) == 0 {
			break
		}
		n, err := s.pruneArchivedSamples(ctx, batch)
		deleted += n
		if err != nil {
			return deleted, "", err
		}
		job.Ts, job.Driver, job.Metric = last.TsMs, last.Driver, last.Metric
		job.PrunedRows += int64(len(batch))
		if err := commitArchiveJob(ctx, stage, job, nil); err != nil {
			return deleted, "", err
		}
		s.archiveProgress(day.Format("2006-01-02"), "prune_samples", job.PrunedRows, job.CopiedRows)
		if err := yield(); err != nil {
			return deleted, "", err
		}
	}
	if err := removeArchiveJob(stage, stagePath); err != nil {
		return deleted, "", err
	}
	return deleted, path, nil
}

func walkSampleFileFrom(ctx context.Context, path string, skip int64, visit func([]parquetSampleRow, int64) error) error {
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
	r := parquet.NewGenericReader[parquetSampleRow](pf)
	defer r.Close()
	if skip > pf.NumRows() {
		return errors.New("archive cursor exceeds source rows")
	}
	if err := r.SeekToRow(skip); err != nil {
		return err
	}
	buf := make([]parquetSampleRow, 1024)
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
