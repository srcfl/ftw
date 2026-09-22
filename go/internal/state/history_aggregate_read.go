package state

import (
	"context"
	"database/sql"
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

// Downsample chart rows in SQLite, so Go receives only the requested buckets.
// Pin finer-bucket probes to the series primary key: the age index reads
// every device in that interval and makes a chart scale with site size.
// Use the same source precedence as the export path, including fine rows just
// outside a partial query edge. Keep the last observed value, not its mean.
func (s *Store) walkSQLiteSeriesBuckets(ctx context.Context, driver, metric string, since, until, width int64, visit func(BucketSummary) error) error {
	var driverID, metricID int64
	if err := s.history.QueryRowContext(ctx, `SELECT d.id,m.id FROM ts_drivers d,ts_metrics m WHERE d.name=? AND m.name=?`, driver, metric).Scan(&driverID, &metricID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	rows, err := s.history.QueryContext(ctx, `WITH
 ten AS NOT MATERIALIZED (
 SELECT b.* FROM ts_buckets b WHERE b.driver_id=? AND b.metric_id=?
 AND b.resolution_ms=10000 AND b.start_ms>=? AND b.start_ms<?),
 minute AS NOT MATERIALIZED (
 SELECT b.* FROM ts_buckets b WHERE b.driver_id=? AND b.metric_id=?
 AND b.resolution_ms=60000 AND b.start_ms>=? AND b.start_ms<?),
 chosen AS (
 SELECT * FROM ten
 UNION ALL
 SELECT * FROM minute b WHERE NOT EXISTS (SELECT 1 FROM ts_buckets f INDEXED BY sqlite_autoindex_ts_buckets_1 WHERE f.driver_id=b.driver_id AND f.metric_id=b.metric_id AND f.resolution_ms=10000 AND f.start_ms>=b.start_ms AND f.start_ms<b.start_ms+60000)
 UNION ALL
 SELECT b.* FROM ts_buckets b WHERE b.driver_id=? AND b.metric_id=?
 AND b.resolution_ms=3600000 AND b.start_ms>=? AND b.start_ms<?
 AND NOT EXISTS (SELECT 1 FROM ts_buckets f INDEXED BY sqlite_autoindex_ts_buckets_1 WHERE f.driver_id=b.driver_id AND f.metric_id=b.metric_id AND f.resolution_ms=10000 AND f.start_ms>=MAX(b.start_ms,?) AND f.start_ms<MIN(b.start_ms+3600000,?))
 AND NOT EXISTS (SELECT 1 FROM ts_buckets f INDEXED BY sqlite_autoindex_ts_buckets_1 WHERE f.driver_id=b.driver_id AND f.metric_id=b.metric_id AND f.resolution_ms=60000 AND f.start_ms>=MAX(b.start_ms,?) AND f.start_ms<MIN(b.start_ms+3600000,?))),
 grouped AS (SELECT MIN(start_ms) AS start_ms,MAX(resolution_ms) AS resolution_ms,MIN(first_ms) AS first_ms,
 MAX(last_ms) AS last_ms,SUM(n) AS n,SUM(sum_value) AS sum_value,MIN(min_value) AS min_value,MAX(max_value) AS max_value
 FROM chosen WHERE last_ms>=? AND last_ms<=? GROUP BY (last_ms-?)/?)
 SELECT g.*, (SELECT last_value FROM ts_buckets b WHERE b.driver_id=? AND b.metric_id=?
 AND b.start_ms IN (g.last_ms-(g.last_ms%10000+10000)%10000,g.last_ms-(g.last_ms%60000+60000)%60000,g.last_ms-(g.last_ms%3600000+3600000)%3600000)
 AND b.last_ms=g.last_ms ORDER BY b.resolution_ms LIMIT 1) FROM grouped g`,
		driverID, metricID, bucketStart(since, ArchiveResolutionMS), bucketStart(until, ArchiveResolutionMS)+ArchiveResolutionMS,
		driverID, metricID, bucketStart(since, ArchiveResolutionMS), bucketStart(until, ArchiveResolutionMS)+ArchiveResolutionMS,
		driverID, metricID, bucketStart(since, HistoryHourResolutionMS), bucketStart(until, HistoryHourResolutionMS)+HistoryHourResolutionMS,
		bucketStart(since, ArchiveResolutionMS), bucketStart(until, ArchiveResolutionMS)+ArchiveResolutionMS,
		bucketStart(since, ArchiveResolutionMS), bucketStart(until, ArchiveResolutionMS)+ArchiveResolutionMS,
		since, until, since, width, driverID, metricID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b BucketSummary
		if err := rows.Scan(&b.StartMS, &b.ResolutionMS, &b.FirstMS, &b.LastMS, &b.N, &b.Sum, &b.Min, &b.Max, &b.Last); err != nil {
			return err
		}
		if err := visit(b); err != nil {
			return err
		}
	}
	return rows.Err()
}
