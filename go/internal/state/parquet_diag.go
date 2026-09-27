package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"
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
