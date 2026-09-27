package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAbsorbColdHistoryReconcilesPublishedSource(t *testing.T) {
	for _, partialPrune := range []bool{false, true} {
		t.Run(map[bool]string{false: "unpruned", true: "partly-pruned"}[partialPrune], func(t *testing.T) {
			s := freshStore(t)
			if err := s.EnableHistoryAggregation(); err != nil {
				t.Fatal(err)
			}
			hour := time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
			base := hour.UnixMilli()
			add := func(offset int64, value float64) {
				t.Helper()
				if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "amps", TsMs: base + offset, Value: value}}, nil); err != nil {
					t.Fatal(err)
				}
				if err := s.FlushHistory(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			add(1000, 8)
			add(61000, 32)
			stored, err := s.storedBuckets(context.Background(), "meter", "amps", ArchiveResolutionMS, base, base+119999)
			if err != nil || len(stored) != 2 {
				t.Fatal(stored, err)
			}
			archive := []metricBucket{{Driver: "meter", Metric: "amps", BucketSummary: stored[0]}, {Driver: "meter", Metric: "amps", BucketSummary: stored[1]}}
			cold := t.TempDir()
			path := filepath.Join(cold, hour.Format("2006/01/02.buckets.parquet"))
			publish := func() {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := writeTestBuckets(path, archive); err != nil {
					t.Fatal(err)
				}
			}
			publish()
			if partialPrune {
				if err := s.pruneBucketMinutes(context.Background(), archive[:1]); err != nil {
					t.Fatal(err)
				}
			}
			// One published source changed; another minute was never archived.
			add(62000, 16)
			add(121000, 12)
			if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
				t.Fatal(err)
			}
			var fine int
			if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets WHERE resolution_ms<3600000`).Scan(&fine); err != nil || fine != 0 {
				t.Fatalf("overlapping source remains: %d %v", fine, err)
			}
			// Simulate a committed import whose file removal was lost on restart.
			// A later contribution must survive the receipt check and roll once.
			publish()
			add(181000, 4)
			if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
				t.Fatal(err)
			}
			if err := s.MaintainPlainHistory(context.Background(), hour.Add(plainMinuteKeep+24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			points, err := s.LoadSeriesBucketsOrRaw("meter", "amps", base, base+HistoryHourResolutionMS-1, 1000)
			if err != nil || len(points) != 1 || points[0].N != 5 || points[0].V != 14.4 || points[0].Min != 4 || points[0].Max != 32 {
				t.Fatalf("rolled import: %+v %v", points, err)
			}
			var last float64
			if err := s.history.QueryRow(`SELECT last_value FROM ts_buckets WHERE resolution_ms=3600000`).Scan(&last); err != nil || last != 4 {
				t.Fatalf("last=%v error=%v", last, err)
			}
		})
	}
}

func TestAbsorbColdHistoryRollsBackSourceRemovalFailure(t *testing.T) {
	s, cold, path, base := coldImportFixture(t, ArchiveResolutionMS)
	if _, err := s.history.Exec(`CREATE TRIGGER fail_import_delete BEFORE DELETE ON ts_buckets BEGIN SELECT RAISE(ABORT, 'injected source removal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.AbsorbColdHistory(context.Background(), cold); err == nil {
		t.Fatal("ignored source removal failure")
	}
	assertColdImportIntact(t, s, path)
	if _, err := s.history.Exec(`DROP TRIGGER fail_import_delete`); err != nil {
		t.Fatal(err)
	}
	if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
		t.Fatal(err)
	}
	points, err := s.LoadSeriesBucketsOrRaw("meter", "amps", base, base+HistoryHourResolutionMS-1, 1000)
	if err != nil || len(points) != 1 || points[0].N != 1 || points[0].V != 8 {
		t.Fatal(points, err)
	}
}

func TestAbsorbColdHistoryRetainsAmbiguousCompactedOverlap(t *testing.T) {
	s, cold, path, _ := coldImportFixture(t, OldArchiveResolutionMS)
	if err := s.AbsorbColdHistory(context.Background(), cold); err == nil {
		t.Fatal("accepted partial overlap with a compacted archive")
	}
	assertColdImportIntact(t, s, path)
}

// 2.x wrote raw sample days with no hourly summary. Absorbing cold history
// must keep such a day until a summary receipt matches the file.
func TestAbsorbColdHistoryKeepsUnsummarizedSampleDays(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	day := time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(24 * time.Hour)
	hour := day.Add(3 * time.Hour).UnixMilli()
	cold := t.TempDir()
	s.coldDir = cold
	path := filepath.Join(cold, day.Format("2006/01/02.parquet"))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	rows := []parquetSampleRow{
		{TsMs: hour + 1000, Driver: "meter", Metric: "grid_w", Value: 10},
		{TsMs: hour + 2000, Driver: "meter", Metric: "grid_w", Value: 30},
	}
	if err := writeParquetDay(path, rows); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(filepath.Dir(path), ".ftw-buckets-"+day.Format("02")+".pending.db")
	if err := os.WriteFile(scratch, []byte("scratch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.AbsorbColdHistory(ctx, cold); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unsummarized sample day removed: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("archive scratch kept: %v", err)
	}
	got, err := s.LoadSeries("meter", "grid_w", hour, hour+HistoryHourResolutionMS-1, 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("kept day unreadable: %v %v", got, err)
	}
	if err := s.summarizeParquetDay(ctx, path); err != nil {
		t.Fatal(err)
	}
	// A day that changed after its summary is not absorbed by that receipt.
	rows = append(rows, parquetSampleRow{TsMs: hour + 3000, Driver: "meter", Metric: "grid_w", Value: 20})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := writeParquetDay(path, rows); err != nil {
		t.Fatal(err)
	}
	if err := s.AbsorbColdHistory(ctx, cold); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("changed sample day removed: %v", err)
	}
	if err := s.summarizeParquetDay(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := s.AbsorbColdHistory(ctx, cold); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("summarized sample day kept: %v", err)
	}
	var n int64
	var sum float64
	if err := s.history.QueryRow(`SELECT h.n,h.sum_value FROM ts_series_hour h JOIN ts_drivers d ON d.id=h.driver_id JOIN ts_metrics m ON m.id=h.metric_id
 WHERE d.name='meter' AND m.name='grid_w' AND h.hour_ms=?`, hour).Scan(&n, &sum); err != nil || n != 3 || sum != 60 {
		t.Fatalf("hourly summary n=%d sum=%v err=%v", n, sum, err)
	}
}

func coldImportFixture(t *testing.T, width int64) (*Store, string, string, int64) {
	t.Helper()
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	hour := time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	base := hour.UnixMilli()
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "amps", TsMs: base + 1000, Value: 8}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := BucketSummary{StartMS: base, ResolutionMS: width, FirstMS: base + 1000, LastMS: base + 1000, N: 1, Sum: 8, Min: 8, Max: 8, Last: 8}
	if width == OldArchiveResolutionMS {
		b.LastMS, b.N, b.Sum, b.Max, b.Last = base+61000, 2, 40, 32, 32
	}
	cold := t.TempDir()
	path := filepath.Join(cold, hour.Format("2006/01/02.buckets.parquet"))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeTestBuckets(path, []metricBucket{{Driver: "meter", Metric: "amps", BucketSummary: b}}); err != nil {
		t.Fatal(err)
	}
	return s, cold, path, base
}

func assertColdImportIntact(t *testing.T, s *Store, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("archive not retained", err)
	}
	var fine, hours, receipts int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets WHERE resolution_ms<3600000`).Scan(&fine); err != nil {
		t.Fatal(err)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets WHERE resolution_ms=3600000`).Scan(&hours); err != nil {
		t.Fatal(err)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_migrations WHERE name LIKE 'plain-archive:%'`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if fine != 2 || hours != 0 || receipts != 0 {
		t.Fatalf("failed import changed SQLite: fine=%d hours=%d receipts=%d", fine, hours, receipts)
	}
}
