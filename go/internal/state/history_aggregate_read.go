package state

import (
	"context"
	"errors"
	"os"
	"strings"
)

func rawBucket(ts int64, v float64) BucketSummary {
	return BucketSummary{StartMS: ts, ResolutionMS: 1, FirstMS: ts, LastMS: ts, N: 1, Sum: v, Min: v, Max: v, Last: v}
}

// Caller holds archiveViewMu. Ten-second SQLite rows win over a published
// minute while that minute awaits its atomic source prune. Legacy archives
// contain separate, pre-policy contributions and are added independently.
func (s *Store) walkAggregateSeries(ctx context.Context, cold, driver, metric string, since, until int64, visit func(BucketSummary) error) error {
	rows, err := s.history.QueryContext(ctx, `SELECT b.start_ms,b.resolution_ms,b.first_ms,b.last_ms,b.n,b.sum_value,b.min_value,b.max_value,b.last_value FROM ts_buckets b JOIN ts_drivers d ON d.id=b.driver_id JOIN ts_metrics m ON m.id=b.metric_id WHERE d.name=? AND m.name=? AND b.resolution_ms=? AND b.start_ms>=? AND b.start_ms<? ORDER BY b.start_ms`, driver, metric, HistoryResolutionMS, bucketStart(since, ArchiveResolutionMS), bucketStart(until, ArchiveResolutionMS)+ArchiveResolutionMS)
	if err != nil {
		return err
	}
	hot := make([]BucketSummary, 0)
	minutes := map[int64]bool{}
	for rows.Next() {
		var b BucketSummary
		if err = rows.Scan(&b.StartMS, &b.ResolutionMS, &b.FirstMS, &b.LastMS, &b.N, &b.Sum, &b.Min, &b.Max, &b.Last); err != nil {
			break
		}
		if len(hot) >= maxRawSeriesPoints {
			err = ErrHistoryQueryLimit
			break
		}
		hot = append(hot, b)
		minutes[bucketStart(b.StartMS, ArchiveResolutionMS)] = true
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	hours := map[int64]bool{}
	for minute := range minutes {
		hours[bucketStart(minute, HistoryHourResolutionMS)] = true
	}
	coarse, err := s.storedBuckets(ctx, driver, metric, ArchiveResolutionMS, since, until)
	if err != nil {
		return err
	}
	for _, b := range coarse {
		if minutes[b.StartMS] {
			continue
		}
		hot = append(hot, b)
		hours[bucketStart(b.StartMS, HistoryHourResolutionMS)] = true
	}
	hourly, err := s.storedBuckets(ctx, driver, metric, HistoryHourResolutionMS, since, until)
	if err != nil {
		return err
	}
	for _, b := range hourly {
		if hours[b.StartMS] {
			continue
		}
		hot = append(hot, b)
	}
	paths, err := aggregatePaths(cold, since, until)
	if err != nil {
		return err
	}
	for _, path := range paths {
		legacy := strings.Contains(path, ".legacy-buckets.")
		if legacy {
			original := strings.Replace(path, ".legacy-buckets.parquet", ".parquet", 1)
			if _, err := os.Stat(original); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := walkBucketFile(ctx, path, func(b metricBucket) error {
			if b.Driver != driver || b.Metric != metric || b.LastMS < since || b.LastMS > until {
				return nil
			}
			if hours[bucketStart(b.StartMS, HistoryHourResolutionMS)] || minutes[b.StartMS] {
				return nil
			}
			return visit(b.BucketSummary)
		}); err != nil {
			return err
		}
	}
	for _, b := range hot {
		if b.LastMS >= since && b.LastMS <= until {
			if err := visit(b); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) storedBuckets(ctx context.Context, driver, metric string, resolution, since, until int64) ([]BucketSummary, error) {
	rows, err := s.history.QueryContext(ctx, `SELECT b.start_ms,b.resolution_ms,b.first_ms,b.last_ms,b.n,b.sum_value,b.min_value,b.max_value,b.last_value FROM ts_buckets b JOIN ts_drivers d ON d.id=b.driver_id JOIN ts_metrics m ON m.id=b.metric_id WHERE d.name=? AND m.name=? AND b.resolution_ms=? AND b.start_ms>=? AND b.start_ms<? ORDER BY b.start_ms`, driver, metric, resolution, bucketStart(since, resolution), bucketStart(until, resolution)+resolution)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BucketSummary, 0)
	for rows.Next() {
		var b BucketSummary
		if err := rows.Scan(&b.StartMS, &b.ResolutionMS, &b.FirstMS, &b.LastMS, &b.N, &b.Sum, &b.Min, &b.Max, &b.Last); err != nil {
			return nil, err
		}
		if len(out) >= maxRawSeriesPoints {
			return nil, ErrHistoryQueryLimit
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) hasAggregateSeries(ctx context.Context, driver, metric string, since, until int64) bool {
	paths, err := aggregatePaths(s.coldDir, since, until)
	if err != nil || len(paths) > 0 {
		return true
	}
	var n int
	err = s.history.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ts_buckets b JOIN ts_drivers d ON d.id=b.driver_id JOIN ts_metrics m ON m.id=b.metric_id WHERE d.name=? AND m.name=? AND b.start_ms>=? AND b.start_ms<=?)`, driver, metric, bucketStart(since, ArchiveResolutionMS), until).Scan(&n)
	return err == nil && n > 0
}
