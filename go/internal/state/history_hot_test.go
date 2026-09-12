package state

import (
	"context"
	"testing"
	"time"
)

func TestLiveTicksStayInSQLiteUntilSeal(t *testing.T) {
	s := freshStore(t)
	now := time.Now().UnixMilli()
	p := HistoryPoint{TsMs: now, GridW: 1000, PVW: -2000, JSON: "{}"}
	samples := []Sample{{TsMs: now, Driver: "meter", Metric: "power", Value: 42}}
	if err := s.EnqueueTelemetryTick(&p, samples, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	var hot, arch int
	if err := s.hot.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&hot); err != nil || hot != 1 {
		t.Fatalf("sqlite hot=%d %v", hot, err)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&arch); err != nil || arch != 0 {
		t.Fatalf("duckdb archive=%d %v", arch, err)
	}
	pts, err := s.LoadHistory(now-60_000, now+1000, 360)
	if err != nil || len(pts) != 1 || pts[0].GridW != 1000 {
		t.Fatalf("1h history=%v %v", pts, err)
	}
	got, err := s.LoadSeries("meter", "power", 0, now+1000, 0)
	if err != nil || len(got) != 1 || got[0].Value != 42 {
		t.Fatalf("series=%v %v", got, err)
	}
}

func TestLiveDayEnergyUsesSQLiteOnly(t *testing.T) {
	s := freshStore(t)
	base := time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC)
	if err := s.RecordHistory(HistoryPoint{TsMs: base.UnixMilli(), GridW: 1000, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHistory(HistoryPoint{TsMs: base.Add(5 * time.Minute).UnixMilli(), GridW: 1000, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`SET memory_limit='2MB'`); err != nil {
		t.Fatal(err)
	}
	d, err := s.LiveDayEnergy(base.Add(-12*time.Hour).UnixMilli(), base.Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("status energy touched DuckDB: %v", err)
	}
	if d.Intervals != 1 {
		t.Fatalf("intervals=%d", d.Intervals)
	}
}

func TestDailyEnergySkipsArchiveOOM(t *testing.T) {
	s := freshStore(t)
	base := time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC)
	if err := s.RecordHistory(HistoryPoint{TsMs: base.UnixMilli(), GridW: 1000, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHistory(HistoryPoint{TsMs: base.Add(5 * time.Minute).UnixMilli(), GridW: 1000, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`SET memory_limit='2MB'`); err != nil {
		t.Fatal(err)
	}
	d, err := s.DailyEnergy(base.Add(-time.Hour).UnixMilli(), base.Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("live energy failed when archive was OOM: %v", err)
	}
	if d.Intervals != 1 {
		t.Fatalf("intervals=%d", d.Intervals)
	}
}

func TestSealCopiesLiveTicksIntoDuckDB(t *testing.T) {
	s := freshStore(t)
	now := time.Now().UnixMilli()
	p := HistoryPoint{TsMs: now, GridW: 500, JSON: "{}"}
	if err := s.EnqueueTelemetryTick(&p, []Sample{{TsMs: now, Driver: "inv", Metric: "pv_w", Value: -800}}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.sealAndPruneHot(ctx, now+1, 0); err != nil {
		t.Fatal(err)
	}
	var arch int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_hot WHERE ts_ms=?`, now).Scan(&arch); err != nil || arch != 1 {
		t.Fatalf("sealed history=%d %v", arch, err)
	}
	var samples int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&samples); err != nil || samples != 1 {
		t.Fatalf("sealed samples=%d %v", samples, err)
	}
}

func TestLoadHistoryMergesSQLiteAndDuckDB(t *testing.T) {
	s := freshStore(t)
	old := HistoryPoint{TsMs: 1_000, GridW: 100, JSON: "{}"}
	if err := s.BulkRecordHistory([]HistoryPoint{old}); err != nil {
		t.Fatal(err)
	}
	live := HistoryPoint{TsMs: 2_000, GridW: 200, JSON: "{}"}
	if err := s.RecordHistory(live); err != nil {
		t.Fatal(err)
	}
	pts, err := s.LoadHistory(0, 3_000, 0)
	if err != nil || len(pts) != 2 {
		t.Fatalf("merged=%v %v", pts, err)
	}
	if pts[0].GridW != 100 || pts[1].GridW != 200 {
		t.Fatalf("order/values=%v", pts)
	}
}

func TestDailyEnergyUsesLiveSQLite(t *testing.T) {
	s := freshStore(t)
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if err := s.RecordHistory(HistoryPoint{TsMs: base.UnixMilli(), GridW: 2000, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHistory(HistoryPoint{TsMs: base.Add(5 * time.Minute).UnixMilli(), GridW: 2000, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.DailyEnergy(base.UnixMilli(), base.Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if d.Intervals != 1 {
		t.Fatalf("intervals=%d", d.Intervals)
	}
	want := 2000.0 * 5 / 60
	if d.ImportWh < want*0.99 || d.ImportWh > want*1.01 {
		t.Fatalf("ImportWh=%v want ~%v", d.ImportWh, want)
	}
}

func TestPruneHotDropsSealedRowsPastRetention(t *testing.T) {
	s := freshStore(t)
	old := time.Now().Add(-50 * time.Hour).UnixMilli()
	p := HistoryPoint{TsMs: old, GridW: 1, JSON: "{}"}
	if err := s.EnqueueTelemetryTick(&p, []Sample{{TsMs: old, Driver: "m", Metric: "p", Value: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.sealAndPruneHot(ctx, old+1, old+1); err != nil {
		t.Fatal(err)
	}
	var hot int
	if err := s.hot.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&hot); err != nil || hot != 0 {
		t.Fatalf("pruned hot=%d %v", hot, err)
	}
	var arch int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&arch); err != nil || arch != 1 {
		t.Fatalf("archive after prune=%d %v", arch, err)
	}
}
