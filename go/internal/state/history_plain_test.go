package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
)

func TestPlainHistoryKeepsBucketsNotRawPolls(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute).UnixMilli()
	for i, v := range []float64{10, 30} {
		if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "grid_w", TsMs: base + int64(i)*1000, Value: v}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	var raw, buckets int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets`).Scan(&buckets); err != nil {
		t.Fatal(err)
	}
	if raw != 0 || buckets < 2 {
		t.Fatalf("raw=%d buckets=%d", raw, buckets)
	}
}

func TestRetireRawHistoryKeepsTheLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "grid_w", TsMs: 1_000, Value: 5}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`INSERT INTO energy_ledger_entries VALUES(1,'site','import',3600000,3600000,12.5,'hardware_counter','measured','counter',1,3600000)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	RetireRawOnOpen = true
	t.Cleanup(func() { RetireRawOnOpen = false })
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var raw int
	var wh float64
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := s.history.QueryRow(`SELECT COALESCE(SUM(energy_wh),0) FROM energy_ledger_entries`).Scan(&wh); err != nil {
		t.Fatal(err)
	}
	if raw != 0 || wh != 12.5 {
		t.Fatalf("raw=%d ledger=%v", raw, wh)
	}
	retired := pathDirHistory(path) + ".raw-retired"
	info, err := os.Stat(retired)
	if err != nil || info.Size() == 0 {
		t.Fatal(err, info)
	}
}

func TestPlainHistoryRollsOldMinutesIntoHours(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-plainMinuteKeep - 24*time.Hour).Truncate(time.Hour)
	for i, v := range []float64{10, 30} {
		ts := old.Add(time.Duration(i) * time.Minute).UnixMilli()
		if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "grid_w", TsMs: ts, Value: v}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainPlainHistory(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	var minutes, hours int
	var sum float64
	var n int64
	hour := old.UnixMilli()
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets WHERE resolution_ms=? AND start_ms>=? AND start_ms<?`, ArchiveResolutionMS, hour, hour+HistoryHourResolutionMS).Scan(&minutes); err != nil {
		t.Fatal(err)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*), COALESCE(SUM(sum_value),0), COALESCE(SUM(n),0) FROM ts_buckets WHERE resolution_ms=? AND start_ms=?`, HistoryHourResolutionMS, hour).Scan(&hours, &sum, &n); err != nil {
		t.Fatal(err)
	}
	if minutes != 0 || hours != 1 || n != 2 || sum != 40 {
		t.Fatalf("minutes=%d hours=%d n=%d sum=%v", minutes, hours, n, sum)
	}
	points, err := s.LoadSeriesBucketsOrRaw("meter", "grid_w", hour, hour+HistoryHourResolutionMS-1, 0)
	if err != nil || len(points) != 1 || points[0].N != 2 || points[0].V != 20 {
		t.Fatalf("chart=%+v err=%v", points, err)
	}
}

func TestAbsorbColdHistoryBecomesHours(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(24 * time.Hour)
	hour := day.Add(3 * time.Hour)
	cold := t.TempDir()
	path := filepath.Join(cold, day.Format("2006/01"), day.Format("02")+".buckets.parquet")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeTestBuckets(path, []metricBucket{
		{Driver: "meter", Metric: "grid_w", BucketSummary: BucketSummary{StartMS: hour.UnixMilli(), ResolutionMS: ArchiveResolutionMS, FirstMS: hour.UnixMilli(), LastMS: hour.UnixMilli() + 1000, N: 1, Sum: 10, Min: 10, Max: 10, Last: 10}},
		{Driver: "meter", Metric: "grid_w", BucketSummary: BucketSummary{StartMS: hour.Add(time.Minute).UnixMilli(), ResolutionMS: ArchiveResolutionMS, FirstMS: hour.Add(time.Minute).UnixMilli(), LastMS: hour.Add(time.Minute).UnixMilli() + 1000, N: 1, Sum: 30, Min: 30, Max: 30, Last: 30}},
	}); err != nil {
		t.Fatal(err)
	}
	diag := filepath.Join(cold, "diagnostics", "keep.parquet")
	if err := os.MkdirAll(filepath.Dir(diag), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diag, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("bucket file still present: %v", err)
	}
	if _, err := os.Stat(diag); err != nil {
		t.Fatal(err)
	}
	points, err := s.LoadSeriesBucketsOrRaw("meter", "grid_w", hour.UnixMilli(), hour.UnixMilli()+HistoryHourResolutionMS-1, 0)
	if err != nil || len(points) != 1 || points[0].N != 2 || points[0].V != 20 {
		t.Fatalf("points=%+v err=%v", points, err)
	}
}

func writeTestBuckets(path string, rows []metricBucket) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := parquet.NewGenericWriter[metricBucket](f)
	if _, err := w.Write(rows); err != nil {
		return err
	}
	return w.Close()
}

func pathDirHistory(statePath string) string {
	return historyDatabasePath(statePath)
}
