package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSaveDiagnosticRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ts := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC).UnixMilli()
	if err := st.SaveDiagnostic(ts, "scheduled", "SE3", 345.6, 96, `{"foo":"bar"}`); err != nil {
		t.Fatalf("SaveDiagnostic: %v", err)
	}
	got, err := st.LoadDiagnosticAt(ts)
	if err != nil {
		t.Fatalf("LoadDiagnosticAt: %v", err)
	}
	if got == nil {
		t.Fatal("got nil; want the row back")
	}
	if got.TsMs != ts || got.Reason != "scheduled" || got.Zone != "SE3" ||
		got.TotalCostOre != 345.6 || got.HorizonSlots != 96 || got.JSON != `{"foo":"bar"}` {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
}

func TestSaveDiagnosticUpsertsOnConflict(t *testing.T) {
	st := openTestStore(t)
	ts := int64(1745000000000)
	_ = st.SaveDiagnostic(ts, "scheduled", "SE3", 100, 96, "v1")
	if err := st.SaveDiagnostic(ts, "manual", "SE3", 200, 48, "v2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ := st.LoadDiagnosticAt(ts)
	if got == nil || got.Reason != "manual" || got.JSON != "v2" {
		t.Errorf("upsert did not replace; got %+v", got)
	}
}

func TestLoadDiagnosticAtFindsClosestEarlier(t *testing.T) {
	st := openTestStore(t)
	// Snapshots at 12:00, 12:15, 12:30
	base := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	for i, q := range []string{"s0", "s1", "s2"} {
		_ = st.SaveDiagnostic(base.Add(time.Duration(i)*15*time.Minute).UnixMilli(),
			"scheduled", "SE3", float64(i), 96, q)
	}
	// Query 12:07 → should return s0 (the 12:00 one — it was the
	// plan driving the EMS at 12:07).
	got, err := st.LoadDiagnosticAt(base.Add(7 * time.Minute).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.JSON != "s0" {
		t.Errorf("at 12:07 expected s0, got %+v", got)
	}
	// Query 12:20 → s1.
	got, _ = st.LoadDiagnosticAt(base.Add(20 * time.Minute).UnixMilli())
	if got == nil || got.JSON != "s1" {
		t.Errorf("at 12:20 expected s1, got %+v", got)
	}
	// Query before the first snapshot → nil (no plan existed yet).
	got, _ = st.LoadDiagnosticAt(base.Add(-1 * time.Minute).UnixMilli())
	if got != nil {
		t.Errorf("before first snapshot expected nil, got %+v", got)
	}
}

func TestLoadDiagnosticsInRangeNewestFirst(t *testing.T) {
	st := openTestStore(t)
	base := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		_ = st.SaveDiagnostic(base.Add(time.Duration(i)*15*time.Minute).UnixMilli(),
			"scheduled", "SE3", float64(i*10), 96, "")
	}
	since := base.UnixMilli()
	until := base.Add(2 * time.Hour).UnixMilli()
	rows, err := st.LoadDiagnosticsInRange(since, until, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(rows))
	}
	// Newest first.
	if rows[0].TsMs <= rows[1].TsMs {
		t.Errorf("not ordered newest first: %v", rows)
	}
	// Limit works.
	rows, _ = st.LoadDiagnosticsInRange(since, until, 2)
	if len(rows) != 2 {
		t.Errorf("limit=2 returned %d rows", len(rows))
	}
}

func TestDeleteDiagnosticsBefore(t *testing.T) {
	st := openTestStore(t)
	base := time.Date(2026, 4, 18, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		_ = st.SaveDiagnostic(base.Add(time.Duration(i)*time.Hour).UnixMilli(),
			"scheduled", "SE3", 0, 96, "")
	}
	cutoff := base.Add(5 * time.Hour).UnixMilli()
	n, err := st.DeleteDiagnosticsBefore(context.Background(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("DeleteBefore returned n=%d, want 5", n)
	}
	rows, _ := st.LoadDiagnosticsInRange(0, base.Add(24*time.Hour).UnixMilli(), 0)
	if len(rows) != 5 {
		t.Errorf("after delete expected 5 rows, got %d", len(rows))
	}
}

func TestDiagnosticsBeforeBatches(t *testing.T) {
	st := openTestStore(t)
	base := time.Date(2026, 4, 18, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		_ = st.SaveDiagnostic(base.Add(time.Duration(i)*time.Hour).UnixMilli(),
			"scheduled", "SE3", float64(i), 96, "")
	}
	cutoff := base.Add(100 * time.Hour).UnixMilli()
	var visited int
	err := st.DiagnosticsBefore(context.Background(), cutoff, 8, func(batch []DiagnosticRow) error {
		visited += len(batch)
		// Each batch should be monotonic in ts.
		for i := 1; i < len(batch); i++ {
			if batch[i].TsMs <= batch[i-1].TsMs {
				t.Errorf("non-monotonic batch: %v", batch)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if visited != 30 {
		t.Errorf("visited %d rows, want 30", visited)
	}
}

// Installs from before 0.x may still hold diagnostics day files in cold
// storage. The API reads them back as summaries and as full snapshots.
func TestDiagnosticsParquetReaders(t *testing.T) {
	st := openTestStore(t)
	coldDir := t.TempDir()
	day := time.Now().Add(-60 * 24 * time.Hour).UTC().Truncate(24 * time.Hour)
	rows := make([]parquetDiagRow, 5)
	for i := range rows {
		rows[i] = parquetDiagRow{
			TsMs: day.Add(time.Duration(i) * 2 * time.Hour).UnixMilli(), Reason: "scheduled", Zone: "SE3",
			TotalCostOre: float64(i), HorizonSlots: 96, JSON: `{"idx":` + itoa(i) + `}`,
		}
	}
	path := filepath.Join(coldDir, "diagnostics", day.Format("2006/01/02.parquet"))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := parquet.NewGenericWriter[parquetDiagRow](f)
	if _, err := w.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(w.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}

	summaries, err := st.LoadDiagnosticsFromParquet(coldDir, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli())
	if err != nil || len(summaries) != 5 {
		t.Fatalf("cold summaries = %+v, %v; want 5", summaries, err)
	}
	if summaries[0].TsMs != rows[0].TsMs || summaries[4].TotalCostOre != 4 || summaries[4].HorizonSlots != 96 {
		t.Fatalf("cold summaries = %+v", summaries)
	}
	// The closest snapshot at or before the requested time is returned.
	full, err := st.LoadDiagnosticFullFromParquet(coldDir, day.Add(5*time.Hour).UnixMilli())
	if err != nil || full == nil || full.TsMs != rows[2].TsMs || full.JSON != rows[2].JSON {
		t.Fatalf("cold full row = %+v, %v; want %+v", full, err, rows[2])
	}
}

// itoa — avoid strconv import for a tiny helper in tests.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [12]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
