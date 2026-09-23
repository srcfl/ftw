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
