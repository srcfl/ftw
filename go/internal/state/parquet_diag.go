package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
)

// parquetDiagRow is the column-oriented form of DiagnosticRow. Reason +
// zone get dictionary-encoded (only a handful of distinct values across
// thousands of rows). The JSON blob is compressed with zstd alongside.
type parquetDiagRow struct {
	TsMs         int64   `parquet:"ts_ms"`
	Reason       string  `parquet:"reason,dict,zstd"`
	Zone         string  `parquet:"zone,dict,zstd"`
	TotalCostOre float64 `parquet:"total_cost_ore,zstd"`
	HorizonSlots int64   `parquet:"horizon_slots,zstd"`
	JSON         string  `parquet:"json,zstd"`
}

// RolloffDiagnosticsToParquet exports snapshots older than
// DiagnosticsRecentRetention into one parquet file per UTC day under
// <coldDir>/diagnostics/YYYY/MM/DD.parquet, then deletes the rolled-off
// rows from SQLite. Mirrors RolloffToParquet in structure; the diagnostics
// live in their own subdirectory so they don't collide with ts_samples
// files at the cold-storage root.
//
// Idempotent: re-running for a day that already has a file rewrites it.
func (s *Store) RolloffDiagnosticsToParquet(ctx context.Context, coldDir string) (rolledRows int64, files []string, err error) {
	if coldDir == "" {
		return 0, nil, fmt.Errorf("RolloffDiagnosticsToParquet: coldDir must be set")
	}
	cutoff := time.Now().Add(-DiagnosticsRecentRetention).UnixMilli()

	type dayKey struct{ year, month, day int }
	byDay := make(map[dayKey][]parquetDiagRow, 4)

	err = s.DiagnosticsBefore(ctx, cutoff, 512, func(batch []DiagnosticRow) error {
		for _, r := range batch {
			t := time.UnixMilli(r.TsMs).UTC()
			k := dayKey{t.Year(), int(t.Month()), t.Day()}
			byDay[k] = append(byDay[k], parquetDiagRow{
				TsMs:         r.TsMs,
				Reason:       r.Reason,
				Zone:         r.Zone,
				TotalCostOre: r.TotalCostOre,
				HorizonSlots: int64(r.HorizonSlots),
				JSON:         r.JSON,
			})
		}
		return nil
	})
	if err != nil {
		return 0, nil, fmt.Errorf("read diagnostics: %w", err)
	}
	if len(byDay) == 0 {
		return 0, nil, nil
	}

	diagDir := filepath.Join(coldDir, "diagnostics")
	for k, rows := range byDay {
		sort.Slice(rows, func(i, j int) bool { return rows[i].TsMs < rows[j].TsMs })
		dayDir := filepath.Join(diagDir, fmt.Sprintf("%04d/%02d", k.year, k.month))
		if err := os.MkdirAll(dayDir, 0o755); err != nil {
			return rolledRows, files, fmt.Errorf("mkdir %s: %w", dayDir, err)
		}
		path := filepath.Join(dayDir, fmt.Sprintf("%02d.parquet", k.day))
		if err := writeParquetDiagDay(path, rows); err != nil {
			return rolledRows, files, fmt.Errorf("write %s: %w", path, err)
		}
		files = append(files, path)
		rolledRows += int64(len(rows))
	}

	// Atomic delete of the rolled-off rows.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM planner_diagnostics WHERE ts_ms < ?`, cutoff); err != nil {
		return rolledRows, files, fmt.Errorf("delete rolled diagnostics: %w", err)
	}
	return rolledRows, files, nil
}

func writeParquetDiagDay(path string, rows []parquetDiagRow) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := parquet.NewGenericWriter[parquetDiagRow](f,
		parquet.Compression(&zstd.Codec{Level: zstd.DefaultLevel}))
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
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// LoadDiagnosticsFromParquet reads snapshot summaries from cold storage
// for the given range. Omits the heavy JSON blob — callers that need the
// full Diagnostic call LoadDiagnosticFullFromParquet with a specific ts.
func (s *Store) LoadDiagnosticsFromParquet(coldDir string, sinceMs, untilMs int64) ([]DiagnosticSummary, error) {
	if coldDir == "" {
		return nil, nil
	}
	since := time.UnixMilli(sinceMs).UTC()
	until := time.UnixMilli(untilMs).UTC()
	out := make([]DiagnosticSummary, 0, 64)
	diagDir := filepath.Join(coldDir, "diagnostics")
	for d := since; !d.After(until); d = d.AddDate(0, 0, 1) {
		path := filepath.Join(diagDir,
			fmt.Sprintf("%04d/%02d/%02d.parquet", d.Year(), int(d.Month()), d.Day()))
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return out, err
		}
		stat, err := f.Stat()
		if err != nil {
			f.Close()
			return out, err
		}
		pf, err := parquet.OpenFile(f, stat.Size())
		if err != nil {
			f.Close()
			return out, err
		}
		reader := parquet.NewGenericReader[parquetDiagRow](pf)
		buf := make([]parquetDiagRow, 256)
		for {
			n, rerr := reader.Read(buf)
			for i := 0; i < n; i++ {
				r := buf[i]
				if r.TsMs < sinceMs || r.TsMs > untilMs {
					continue
				}
				out = append(out, DiagnosticSummary{
					TsMs:         r.TsMs,
					Reason:       r.Reason,
					Zone:         r.Zone,
					TotalCostOre: r.TotalCostOre,
					HorizonSlots: int(r.HorizonSlots),
				})
			}
			if rerr != nil {
				break
			}
		}
		reader.Close()
		f.Close()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TsMs < out[j].TsMs })
	return out, nil
}

// LoadDiagnosticFullFromParquet returns the single snapshot whose ts_ms
// is closest to and ≤ the given ts. Used when the UI clicks a point in
// cold storage and wants the full JSON blob.
func (s *Store) LoadDiagnosticFullFromParquet(coldDir string, tsMs int64) (*DiagnosticRow, error) {
	if coldDir == "" {
		return nil, nil
	}
	// Look in the target day's file first; fall back to earlier days
	// when the target day has no rows ≤ tsMs.
	t := time.UnixMilli(tsMs).UTC()
	diagDir := filepath.Join(coldDir, "diagnostics")
	for i := 0; i < 30; i++ { // scan up to 30 days back
		d := t.AddDate(0, 0, -i)
		path := filepath.Join(diagDir,
			fmt.Sprintf("%04d/%02d/%02d.parquet", d.Year(), int(d.Month()), d.Day()))
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		stat, _ := f.Stat()
		pf, err := parquet.OpenFile(f, stat.Size())
		if err != nil {
			f.Close()
			return nil, err
		}
		reader := parquet.NewGenericReader[parquetDiagRow](pf)
		buf := make([]parquetDiagRow, 256)
		var best *parquetDiagRow
		for {
			n, rerr := reader.Read(buf)
			for j := 0; j < n; j++ {
				r := buf[j]
				if r.TsMs > tsMs {
					continue
				}
				if best == nil || r.TsMs > best.TsMs {
					cp := r
					best = &cp
				}
			}
			if rerr != nil {
				break
			}
		}
		reader.Close()
		f.Close()
		if best != nil {
			return &DiagnosticRow{
				DiagnosticSummary: DiagnosticSummary{
					TsMs:         best.TsMs,
					Reason:       best.Reason,
					Zone:         best.Zone,
					TotalCostOre: best.TotalCostOre,
					HorizonSlots: int(best.HorizonSlots),
				},
				JSON: best.JSON,
			}, nil
		}
	}
	return nil, nil
}
