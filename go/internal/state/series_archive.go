package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// These bounds apply before allocation. Longer exports must page by time;
// graph callers can request buckets without materializing the raw series.
const maxRawSeriesPoints = 200000
const maxSeriesBuckets = 10000

var ErrHistoryQueryLimit = errors.New("history query exceeds the local limit; use buckets or a shorter time range")

func parquetPaths(coldDir string, since, until int64) ([]string, error) {
	if coldDir == "" || until < since {
		return nil, nil
	}
	paths, err := filepath.Glob(filepath.Join(coldDir, "[0-9][0-9][0-9][0-9]", "[0-9][0-9]", "[0-9][0-9].parquet"))
	if err != nil {
		return nil, err
	}
	out := paths[:0]
	for _, p := range paths {
		rel, err := filepath.Rel(coldDir, p)
		if err != nil {
			return nil, err
		}
		day, err := time.Parse("2006/01/02.parquet", filepath.ToSlash(rel))
		if err != nil {
			return nil, err
		}
		if day.UnixMilli() <= until && day.Add(24*time.Hour).UnixMilli() > since {
			out = append(out, p)
		}
	}
	return out, nil
}

// walkMergedSeries only holds one selected series/day in memory. Raw SQLite
// wins on matching timestamps during a retry between file publish and prune.
func (s *Store) walkMergedSeries(ctx context.Context, coldDir, driver, metric string, since, until int64, visit func(int64, float64) error) error {
	return s.walkMergedSeriesStats(ctx, coldDir, driver, metric, since, until, func(b BucketSummary) error { return visit(b.LastMS, b.Sum/float64(b.N)) })
}

func (s *Store) walkMergedSeriesStats(ctx context.Context, coldDir, driver, metric string, since, until int64, visit func(BucketSummary) error) error {
	return s.walkMergedSeriesStatsBinned(ctx, coldDir, driver, metric, since, until, 0, visit)
}

func (s *Store) walkMergedSeriesStatsBinned(ctx context.Context, coldDir, driver, metric string, since, until, width int64, visit func(BucketSummary) error) error {
	if err := lockContext(ctx, s.archiveViewMu.TryRLock); err != nil {
		return err
	}
	defer s.archiveViewMu.RUnlock()
	if until < since {
		return nil
	}
	paths, err := parquetPaths(coldDir, since, until)
	if err != nil {
		return err
	}
	covered := make(map[int64]bool, len(paths))
	for _, path := range paths {
		rel, _ := filepath.Rel(coldDir, path)
		day, err := time.Parse("2006/01/02.parquet", filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		from, to := max(since, day.UnixMilli()), min(until, day.Add(24*time.Hour).UnixMilli()-1)
		raw, err := s.rawSeriesMap(ctx, driver, metric, from, to)
		if err != nil {
			return err
		}
		err = walkParquetRows(ctx, path, func(batch []parquetSampleRow) error {
			for _, r := range batch {
				if r.Driver != driver || r.Metric != metric || r.TsMs < from || r.TsMs > to {
					continue
				}
				if _, ok := raw[r.TsMs]; ok {
					continue
				}
				if err := visit(rawBucket(r.TsMs, r.Value)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		for ts, v := range raw {
			if err := visit(rawBucket(ts, v)); err != nil {
				return err
			}
		}
		covered[day.UnixMilli()] = true
	}
	rows, err := s.history.QueryContext(ctx, `SELECT s.ts_ms,s.value FROM ts_samples s JOIN ts_drivers d ON d.id=s.driver_id JOIN ts_metrics m ON m.id=s.metric_id WHERE d.name=? AND m.name=? AND s.ts_ms BETWEEN ? AND ? ORDER BY s.ts_ms`, driver, metric, since, until)
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
		if covered[time.UnixMilli(ts).UTC().Truncate(24*time.Hour).UnixMilli()] {
			continue
		}
		if err := visit(rawBucket(ts, v)); err != nil {
			rows.Close()
			return err
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if width > 0 {
		paths, err := aggregatePaths(coldDir, since, until)
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			return s.walkSQLiteSeriesBuckets(ctx, driver, metric, since, until, width, visit)
		}
	}
	return s.walkAggregateSeries(ctx, coldDir, driver, metric, since, until, visit)
}

func (s *Store) rawSeriesMap(ctx context.Context, driver, metric string, from, to int64) (map[int64]float64, error) {
	rows, err := s.history.QueryContext(ctx, `SELECT s.ts_ms,s.value FROM ts_samples s JOIN ts_drivers d ON d.id=s.driver_id JOIN ts_metrics m ON m.id=s.metric_id WHERE d.name=? AND m.name=? AND s.ts_ms BETWEEN ? AND ? LIMIT ?`, driver, metric, from, to, maxRawSeriesPoints+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]float64)
	for rows.Next() {
		var ts int64
		var v float64
		if err := rows.Scan(&ts, &v); err != nil {
			return nil, err
		}
		if len(out) >= maxRawSeriesPoints {
			return nil, ErrHistoryQueryLimit
		}
		out[ts] = v
	}
	return out, rows.Err()
}

func (s *Store) mergedSeries(ctx context.Context, coldDir, driver, metric string, since, until int64, maxPoints int) ([]SeriesPoint, error) {
	if maxPoints > maxSeriesBuckets {
		return nil, ErrHistoryQueryLimit
	}
	if maxPoints <= 0 {
		out := make([]SeriesPoint, 0)
		err := s.walkMergedSeriesStats(ctx, coldDir, driver, metric, since, until, func(b BucketSummary) error {
			if len(out) >= maxRawSeriesPoints {
				return ErrHistoryQueryLimit
			}
			out = append(out, bucketSeriesPoint(b))
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(out, func(i, j int) bool { return out[i].TsMs < out[j].TsMs })
		return out, nil
	}
	width := BucketWidthMs(since, until, maxPoints)
	acc := make(map[int64]*seriesBucketAcc)
	err := s.walkMergedSeriesStatsBinned(ctx, coldDir, driver, metric, since, until, width, func(b BucketSummary) error {
		key := (b.LastMS - since) / width
		a := acc[key]
		if a == nil {
			a = &seriesBucketAcc{}
			acc[key] = a
		}
		a.addBucket(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	keys := make([]int64, 0, len(acc))
	for k := range acc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]SeriesPoint, 0, len(keys))
	for _, k := range keys {
		a := acc[k]
		out = append(out, a.point())
	}
	return out, nil
}

// Rolloff is mandatory before raw retention. A failed archive never falls
// through to a DELETE. Retention removes only verified files with summaries.
func (s *Store) retainSampleHistory(ctx context.Context, days int, now time.Time) error {
	if s.coldDir == "" {
		return nil
	}
	if _, _, err := s.RolloffToParquet(ctx, s.coldDir); err != nil {
		return err
	}
	if days <= 0 {
		return nil
	}
	if err := s.ensureSeriesHours(ctx); err != nil {
		return err
	}
	paths, err := parquetPaths(s.coldDir, 0, now.UTC().AddDate(0, 0, -days).Truncate(24*time.Hour).UnixMilli()-1)
	if err != nil {
		return err
	}
	if err := s.lockArchive(ctx); err != nil {
		return err
	}
	defer s.archiveMu.Unlock()
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Build/verify the aggregate before applying the configured raw retention.
		if err := s.summarizeParquetDay(ctx, path); err != nil {
			return err
		}
		if err := s.removeArchive(ctx, path); err != nil {
			return err
		}
	}
	return nil
}

// A marker binds summaries to the exact file content. Changed or late files
// rebuild before they are allowed to expire. Only file hashes are persisted.
func (s *Store) summarizeParquetDay(ctx context.Context, path string) error {
	digest, err := historyFileHashContext(ctx, path)
	if err != nil {
		return err
	}
	var previous string
	err = s.history.QueryRowContext(ctx, `SELECT sha256 FROM ts_archive_days WHERE path=?`, parquetSummaryKey(path)).Scan(&previous)
	if err == nil && previous == digest {
		return nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ftw-summary-*.db")
	if err != nil {
		return err
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)
	defer os.Remove(tmp + "-journal")
	stage, err := openArchiveStage(tmp)
	if err != nil {
		return err
	}
	defer stage.Close()
	if err := walkParquetRows(ctx, path, func(batch []parquetSampleRow) error { return insertArchiveRows(ctx, stage, batch) }); err != nil {
		return err
	}
	if err := s.archiveDayHours(ctx, stage); err != nil {
		return err
	}
	after, err := historyFileHashContext(ctx, path)
	if err != nil {
		return err
	}
	if after != digest {
		return fmt.Errorf("archive changed while summarizing %s", filepath.Base(path))
	}
	_, err = s.history.ExecContext(ctx, `INSERT INTO ts_archive_days(path,sha256) VALUES(?,?) ON CONFLICT(path) DO UPDATE SET sha256=excluded.sha256`, parquetSummaryKey(path), digest)
	return err
}

func (s *Store) ensureParquetHours(ctx context.Context) error {
	paths, err := filepath.Glob(filepath.Join(s.coldDir, "[0-9][0-9][0-9][0-9]", "[0-9][0-9]", "[0-9][0-9].parquet"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(s.coldDir) == "" {
		return nil
	}
	for _, path := range paths {
		if err := s.yieldHistoryImport(ctx); err != nil {
			return err
		}
		if err := s.lockArchive(ctx); err != nil {
			return err
		}
		err := s.summarizeParquetDay(ctx, path)
		s.archiveMu.Unlock()
		if err != nil {
			return err
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) onlySeriesSummary(ctx context.Context, driver, metric string, since, until int64) bool {
	if s.hasAggregateSeries(ctx, driver, metric, since, until) {
		return false
	}
	paths, err := parquetPaths(s.coldDir, since, until)
	if err != nil || len(paths) > 0 {
		return false
	}
	var n int
	err = s.history.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ts_samples s JOIN ts_drivers d ON d.id=s.driver_id JOIN ts_metrics m ON m.id=s.metric_id WHERE d.name=? AND m.name=? AND s.ts_ms BETWEEN ? AND ? LIMIT 1)`, driver, metric, since, until).Scan(&n)
	return err == nil && n == 0
}

func parquetSummaryKey(path string) string {
	return filepath.ToSlash(filepath.Join(filepath.Base(filepath.Dir(filepath.Dir(path))), filepath.Base(filepath.Dir(path)), filepath.Base(path)))
}
func (s *Store) markParquetSummary(ctx context.Context, path string) error {
	digest, err := historyFileHashContext(ctx, path)
	if err != nil {
		return err
	}
	_, err = s.history.ExecContext(ctx, `INSERT INTO ts_archive_days(path,sha256) VALUES(?,?) ON CONFLICT(path) DO UPDATE SET sha256=excluded.sha256`, parquetSummaryKey(path), digest)
	return err
}

func (s *Store) lockArchive(ctx context.Context) error {
	return lockContext(ctx, s.archiveMu.TryLock)
}

func lockContext(ctx context.Context, tryLock func() bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tryLock() {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) removeArchive(ctx context.Context, path string) error {
	if err := lockContext(ctx, s.archiveViewMu.TryLock); err != nil {
		return err
	}
	defer s.archiveViewMu.Unlock()
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (s *Store) replaceArchive(ctx context.Context, tmp, path string) error {
	if err := lockContext(ctx, s.archiveViewMu.TryLock); err != nil {
		return err
	}
	defer s.archiveViewMu.Unlock()
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
