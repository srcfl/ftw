package state

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestSeriesHourBackfillReadDoesNotOwnSQLiteWriter(t *testing.T) {
	reading := make(chan struct{})
	release := make(chan struct{})
	var entered, released sync.Once
	defer released.Do(func() { close(release) })
	function := fmt.Sprintf("test_hour_backfill_slow_value_%d", time.Now().UnixNano())
	if err := sqlite.RegisterScalarFunction(function, 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			entered.Do(func() { close(reading) })
			<-release
			return args[0], nil
		}); err != nil {
		t.Fatal(err)
	}
	s := freshStore(t)
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "pv_w", TsMs: seriesHourMs, Value: 1200}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM history_migrations WHERE name='ts-series-hour-v1'`,
		`DELETE FROM ts_series_hour`,
		`ALTER TABLE ts_samples RENAME TO backfill_source`,
		`CREATE VIEW ts_samples AS SELECT driver_id,metric_id,ts_ms,` + function + `(value) AS value FROM backfill_source`,
	} {
		if _, err := s.history.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.ensureSeriesHours(ctx) }()
	select {
	case <-reading:
	case err := <-done:
		t.Fatalf("backfill ended before source read: %v", err)
	case <-ctx.Done():
		t.Fatal("backfill did not read source")
	}
	conn, err := s.history.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=100`); err != nil {
		t.Fatal(err)
	}
	_, writeErr := conn.ExecContext(ctx, `UPDATE backfill_source SET value=2400`)
	conn.Close()
	released.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if writeErr != nil {
		t.Fatalf("source aggregation held the SQLite writer: %v", writeErr)
	}
	var n int
	var sum float64
	if err := s.history.QueryRow(`SELECT n,sum_value FROM ts_series_hour`).Scan(&n, &sum); err != nil || n != 1 || sum != 2400 {
		t.Fatalf("backfill result: n=%d sum=%v err=%v", n, sum, err)
	}
}

func TestSeriesHourBackfillDoesNotPauseForEmptyGap(t *testing.T) {
	s := freshStore(t)
	if err := s.RecordSamples([]Sample{
		{Driver: "meter", Metric: "pv_w", TsMs: 0, Value: 1},
		{Driver: "meter", Metric: "pv_w", TsMs: 96 * seriesHourMs, Value: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`DELETE FROM ts_series_hour`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`DELETE FROM history_migrations WHERE name=?`, seriesHoursMigration); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.ensureSeriesHours(ctx); err != nil {
		t.Fatalf("sparse history did not finish within the maintenance budget: %v", err)
	}
	if !s.seriesHoursReady() {
		t.Fatal("completed sparse backfill lacks its completion marker")
	}
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_series_hour`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("sparse summaries: count=%d err=%v", n, err)
	}
}
