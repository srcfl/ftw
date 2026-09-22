package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	old := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Hour)
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

func pathDirHistory(statePath string) string {
	return historyDatabasePath(statePath)
}
