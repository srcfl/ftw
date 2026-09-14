package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"sort"
	"time"
)

// parquetSampleRow mirrors the long-format schema in column-oriented form.
// Driver and Metric are interned strings (parquet's dictionary encoding makes
// the repetition cheap — typically <2 bytes per row after compression).
type parquetSampleRow struct {
	TsMs   int64   `parquet:"ts_ms"`
	Driver string  `parquet:"driver,dict,zstd"`
	Metric string  `parquet:"metric,dict,zstd"`
	Value  float64 `parquet:"value,zstd"`
}

// RolloffToParquet exports samples older than RecentRetention into one
// parquet file per UTC-day, then deletes the rolled-off rows from SQLite.
// Idempotent: re-running for a day that already has a file merges into it
// (we accumulate strict-cutoff data, never lose anything).
//
// File layout: <coldDir>/YYYY/MM/DD.parquet
func (s *Store) RolloffToParquet(ctx context.Context, coldDir string) (rolledRows int64, files []string, err error) {
	if coldDir == "" {
		return 0, nil, fmt.Errorf("RolloffToParquet: coldDir must be set")
	}
	if !s.HistoryMigrationStatus().HistoryComplete {
		return 0, nil, nil
	}
	if err := s.ensureSeriesHours(ctx); err != nil {
		return 0, nil, err
	}
	if err := s.lockArchive(ctx); err != nil {
		return 0, nil, err
	}
	defer s.archiveMu.Unlock()
	if err := cleanupArchiveTemps(ctx, coldDir, time.Now()); err != nil {
		return 0, nil, err
	}
	cutoff := time.Now().Add(-RecentRetention).UTC()
	// Only complete UTC days roll off, so a retry merges the same day boundary.
	cutoff = time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	for {
		if err := ctx.Err(); err != nil {
			return rolledRows, files, err
		}
		var first sql.NullInt64
		if err := s.history.QueryRowContext(ctx, `SELECT MIN(ts_ms) FROM ts_samples WHERE ts_ms<?`, cutoff.UnixMilli()).Scan(&first); err != nil {
			return rolledRows, files, err
		}
		if !first.Valid {
			return rolledRows, files, nil
		}
		day := time.UnixMilli(first.Int64).UTC()
		from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
		n, path, err := s.archiveSampleDay(ctx, coldDir, from, from+24*time.Hour.Milliseconds())
		rolledRows += n
		if path != "" {
			files = append(files, path)
		}
		if err != nil {
			return rolledRows, files, err
		}
		if n == 0 {
			return rolledRows, files, fmt.Errorf("archive made no progress; source rows retained")
		}
	}
}

// syncDir fsyncs a directory so a completed rename survives power loss.
// Best-effort on platforms where directories can't be fsynced (Windows).
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

// LoadSeriesFromParquet reads one (driver, metric) series from cold storage.
// Scans every parquet file whose day overlaps [sinceMs, untilMs]. Filtered in
// process — daily files are small enough that pushdown isn't worth the
// complexity for this dataset size.
func (s *Store) LoadSeriesFromParquet(coldDir, driver, metric string, sinceMs, untilMs int64) ([]Sample, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	paths, err := parquetPaths(coldDir, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	out := make([]Sample, 0)
	for _, path := range paths {
		if err := walkParquetRows(ctx, path, func(rows []parquetSampleRow) error {
			for _, r := range rows {
				if r.Driver == driver && r.Metric == metric && r.TsMs >= sinceMs && r.TsMs <= untilMs {
					if len(out) >= maxRawSeriesPoints {
						return ErrHistoryQueryLimit
					}
					out = append(out, Sample{Driver: r.Driver, Metric: r.Metric, TsMs: r.TsMs, Value: r.Value})
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TsMs < out[j].TsMs })
	return out, nil
}
