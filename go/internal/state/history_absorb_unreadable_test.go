package state

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func unreadableBucket(at time.Time, v float64) metricBucket {
	return metricBucket{Driver: "meter", Metric: "grid_w", BucketSummary: BucketSummary{StartMS: at.UnixMilli(), ResolutionMS: ArchiveResolutionMS, FirstMS: at.UnixMilli(), LastMS: at.UnixMilli() + 1000, N: 1, Sum: v, Min: v, Max: v, Last: v}}
}

// unreadableColdFixture opens a store the way main.go does, with the same
// cold folder for reads and for the boot import, so runtime readers see
// whatever the import leaves behind.
func unreadableColdFixture(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	cold := t.TempDir()
	s.coldDir = cold
	return s, cold, time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(24 * time.Hour)
}

func dayFile(t *testing.T, cold string, day time.Time, suffix string) string {
	t.Helper()
	path := filepath.Join(cold, day.Format("2006/01"), day.Format("02")+suffix)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertFile(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	if want && err != nil {
		t.Fatalf("%s must stay: %v", path, err)
	}
	if !want && !os.IsNotExist(err) {
		t.Fatalf("%s must be gone: %v", path, err)
	}
}

func listedUnreadable(s *Store) []string {
	listed, _ := s.HistoryBackend()["unreadable_cold_files"].([]string)
	return listed
}

// One damaged bucket day must not stop boot-time absorb of the other days.
// main.go exits on an AbsorbColdHistory error, and the file would stay, so
// every restart would fail the same way. The kept file must also leave the
// day's history reads working.
func TestAbsorbColdHistorySkipsUnreadableBucketFile(t *testing.T) {
	damage := map[string]func(t *testing.T, path string){
		"truncated": func(t *testing.T, path string) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, info.Size()/2); err != nil {
				t.Fatal(err)
			}
		},
		"no read permission": func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads files without permission")
			}
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0o644); _ = os.Chmod(path+unreadableColdSuffix, 0o644) })
		},
	}
	for name, spoil := range damage {
		t.Run(name, func(t *testing.T) {
			s, cold, good := unreadableColdFixture(t)
			bad := good.Add(24 * time.Hour)
			hour := good.Add(3 * time.Hour)
			badHour := bad.Add(5 * time.Hour)
			goodPath := dayFile(t, cold, good, ".buckets.parquet")
			badPath := dayFile(t, cold, bad, ".buckets.parquet")
			legacyPath := dayFile(t, cold, bad, ".legacy-buckets.parquet")
			if err := writeTestBuckets(goodPath, []metricBucket{unreadableBucket(hour, 10)}); err != nil {
				t.Fatal(err)
			}
			if err := writeTestBuckets(badPath, []metricBucket{unreadableBucket(bad, 5)}); err != nil {
				t.Fatal(err)
			}
			spoil(t, badPath)
			// The older copy of the damaged day is readable and serves it.
			if err := writeTestBuckets(legacyPath, []metricBucket{unreadableBucket(badHour, 7)}); err != nil {
				t.Fatal(err)
			}

			for boot := 1; boot <= 2; boot++ {
				if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
					t.Fatalf("boot %d: AbsorbColdHistory: %v", boot, err)
				}
				assertFile(t, goodPath, false)
				assertFile(t, badPath, false)
				assertFile(t, badPath+unreadableColdSuffix, true)
				assertFile(t, legacyPath, true)
				if want := bad.Format("2006/01/02") + ".buckets.parquet" + unreadableColdSuffix; !slices.Equal(listedUnreadable(s), []string{want}) {
					t.Fatalf("boot %d: unreadable_cold_files = %v, want [%s]", boot, listedUnreadable(s), want)
				}
				for _, want := range []struct {
					at time.Time
					v  float64
				}{{hour, 10}, {badHour, 7}} {
					points, err := s.LoadSeriesBucketsOrRaw("meter", "grid_w", want.at.UnixMilli(), want.at.UnixMilli()+HistoryHourResolutionMS-1, 0)
					if err != nil || len(points) != 1 || points[0].V != want.v {
						t.Fatalf("boot %d: hour %s = %+v, %v; want one point of %v", boot, want.at, points, err, want.v)
					}
				}
				from, to := good.UnixMilli(), bad.Add(48*time.Hour).UnixMilli()
				for _, maxPoints := range []int{0, 200} {
					for _, metric := range []string{"grid_w", "other_w"} {
						if _, err := s.LoadSeriesBucketsOrRaw("meter", metric, from, to, maxPoints); err != nil {
							t.Fatalf("boot %d: %s range over the damaged day, maxPoints=%d: %v", boot, metric, maxPoints, err)
						}
					}
				}
				now := time.Now()
				if _, err := s.LoadSeriesBuckets("meter", "grid_w", now.Add(-365*24*time.Hour).UnixMilli(), now.UnixMilli(), 365); err != nil {
					t.Fatalf("boot %d: year chart: %v", boot, err)
				}
			}
		})
	}
}

// An operator who puts back a good copy of the damaged file must get a
// clean boot. Had the legacy copy imported the same hour, the restored file
// would conflict with it and stop boot again.
func TestAbsorbColdHistoryImportsRestoredBucketFile(t *testing.T) {
	s, cold, day := unreadableColdFixture(t)
	at := day.Add(2 * time.Hour)
	orig := dayFile(t, cold, day, ".buckets.parquet")
	legacy := dayFile(t, cold, day, ".legacy-buckets.parquet")
	full := []metricBucket{unreadableBucket(at, 3), unreadableBucket(at.Add(time.Minute), 4)}
	if err := writeTestBuckets(orig, full); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(orig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(orig, int64(len(good)/2)); err != nil {
		t.Fatal(err)
	}
	if err := writeTestBuckets(legacy, full[:1]); err != nil {
		t.Fatal(err)
	}
	if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if err := os.Remove(orig + unreadableColdSuffix); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orig, good, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
		t.Fatalf("boot after restoring the file: %v", err)
	}
	assertFile(t, orig, false)
	assertFile(t, legacy, false)
	if listed := listedUnreadable(s); len(listed) != 0 {
		t.Fatalf("unreadable_cold_files = %v after recovery", listed)
	}
	points, err := s.LoadSeriesBucketsOrRaw("meter", "grid_w", at.UnixMilli(), at.UnixMilli()+HistoryHourResolutionMS-1, 0)
	if err != nil || len(points) != 1 || points[0].N != 2 {
		t.Fatalf("restored hour = %+v, %v; want one point from both minutes", points, err)
	}
}
