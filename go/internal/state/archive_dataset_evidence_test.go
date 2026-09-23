package state

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Compare a completed day with the union of SQLite and Parquet from a separate
// restored baseline. Neither directory may be a running installation.
func TestHistoryArchiveDatasetEvidence(t *testing.T) {
	before, after, date := os.Getenv("FTW_EVIDENCE_BASELINE"), os.Getenv("FTW_EVIDENCE_RESULT"), os.Getenv("FTW_EVIDENCE_DAY")
	if before == "" || after == "" || date == "" {
		t.Skip("set FTW_EVIDENCE_BASELINE, FTW_EVIDENCE_RESULT and FTW_EVIDENCE_DAY to disposable copies")
	}
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		t.Fatal(err)
	}
	src, err := openDurableHistory(filepath.Join(before, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	stage, err := openArchiveStage(filepath.Join(t.TempDir(), "expected.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rel := filepath.Join("cold", day.Format("2006/01/02.parquet"))
	err = walkParquetRows(ctx, filepath.Join(before, rel), func(rows []parquetSampleRow) error { return insertArchiveRows(ctx, stage, rows) })
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	rows, err := src.QueryContext(ctx, `SELECT s.ts_ms,d.name,m.name,s.value FROM ts_samples s JOIN ts_drivers d ON d.id=s.driver_id JOIN ts_metrics m ON m.id=s.metric_id WHERE s.ts_ms>=? AND s.ts_ms<? ORDER BY s.ts_ms,s.driver_id,s.metric_id`, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	batch := make([]parquetSampleRow, 0, 1024)
	for rows.Next() {
		var r parquetSampleRow
		if err := rows.Scan(&r.TsMs, &r.Driver, &r.Metric, &r.Value); err != nil {
			t.Fatal(err)
		}
		batch = append(batch, r)
		if len(batch) == cap(batch) {
			if err := insertArchiveRows(ctx, stage, batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		t.Fatal(err)
	}
	if err := insertArchiveRows(ctx, stage, batch); err != nil {
		t.Fatal(err)
	}
	expected := sha256.New()
	var want int64
	rows, err = stage.QueryContext(ctx, `SELECT ts_ms,driver,metric,value FROM samples ORDER BY ts_ms,driver,metric`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r parquetSampleRow
		if err := rows.Scan(&r.TsMs, &r.Driver, &r.Metric, &r.Value); err != nil {
			t.Fatal(err)
		}
		if err := hashHistoryRow(expected, []any{r.TsMs, r.Driver, r.Metric, r.Value}); err != nil {
			t.Fatal(err)
		}
		want++
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		t.Fatal(err)
	}
	actual := sha256.New()
	var got int64
	if err := walkParquetRows(ctx, filepath.Join(after, rel), func(rows []parquetSampleRow) error {
		for _, r := range rows {
			if err := hashHistoryRow(actual, []any{r.TsMs, r.Driver, r.Metric, r.Value}); err != nil {
				return err
			}
			got++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantHash, gotHash := fmt.Sprintf("%x", expected.Sum(nil)), fmt.Sprintf("%x", actual.Sum(nil))
	if got != want || gotHash != wantHash {
		t.Fatalf("day=%s counts=%d/%d hashes=%s/%s", date, want, got, wantHash, gotHash)
	}
	t.Logf("verified day=%s rows=%d sha256=%s", date, got, gotHash)
}
