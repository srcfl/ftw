package state

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// One damaged bucket day must not stop boot-time absorb of the other days.
// main.go exits on an AbsorbColdHistory error, and the file would stay, so
// every restart would fail the same way.
func TestAbsorbColdHistorySkipsUnreadableBucketFile(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	good := time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(24 * time.Hour)
	bad := good.Add(24 * time.Hour)
	hour := good.Add(3 * time.Hour)
	badHour := bad.Add(5 * time.Hour)
	cold := t.TempDir()
	goodPath := filepath.Join(cold, good.Format("2006/01"), good.Format("02")+".buckets.parquet")
	badPath := filepath.Join(cold, bad.Format("2006/01"), bad.Format("02")+".buckets.parquet")
	legacyPath := filepath.Join(cold, bad.Format("2006/01"), bad.Format("02")+".legacy-buckets.parquet")
	for _, p := range []string{goodPath, badPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bucket := func(at time.Time, v float64) []metricBucket {
		return []metricBucket{{Driver: "meter", Metric: "grid_w", BucketSummary: BucketSummary{StartMS: at.UnixMilli(), ResolutionMS: ArchiveResolutionMS, FirstMS: at.UnixMilli(), LastMS: at.UnixMilli() + 1000, N: 1, Sum: v, Min: v, Max: v, Last: v}}}
	}
	if err := writeTestBuckets(goodPath, bucket(hour, 10)); err != nil {
		t.Fatal(err)
	}
	// A truncated copy: write a valid file, then cut its footer off.
	if err := writeTestBuckets(badPath, bucket(bad, 5)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(badPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(badPath, info.Size()/2); err != nil {
		t.Fatal(err)
	}
	// The older copy of the damaged day is readable and takes its place.
	if err := writeTestBuckets(legacyPath, bucket(badHour, 7)); err != nil {
		t.Fatal(err)
	}

	if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
		t.Fatalf("AbsorbColdHistory: %v", err)
	}
	for _, want := range []struct {
		at time.Time
		v  float64
	}{{hour, 10}, {badHour, 7}} {
		points, err := s.LoadSeriesBucketsOrRaw("meter", "grid_w", want.at.UnixMilli(), want.at.UnixMilli()+HistoryHourResolutionMS-1, 0)
		if err != nil || len(points) != 1 || points[0].V != want.v {
			t.Fatalf("hour %s = %+v, %v; want one point of %v", want.at, points, err, want.v)
		}
	}
	if _, err := os.Stat(badPath); err != nil {
		t.Fatalf("damaged file must be kept for recovery: %v", err)
	}
	for _, p := range []string{goodPath, legacyPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed after import: %v", p, err)
		}
	}
	listed, _ := s.HistoryBackend()["unreadable_cold_files"].([]string)
	if want := bad.Format("2006/01/02") + ".buckets.parquet"; !slices.Equal(listed, []string{want}) {
		t.Fatalf("unreadable_cold_files = %v, want [%s]", listed, want)
	}
	// The next boot meets the same file and still starts.
	if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
		t.Fatalf("second AbsorbColdHistory: %v", err)
	}
}
