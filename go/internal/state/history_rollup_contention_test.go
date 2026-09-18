package state

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestDashboardRollupReadYieldsAndKeepsLateRows(t *testing.T) {
	reading, release := make(chan struct{}), make(chan struct{})
	var entered, released sync.Once
	defer released.Do(func() { close(release) })
	fn := fmt.Sprintf("test_rollup_read_%d", time.Now().UnixNano())
	if err := sqlite.RegisterScalarFunction(fn, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		entered.Do(func() { close(reading); <-release })
		return args[0], nil
	}); err != nil {
		t.Fatal(err)
	}
	s := freshStore(t)
	for _, q := range []string{
		`CREATE TABLE rollup_source AS SELECT * FROM history_hot`,
		`INSERT INTO rollup_source VALUES(1,100,0,0,0,0,'first')`,
		`CREATE VIEW slow_rollup AS SELECT ts_ms,` + fn + `(grid_w) AS grid_w,pv_w,bat_w,load_w,bat_soc,json FROM rollup_source`,
		`CREATE TRIGGER delete_slow_rollup INSTEAD OF DELETE ON slow_rollup BEGIN DELETE FROM rollup_source WHERE ts_ms=OLD.ts_ms; END`,
	} {
		if _, err := s.history.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.pruneChunk(ctx, "slow_rollup", "history_warm", 0, WarmBucketMS, WarmBucketMS)
		done <- err
	}()
	select {
	case <-reading:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The aggregation is suspended. Live telemetry must still commit.
	if err := s.EnqueueTelemetryTick(&HistoryPoint{TsMs: time.Now().UnixMilli(), GridW: 42, JSON: "{}"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	flush, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	err := s.FlushHistory(flush)
	stop()
	if err != nil {
		released.Do(func() { close(release) })
		<-done
		t.Fatal("rollup scan blocked live writes:", err)
	}
	if _, err := s.history.Exec(`INSERT INTO rollup_source VALUES(2,300,0,0,0,0,'last')`); err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var average float64
	var latest string
	if err := s.history.QueryRow(`SELECT grid_w,json FROM history_warm`).Scan(&average, &latest); err != nil {
		t.Fatal(err)
	}
	if average != 200 || latest != "last" {
		t.Fatalf("late source row lost: average=%v latest=%q", average, latest)
	}
}

func TestHistoryRollupLockWaitHonorsCancellation(t *testing.T) {
	for _, kind := range []string{"dashboard", "ledger"} {
		t.Run(kind, func(t *testing.T) {
			s := freshStore(t)
			s.historyWriteMu.Lock()
			defer s.historyWriteMu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if kind == "dashboard" {
					_, err = s.pruneChunk(ctx, "history_hot", "history_warm", 0, WarmBucketMS, WarmBucketMS)
				} else {
					_, err = s.rollupEnergyLedgerWidth(ctx, 0, EnergyLedgerRollupBucketMS, EnergyLedgerRollupBucketMS)
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled rollup still waits for the live writer mutex")
			}
		})
	}
}

func TestEnergyLedgerBatchedRollupPreservesExactTotals(t *testing.T) {
	s := freshStore(t)
	const width = EnergyLedgerRollupBucketMS
	for minute := int64(0); minute < 12; minute++ {
		for asset := 0; asset < 20; asset++ {
			insertLedgerEntryTest(t, s, fmt.Sprintf("meter-%d", asset), FlowGridImport, minute*EnergyLedgerBucketMS, EnergyLedgerBucketMS, 2, "hardware_counter", "measured", "counter", 1)
		}
	}
	n, err := s.rollupEnergyLedgerWidth(context.Background(), 0, width, width)
	if err != nil || n != 240 {
		t.Fatalf("rolled=%d err=%v", n, err)
	}
	var count, samples int64
	var energy float64
	if err := s.history.QueryRow(`SELECT COUNT(*),SUM(sample_count),SUM(energy_wh) FROM energy_ledger_entries WHERE bucket_len_ms=?`, width).Scan(&count, &samples, &energy); err != nil {
		t.Fatal(err)
	}
	if count != 20 || samples != 240 || energy != 480 {
		t.Fatalf("rollup lost totals: rows=%d samples=%d energy=%v", count, samples, energy)
	}
	if n, err := s.rollupEnergyLedgerWidth(context.Background(), 0, width, width); err != nil || n != 0 {
		t.Fatalf("rollup not idempotent: %d %v", n, err)
	}
}
