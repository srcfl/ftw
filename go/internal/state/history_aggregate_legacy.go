package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"
)

var ErrCompactedHistory = errors.New("individual samples cannot correct compacted history; restore the original history before importing corrections")

// The bounded staging database stores its source cursor with each aggregate
// update. A restart resumes the same file instead of repeating a large copy.
// The immutable raw archive remains until independent evidence checks and
// readback of the replacement have passed.
func (s *Store) CompactLegacyHistory(ctx context.Context, cold string, now time.Time) error {
	if cold == "" || !s.HistoryMigrationStatus().HistoryComplete {
		return nil
	}
	if err := s.lockArchive(ctx); err != nil {
		return err
	}
	defer s.archiveMu.Unlock()
	paths, err := parquetPaths(cold, 0, now.Add(-AggregateRecentRetention).UTC().Truncate(24*time.Hour).UnixMilli()-1)
	if err != nil {
		return err
	}
	for _, path := range paths {
		for s.HistoryWriterStatus().Pending >= historyCommitMaxTicks/2 {
			if err := pauseMaintenance(ctx); err != nil {
				return err
			}
		}
		if err := s.compactLegacyFile(ctx, path, now); err != nil {
			return err
		}
		if err := archiveCheckpoint(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) compactLegacyFile(ctx context.Context, path string, now time.Time) error {
	day, err := time.Parse("2006/01/02.parquet", filepath.ToSlash(filepath.Join(filepath.Base(filepath.Dir(filepath.Dir(path))), filepath.Base(filepath.Dir(path)), filepath.Base(path))))
	if err != nil {
		return err
	}
	var hot int
	if err := s.history.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ts_samples WHERE ts_ms>=? AND ts_ms<?)`, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli()).Scan(&hot); err != nil {
		return err
	}
	if hot != 0 {
		return nil
	}
	if err := s.summarizeParquetDay(ctx, path); err != nil {
		return err
	}
	digest, err := historyFileHashContext(ctx, path)
	if err != nil {
		return err
	}
	width := ArchiveResolutionMS
	if !day.Add(24 * time.Hour).After(now.Add(-AggregateMinuteRetention)) {
		width = OldArchiveResolutionMS
	}
	stagePath := filepath.Join(filepath.Dir(path), ".ftw-buckets-"+day.Format("02")+".pending.db")
	stage, err := openBackupDestination(stagePath)
	if err != nil {
		return err
	}
	defer func() {
		if stage != nil {
			stage.Close()
		}
	}()
	for _, q := range []string{bucketStageSchema, `CREATE TABLE IF NOT EXISTS bucket_progress(id INTEGER PRIMARY KEY,source_sha256 TEXT NOT NULL,resolution_ms INTEGER NOT NULL,rows_done INTEGER NOT NULL)`} {
		if _, err := stage.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	var previous string
	var priorWidth, cursor int64
	err = stage.QueryRowContext(ctx, `SELECT source_sha256,resolution_ms,rows_done FROM bucket_progress WHERE id=1`).Scan(&previous, &priorWidth, &cursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if previous != "" && (previous != digest || priorWidth != width) {
		// Only derived scratch data is reset. The raw source is still intact.
		if _, err := stage.ExecContext(ctx, `DELETE FROM buckets;DELETE FROM bucket_progress`); err != nil {
			return err
		}
		cursor = 0
		previous = ""
	}
	if previous == "" {
		if _, err := stage.ExecContext(ctx, `INSERT INTO bucket_progress VALUES(1,?,?,0)`, digest, width); err != nil {
			return err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	pf, err := parquet.OpenFile(f, info.Size())
	if err != nil {
		return err
	}
	if pf.NumRows() == 0 {
		return nil
	}
	if cursor > pf.NumRows() {
		return errors.New("legacy aggregate cursor exceeds source rows")
	}
	r := parquet.NewGenericReader[parquetSampleRow](pf)
	defer func() {
		if r != nil {
			r.Close()
		}
	}()
	if err := r.SeekToRow(cursor); err != nil {
		return err
	}
	raw := make([]parquetSampleRow, 8192)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := r.Read(raw)
		groups := map[[3]string]metricBucket{}
		for _, v := range raw[:n] {
			start := bucketStart(v.TsMs, width)
			key := [3]string{v.Driver, v.Metric, fmt.Sprint(start)}
			b := groups[key]
			b.Driver, b.Metric = v.Driver, v.Metric
			b.StartMS, b.ResolutionMS = start, width
			b.merge(rawBucket(v.TsMs, v.Value))
			groups[key] = b
		}
		if n > 0 {
			batch := make([]metricBucket, 0, len(groups))
			for _, b := range groups {
				batch = append(batch, b)
			}
			if err := stageBucketsProgress(ctx, stage, batch, true, cursor+int64(n)); err != nil {
				return err
			}
			cursor += int64(n)
			s.archiveProgress(day.Format("2006-01-02"), "aggregate_legacy", cursor, pf.NumRows())
			if err := archiveCheckpoint(ctx); err != nil {
				return err
			}
			if err := pauseMaintenance(ctx); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	if cursor != pf.NumRows() {
		return errors.New("legacy source was not read completely")
	}
	// Windows will not remove an archive while this reader owns its handle.
	// Verification below opens its own bounded reads before source retirement.
	closeErr := errors.Join(r.Close(), f.Close())
	r = nil
	f = nil
	if closeErr != nil {
		return closeErr
	}

	evidence := bucketEvidenceSet{}
	if err := walkParquetRows(ctx, path, func(rows []parquetSampleRow) error {
		for _, v := range rows {
			if err := evidence.add(metricBucket{Driver: v.Driver, Metric: v.Metric, BucketSummary: rawBucket(v.TsMs, v.Value)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := evidence.verifyStage(ctx, stage); err != nil {
		return err
	}
	target := strings.TrimSuffix(path, ".parquet") + ".legacy-buckets.parquet"
	if err := publishBucketStage(ctx, stage, target); err != nil {
		return err
	}
	current, err := historyFileHashContext(ctx, path)
	if err != nil {
		return err
	}
	if current != digest {
		return errors.New("legacy source changed during compaction; source retained")
	}
	targetHash, err := historyFileHashContext(ctx, target)
	if err != nil {
		return err
	}
	// This lock serializes the transition from raw to aggregate for readers
	// and rejects individual late corrections before removing their source.
	if err := s.archiveTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ts_samples WHERE ts_ms>=? AND ts_ms<?)`, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli()).Scan(&pending); err != nil {
			return err
		}
		if pending != 0 {
			return errors.New("late raw samples arrived during compaction; source retained")
		}
		return nil
	}, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO ts_legacy_bucket_days VALUES(?,?,?) ON CONFLICT(day_ms) DO UPDATE SET source_sha256=excluded.source_sha256,sha256=excluded.sha256`, day.UnixMilli(), digest, targetHash)
		return err
	}); err != nil {
		return fmt.Errorf("seal legacy day %s: %w", day.Format("2006-01-02"), err)
	}
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
	if err := stage.Close(); err != nil {
		return err
	}
	stage = nil
	os.Remove(stagePath)
	os.Remove(stagePath + "-journal")
	return nil
}

func rejectCompactedSamples(ctx context.Context, tx *sql.Tx, rs []resolvedSample) error {
	seen := map[int64]bool{}
	for _, r := range rs {
		day := bucketStart(r.ts, 24*time.Hour.Milliseconds())
		if seen[day] {
			continue
		}
		seen[day] = true
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ts_legacy_bucket_days WHERE day_ms=?)`, day).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return ErrCompactedHistory
		}
	}
	return nil
}
