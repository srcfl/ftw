package state

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/parquet-go/parquet-go"
)

func TestHistoryParquetResumesCommittedChunkAndRejectsChangedSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	cold := filepath.Join(dir, "cold")
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(day, "01.parquet")
	points := make([]parquetSampleRow, historyImportRows*2+17)
	for i := range points {
		// Deliberately unsorted; row position must not be a sample identity.
		points[i] = parquetSampleRow{TsMs: int64(len(points) - i), Driver: "meter", Metric: "power", Value: math.Nextafter(float64(i+1), math.Inf(1))}
	}
	if err := writeParquetDay(file, points); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "power", TsMs: points[0].TsMs, Value: 99}}); err != nil {
		t.Fatal(err)
	}
	conn, err := s.history.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE history_import_source (ts_ms BIGINT NOT NULL,driver_id BIGINT NOT NULL,metric_id BIGINT NOT NULL,value DOUBLE NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	n, err := stageHistoryParquet(ctx, conn, file)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := historyFileHash(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO history_parquet_imports VALUES (?,?)`, file, digest); err != nil {
		t.Fatal(err)
	}
	if err := importHistoryChunk(ctx, conn, 0, n); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BackupToCompressed(filepath.Join(dir, "partial.gz")); err == nil {
		t.Fatal("exported a partial migration with overlapping cold files")
	}
	points[0].Value = 1234
	if err := writeParquetDay(file, points); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyParquet(ctx, cold); err == nil {
		t.Fatal("accepted changed source after a committed chunk")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyParquet(ctx, cold); err == nil {
		t.Fatal("started without an interrupted source")
	}
	if err := os.WriteFile(file, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyParquet(ctx, cold); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyParquet(ctx, cold); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSeries("meter", "power", 0, int64(len(points)), 0)
	if err != nil || len(got) != len(points) {
		t.Fatalf("rows=%d want=%d err=%v", len(got), len(points), err)
	}
	for i, p := range got {
		want := points[len(points)-i-1].Value
		if p.TsMs == points[0].TsMs {
			want = 99
		}
		if math.Float64bits(p.Value) != math.Float64bits(want) {
			t.Fatalf("timestamp %d: %.17g want %.17g", p.TsMs, p.Value, want)
		}
	}
	var pending int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_parquet_imports`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
}

func TestHistoryParquetRejectsCrossChunkDuplicatesBeforePrimaryWrite(t *testing.T) {
	s := freshStore(t)
	cold := t.TempDir()
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	points := make([]parquetSampleRow, historyImportRows+1)
	for i := range points {
		points[i] = parquetSampleRow{TsMs: int64(i), Driver: "meter", Metric: "power", Value: 1}
	}
	points[len(points)-1] = points[0]
	if err := writeParquetDay(filepath.Join(day, "01.parquet"), points); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyParquet(context.Background(), cold); err == nil {
		t.Fatal("accepted duplicate in different chunks")
	}
	var count int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partially imported invalid source: count=%d err=%v", count, err)
	}
}

func TestHistoryParquetRejectsNullValue(t *testing.T) {
	s := freshStore(t)
	cold := t.TempDir()
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	type row struct {
		Ts     int64    `parquet:"ts_ms"`
		Driver string   `parquet:"driver"`
		Metric string   `parquet:"metric"`
		Value  *float64 `parquet:"value"`
	}
	if err := parquet.WriteFile(filepath.Join(day, "01.parquet"), []row{{Ts: 1, Driver: "meter", Metric: "power"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyParquet(context.Background(), cold); err == nil {
		t.Fatal("converted a null sample to zero")
	}
}

func TestHistoryTableReadbackCrossesPages(t *testing.T) {
	s := freshStore(t)
	points := make([]HistoryPoint, 2048*2+1)
	for i := range points {
		points[i] = HistoryPoint{TsMs: int64(i), GridW: float64(i), JSON: "{}"}
	}
	if err := s.BulkRecordHistory(points); err != nil {
		t.Fatal(err)
	}
	var seen int64
	n, err := scanHistoryTable(context.Background(), s.history, "history_hot", func(values []any) error {
		if values[0].(int64) != seen || values[1].(float64) != float64(seen) {
			t.Fatalf("readback skipped or repeated row %d: %v", seen, values)
		}
		seen++
		return nil
	})
	if err != nil || n != int64(len(points)) {
		t.Fatalf("count=%d seen=%d err=%v", n, seen, err)
	}
}
