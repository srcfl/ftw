package state

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestPlainHistoryRetentionIncludesSummaryTables(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	cutoff := bucketStart(now.Add(-plainHourKeep).UnixMilli(), HistoryHourResolutionMS)
	for _, ts := range []int64{cutoff - HistoryHourResolutionMS, cutoff, now.UnixMilli()} {
		if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "amps", TsMs: ts, Value: 32}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureSeriesHours(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainPlainHistory(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ts_buckets", "ts_series_hour", "ts_aggregate_hours"} {
		column := "hour_ms"
		if table == "ts_buckets" {
			column = "start_ms"
		}
		var old, kept int
		if err := s.history.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+column+`<?`, cutoff).Scan(&old); err != nil {
			t.Fatal(err)
		}
		if err := s.history.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+column+`=?`, cutoff).Scan(&kept); err != nil {
			t.Fatal(err)
		}
		if old != 0 || kept != 1 {
			t.Errorf("%s: expired=%d boundary=%d", table, old, kept)
		}
	}
	points, err := s.LoadSeriesBucketsOrRaw("meter", "amps", cutoff-2*HistoryHourResolutionMS, cutoff-1, 1000)
	if err != nil || len(points) != 0 {
		t.Fatalf("expired graph=%+v error=%v", points, err)
	}
}

func TestPlainRollupWaitsForWriterAndRetainsLateSamples(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	hour := time.Now().UTC().Add(-plainMinuteKeep - time.Hour).Truncate(time.Hour).UnixMilli()
	add := func(ts int64, value float64) {
		t.Helper()
		if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "amps", TsMs: ts, Value: value}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.FlushHistory(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	add(hour, 8)
	add(hour+5000, 32)
	add(hour+60000, 16)
	s.historyWriteMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := s.rollMinuteHour(ctx, hour)
	cancel()
	s.historyWriteMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rollup ignored writer lock: %v", err)
	}
	var count int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets WHERE resolution_ms=60000`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("source changed before commit: %d %v", count, err)
	}
	if err := s.rollMinuteHour(context.Background(), hour); err != nil {
		t.Fatal(err)
	}
	// A resumed no-op must not erase the hour; a late minute adds its own values.
	if err := s.rollMinuteHour(context.Background(), hour); err != nil {
		t.Fatal(err)
	}
	add(hour+120000, 12)
	if err := s.rollMinuteHour(context.Background(), hour); err != nil {
		t.Fatal(err)
	}
	var n int64
	var sum, minV, maxV, last float64
	err = s.history.QueryRow(`SELECT n,sum_value,min_value,max_value,last_value FROM ts_buckets WHERE resolution_ms=3600000 AND start_ms=?`, hour).Scan(&n, &sum, &minV, &maxV, &last)
	if err != nil || n != 4 || sum != 68 || minV != 8 || maxV != 32 || last != 12 {
		t.Fatalf("n=%d sum=%v min=%v max=%v last=%v error=%v", n, sum, minV, maxV, last, err)
	}
}

func TestPlainDeleteWaitsForWriter(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "amps", TsMs: 1000, Value: 32}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.historyWriteMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := s.deleteBucketSpan(ctx, HistoryResolutionMS, 60000)
	cancel()
	s.historyWriteMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delete ignored writer lock: %v", err)
	}
	var count int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets WHERE resolution_ms=10000`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("source deleted: %d %v", count, err)
	}
	if err := s.deleteBucketSpan(context.Background(), HistoryResolutionMS, 60000); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteChartBucketsMatchStoredHistory(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	s.historyWriter.aggregateBeforeMS = -10_000_000
	// Include negative timestamps, gaps, overlapping resolutions, different
	// series and maxima that differ from the last observed value.
	for _, driver := range []string{"meter", "other"} {
		for i, ts := range []int64{-7200000, -7195000, -7140000, -3600000, -3595000, -3540000, 0, 5000, 60000, 65000, 180000, 3600000} {
			value := []float64{8, 32, 16, 7}[i%4]
			if driver == "other" {
				value = 9000
			}
			if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: driver, Metric: "amps", TsMs: ts, Value: value}}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteBucketSpan(context.Background(), HistoryResolutionMS, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.rollMinuteHour(context.Background(), -7200000); err != nil {
		t.Fatal(err)
	}
	for _, window := range [][2]int64{{-7200000, 3600000}, {-7199000, -7100000}, {-3600000, 65000}, {1000, 64000}, {120000, 170000}, {0, 0}} {
		for _, budget := range []int{1, 2, 5, 100} {
			t.Run(fmt.Sprintf("%d/%d/%d", window[0], window[1], budget), func(t *testing.T) {
				width := BucketWidthMs(window[0], window[1], budget)
				expected := map[int64]*seriesBucketAcc{}
				err := s.walkMergedSeriesStats(context.Background(), s.coldDir, "meter", "amps", window[0], window[1], func(b BucketSummary) error {
					key := (b.LastMS - window[0]) / width
					if expected[key] == nil {
						expected[key] = &seriesBucketAcc{}
					}
					expected[key].addBucket(b)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				got, err := s.mergedSeries(context.Background(), s.coldDir, "meter", "amps", window[0], window[1], budget)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != len(expected) || len(got) > budget {
					t.Fatalf("got %d want %d budget %d", len(got), len(expected), budget)
				}
				for _, p := range got {
					a := expected[(p.TsMs-window[0])/width]
					if a == nil {
						t.Fatalf("unexpected point %+v", p)
					}
					want := a.point()
					if math.Abs(p.V-want.V) > 1e-9 {
						t.Fatalf("mean=%v want=%v", p.V, want.V)
					}
					p.V = want.V
					if !reflect.DeepEqual(p, want) {
						t.Fatalf("got %+v want %+v", p, want)
					}
				}
			})
		}
	}
}

func TestPlainMaintenanceHonorsBackupPauseAndRollsEnergy(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	old := now.Add(-EnergyLedgerRetention - 24*time.Hour).UnixMilli()
	if _, err := s.history.Exec(`INSERT INTO energy_ledger_entries VALUES(1,'site','import',?,300000,12.5,'hardware_counter','measured','counter',1,?)`, old, old); err != nil {
		t.Fatal(err)
	}
	resume, err := s.PauseHistoryMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainPlainHistory(context.Background(), now); err != nil {
		resume()
		t.Fatal(err)
	}
	if got := s.HistoryMaintenanceStatus(); got.State != "paused" {
		resume()
		t.Fatalf("maintenance ignored backup pause: %+v", got)
	}
	resume()
	if err := s.MaintainPlainHistory(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var count int
	var width int64
	var energy float64
	if err := s.history.QueryRow(`SELECT COUNT(*),bucket_len_ms,SUM(energy_wh) FROM energy_ledger_entries`).Scan(&count, &width, &energy); err != nil {
		t.Fatal(err)
	}
	if count != 1 || width != EnergyLedgerDailyBucketMS || energy != 12.5 {
		t.Fatalf("energy changed or not rolled: count=%d width=%d wh=%v", count, width, energy)
	}
	if got := s.HistoryMaintenanceStatus(); got.State != "complete" || got.Runs != 1 {
		t.Fatalf("maintenance not visible: %+v", got)
	}
	policy := s.HistoryBackend()["policy"].(map[string]any)
	if policy["recent_retention_hours"] != 168 || policy["archive_minute_days"] != 90 || policy["detailed_retention_days"] != 1825 || s.HistoryBackend()["archive"] != "sqlite" {
		t.Fatalf("wrong policy: %v", policy)
	}
}

func TestSQLiteChartIncludesRetiredHoursBesideRecentDetail(t *testing.T) {
	s := freshStore(t)
	old := time.Now().UTC().Add(-15 * 24 * time.Hour).Truncate(time.Hour).UnixMilli()
	recent := old + 14*86400000
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "amps", TsMs: old, Value: 8}, {Driver: "meter", Metric: "amps", TsMs: old + 5000, Value: 32}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureSeriesHours(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Match an upgraded box: raw rows are gone, but their hourly record remains.
	if _, err := s.history.Exec(`DELETE FROM ts_samples`); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "amps", TsMs: recent, Value: 16}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	points, err := s.LoadSeriesBucketsOrRaw("meter", "amps", old, recent+1000, 1000)
	if err != nil || len(points) != 2 {
		t.Fatalf("mixed chart: %+v %v", points, err)
	}
	if p := points[0]; p.N != 2 || p.V != 20 || p.Min != 8 || p.Max != 32 || p.Last != nil || p.FirstMS != 0 || p.ResolutionMS != HistoryHourResolutionMS {
		t.Fatalf("retired evidence changed: %+v", p)
	}
	if p := points[1]; p.N != 1 || p.V != 16 || p.Last == nil || *p.Last != 16 {
		t.Fatalf("recent evidence changed or doubled: %+v", p)
	}
	// The direct merged path must also combine both contributions when they
	// share one output bucket, preserving the actual newest observation.
	points, err = s.mergedSeries(context.Background(), s.coldDir, "meter", "amps", old, recent+1000, 1)
	if err != nil || len(points) != 1 || points[0].N != 3 || math.Abs(points[0].V-56.0/3) > 1e-9 || points[0].Last == nil || *points[0].Last != 16 || points[0].FirstMS != 0 {
		t.Fatalf("combined chart: %+v %v", points, err)
	}
}
