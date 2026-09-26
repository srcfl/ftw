package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// One unreadable cold sample day must not hold back the days after it, or
// the hourly rollup retries every few seconds and never completes.
func TestEnsureParquetHoursSkipsAnUnreadableDay(t *testing.T) {
	s := freshStore(t)
	s.coldDir = t.TempDir()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-96 * time.Hour)
	bad := filepath.Join(s.coldDir, day.Format("2006/01/02.parquet"))
	good := filepath.Join(s.coldDir, day.Add(24*time.Hour).Format("2006/01/02.parquet"))
	for _, p := range []string{bad, good} {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(bad, []byte("not a parquet file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeParquetDay(good, []parquetSampleRow{{Driver: "meter", Metric: "power", TsMs: day.Add(24*time.Hour).UnixMilli() + 1, Value: 42}}); err != nil {
		t.Fatal(err)
	}

	if err := s.ensureParquetHours(context.Background()); err != nil {
		t.Fatalf("an unreadable day stopped the rollup: %v", err)
	}
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_archive_days WHERE path = ?`, parquetSummaryKey(good)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the readable day after it was not summarized: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatalf("the unreadable day was not kept: %v", err)
	}
}
