package state

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
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
	cutoff := time.Now().Add(-RecentRetention).UnixMilli()

	// SamplesBefore streams in ts order, so UTC-day boundaries arrive in
	// order: accumulate one day at a time and flush on day change. Peak
	// memory is one day of samples (~30 MB) — a multi-day backlog must NOT
	// be buffered whole, because ~20M backlog rows ≈ 640 MB OOMs a Pi, and
	// a killed rolloff never trims SQLite, making the next attempt bigger.
	type dayKey struct{ year, month, day int }
	var (
		cur    dayKey
		curSet bool
		rows   []parquetSampleRow
	)
	flush := func() error {
		if len(rows) == 0 {
			return nil
		}
		path, err := flushParquetDay(coldDir, cur.year, cur.month, cur.day, rows)
		if err != nil {
			return err
		}
		// Delete this day's rows as soon as its file is durable, in
		// hour-sized transactions. A single end-of-run DELETE over a large
		// backlog holds the write lock for minutes and loses the race
		// against the control loop's writers (observed as SQLITE_BUSY in
		// production, leaving the rolloff to redo everything each hour).
		dayStart := time.Date(cur.year, time.Month(cur.month), cur.day, 0, 0, 0, 0, time.UTC).UnixMilli()
		dayEnd := dayStart + 24*60*60*1000
		if dayEnd > cutoff {
			dayEnd = cutoff
		}
		if err := s.deleteSamplesChunked(ctx, dayStart, dayEnd); err != nil {
			return fmt.Errorf("delete day %04d-%02d-%02d: %w", cur.year, cur.month, cur.day, err)
		}
		files = append(files, path)
		rolledRows += int64(len(rows))
		rows = rows[:0]
		return nil
	}
	err = s.SamplesBefore(ctx, cutoff, 50000, func(batch []Sample) error {
		for _, sm := range batch {
			t := time.UnixMilli(sm.TsMs).UTC()
			k := dayKey{t.Year(), int(t.Month()), t.Day()}
			if curSet && k != cur {
				if err := flush(); err != nil {
					return err
				}
			}
			cur, curSet = k, true
			rows = append(rows, parquetSampleRow{
				TsMs: sm.TsMs, Driver: sm.Driver, Metric: sm.Metric, Value: sm.Value,
			})
		}
		return nil
	})
	if err != nil {
		return rolledRows, files, fmt.Errorf("read samples: %w", err)
	}
	if err := flush(); err != nil {
		return rolledRows, files, err
	}
	if rolledRows == 0 {
		return 0, nil, nil
	}
	return rolledRows, files, nil
}

// deleteSamplesChunked removes ts_samples in [fromMs, toMs) one hour-window
// transaction at a time, so the write lock is released between chunks and
// live writers interleave instead of timing out.
func (s *Store) deleteSamplesChunked(ctx context.Context, fromMs, toMs int64) error {
	const windowMs = 60 * 60 * 1000
	for start := fromMs; start < toMs; start += windowMs {
		end := start + windowMs
		if end > toMs {
			end = toMs
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM ts_samples WHERE ts_ms >= ? AND ts_ms < ?`, start, end); err != nil {
			return err
		}
		// Writer-fairness gap — see pruneChunkPause for why bounded
		// transactions alone don't let waiting writers in.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(maintenancePause):
		}
	}
	return nil
}

// maintenancePause is the inter-transaction yield for chunked rolloff
// deletes. Var so tests can shrink it.
var maintenancePause = 100 * time.Millisecond

// flushParquetDay merges rows into the existing day file (if any) and writes
// the result durably. Returns the file path.
func flushParquetDay(coldDir string, year, month, day int, rows []parquetSampleRow) (string, error) {
	// Sort by ts to maximize compression and make consumer scans linear.
	sort.Slice(rows, func(i, j int) bool { return rows[i].TsMs < rows[j].TsMs })

	dayDir := filepath.Join(coldDir, fmt.Sprintf("%04d/%02d", year, month))
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dayDir, err)
	}
	path := filepath.Join(dayDir, fmt.Sprintf("%02d.parquet", day))
	if existing, err := readParquetDay(path); err == nil {
		rows = mergeParquetRows(existing, rows)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read existing %s: %w", path, err)
	}
	if err := writeParquetDay(path, rows); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

func writeParquetDay(path string, rows []parquetSampleRow) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := parquet.NewGenericWriter[parquetSampleRow](f, parquet.Compression(&zstd.Codec{Level: zstd.DefaultLevel}))
	if _, err := w.Write(rows); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := w.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	// fsync before rename: the caller deletes the SQLite rows as soon as this
	// returns, so a power cut must not be able to leave a truncated file
	// behind an already-visible rename (rename is only atomic for data that
	// has reached disk).
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
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

func readParquetDay(path string) ([]parquetSampleRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	pf, err := parquet.OpenFile(f, stat.Size())
	if err != nil {
		return nil, err
	}
	reader := parquet.NewGenericReader[parquetSampleRow](pf)
	defer reader.Close()

	rows := make([]parquetSampleRow, 0, 1024)
	buf := make([]parquetSampleRow, 1024)
	for {
		n, err := reader.Read(buf)
		rows = append(rows, buf[:n]...)
		if err == nil {
			continue
		}
		if err == io.EOF {
			return rows, nil
		}
		return rows, err
	}
}

func mergeParquetRows(existing, current []parquetSampleRow) []parquetSampleRow {
	type sampleKey struct {
		ts             int64
		driver, metric string
	}
	byKey := make(map[sampleKey]parquetSampleRow, len(existing)+len(current))
	for _, r := range existing {
		byKey[sampleKey{ts: r.TsMs, driver: r.Driver, metric: r.Metric}] = r
	}
	for _, r := range current {
		byKey[sampleKey{ts: r.TsMs, driver: r.Driver, metric: r.Metric}] = r
	}
	out := make([]parquetSampleRow, 0, len(byKey))
	for _, r := range byKey {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TsMs != out[j].TsMs {
			return out[i].TsMs < out[j].TsMs
		}
		if out[i].Driver != out[j].Driver {
			return out[i].Driver < out[j].Driver
		}
		return out[i].Metric < out[j].Metric
	})
	return out
}

// LoadSeriesFromParquet reads one (driver, metric) series from cold storage.
// Scans every parquet file whose day overlaps [sinceMs, untilMs]. Filtered in
// process — daily files are small enough that pushdown isn't worth the
// complexity for this dataset size.
func (s *Store) LoadSeriesFromParquet(coldDir, driver, metric string, sinceMs, untilMs int64) ([]Sample, error) {
	if coldDir == "" {
		return nil, nil
	}
	since := time.UnixMilli(sinceMs).UTC()
	until := time.UnixMilli(untilMs).UTC()
	out := make([]Sample, 0, 256)

	for d := since; !d.After(until); d = d.AddDate(0, 0, 1) {
		path := filepath.Join(coldDir,
			fmt.Sprintf("%04d/%02d/%02d.parquet", d.Year(), int(d.Month()), d.Day()))
		rows, err := readParquetDay(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return out, err
		}
		for _, r := range rows {
			if r.Driver == driver && r.Metric == metric &&
				r.TsMs >= sinceMs && r.TsMs <= untilMs {
				out = append(out, Sample{Driver: r.Driver, Metric: r.Metric, TsMs: r.TsMs, Value: r.Value})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TsMs < out[j].TsMs })
	return out, nil
}
