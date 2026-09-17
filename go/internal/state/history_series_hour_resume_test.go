package state

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func unbuiltSeriesHours(t *testing.T, s *Store, hours int) {
	t.Helper()
	var samples []Sample
	for i := 0; i < hours; i++ {
		samples = append(samples, Sample{Driver: "meter", Metric: "pv_w", TsMs: int64(i) * seriesHourMs, Value: 1},
			Sample{Driver: "meter", Metric: "pv_w", TsMs: int64(i)*seriesHourMs + 1, Value: 3})
	}
	if err := s.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DELETE FROM ts_series_hour`, `DELETE FROM history_migrations WHERE name='ts-series-hour-v1'`} {
		if _, err := s.history.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeriesHourBackfillResumesAfterReopenAndLateWrite(t *testing.T) {
	s := freshStore(t)
	unbuiltSeriesHours(t, s, 4)
	if done, err := s.backfillSeriesHours(context.Background(), 1); err != nil || done {
		t.Fatalf("first batch: done=%v err=%v", done, err)
	}
	var rows, through int64
	if err := s.history.QueryRow(`SELECT rows_done,ts_ms FROM history_sqlite_progress WHERE source=?`, seriesHoursMigration).Scan(&rows, &through); err != nil || rows != 2 || through != seriesHourMs-1 {
		t.Fatalf("cursor: rows=%d through=%d err=%v", rows, through, err)
	}
	path := s.mainDBPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var oldReads atomic.Int64
	function := fmt.Sprintf("test_resume_value_%d", time.Now().UnixNano())
	if err := sqlite.RegisterScalarFunction(function, 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if args[0].(int64) < seriesHourMs {
			oldReads.Add(1)
		}
		return args[1], nil
	}); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A late sample behind the cursor must update its completed summary.
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "pv_w", TsMs: 2, Value: 8}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`ALTER TABLE ts_samples RENAME TO resume_source`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`CREATE VIEW ts_samples AS SELECT driver_id,metric_id,ts_ms,` + function + `(ts_ms,value) AS value FROM resume_source`); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureSeriesHours(context.Background()); err != nil {
		t.Fatal(err)
	}
	if oldReads.Load() != 0 {
		t.Fatalf("reread %d values behind durable cursor", oldReads.Load())
	}
	var n int64
	var sum float64
	if err := s.history.QueryRow(`SELECT SUM(n),SUM(sum_value) FROM ts_series_hour`).Scan(&n, &sum); err != nil || n != 9 || sum != 24 {
		t.Fatalf("rollup: n=%d sum=%v err=%v", n, sum, err)
	}
	if !s.seriesHoursReady() {
		t.Fatal("missing completion marker")
	}
}

func TestSeriesHourBackfillCursorAndSummaryCommitTogether(t *testing.T) {
	s := freshStore(t)
	unbuiltSeriesHours(t, s, 2)
	if _, err := s.history.Exec(`CREATE TRIGGER reject_hour_progress BEFORE INSERT ON history_sqlite_progress
		WHEN NEW.source='ts-series-hour-v1' BEGIN SELECT RAISE(ABORT,'test interrupted checkpoint'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backfillSeriesHours(context.Background(), 1); err == nil {
		t.Fatal("expected checkpoint failure")
	}
	for _, table := range []string{"ts_series_hour", "history_sqlite_progress"} {
		var n int
		if err := s.history.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("partial commit in %s: %d %v", table, n, err)
		}
	}
	if _, err := s.history.Exec(`DROP TRIGGER reject_hour_progress`); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureSeriesHours(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSeriesHourBackfillRetriesWithoutRestart(t *testing.T) {
	var attempts atomic.Int64
	function := fmt.Sprintf("test_retry_hour_value_%d", time.Now().UnixNano())
	if err := sqlite.RegisterScalarFunction(function, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("temporary read failure")
		}
		return args[0], nil
	}); err != nil {
		t.Fatal(err)
	}
	s := freshStore(t)
	unbuiltSeriesHours(t, s, 2)
	if _, err := s.history.Exec(`ALTER TABLE ts_samples RENAME TO retry_source`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`CREATE VIEW ts_samples AS SELECT driver_id,metric_id,ts_ms,` + function + `(value) AS value FROM retry_source`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.runSeriesHourBackfill(ctx, time.Millisecond)
	if !s.seriesHoursReady() {
		t.Fatal("background retry did not finish without a restart")
	}
}
