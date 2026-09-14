package state

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
)

// Run against a disposable COPY of a large history.db, never a live database:
// FTW_BACKFILL_TEST_DB=/path/to/copy.db go test ./internal/state -run TestSeriesHourBackfillDataset -v -timeout 30m
func TestSeriesHourBackfillDataset(t *testing.T) {
	path := os.Getenv("FTW_BACKFILL_TEST_DB")
	if path == "" {
		t.Skip("set FTW_BACKFILL_TEST_DB to a disposable history copy")
	}
	db, err := openDurableHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{history: db}
	for _, q := range []string{`DELETE FROM ts_series_hour`, `DELETE FROM history_migrations WHERE name='ts-series-hour-v1'`, `DELETE FROM history_sqlite_progress WHERE source='ts-series-hour-v1'`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	start := time.Now()
	for batch := 0; ; batch++ {
		done, err := s.backfillSeriesHours(ctx, 64)
		if err != nil {
			t.Fatal(err)
		}
		if batch%200 == 0 {
			t.Logf("batch=%d elapsed=%s status=%v", batch, time.Since(start).Round(time.Second), s.SeriesHourBackfillStatus())
		}
		if done {
			break
		}
		if err := pauseMaintenance(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ensureSeriesHours(ctx); err != nil {
		t.Fatal(err)
	}
	var rawN, hourN int64
	var rawSum, hourSum, rawMin, hourMin, rawMax, hourMax float64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),SUM(value),MIN(value),MAX(value) FROM ts_samples`).Scan(&rawN, &rawSum, &rawMin, &rawMax); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT SUM(n),SUM(sum_value),MIN(min_value),MAX(max_value) FROM ts_series_hour`).Scan(&hourN, &hourSum, &hourMin, &hourMax); err != nil {
		t.Fatal(err)
	}
	if rawN != hourN || rawMin != hourMin || rawMax != hourMax || math.Abs(rawSum-hourSum) > 1e-8*math.Max(1, math.Abs(rawSum)) {
		t.Fatalf("raw/summary mismatch: n=%d/%d sum=%g/%g min=%g/%g max=%g/%g", rawN, hourN, rawSum, hourSum, rawMin, hourMin, rawMax, hourMax)
	}
	t.Logf("verified rows=%d elapsed=%s ready=%v", rawN, time.Since(start).Round(time.Second), s.seriesHoursReady())
}
