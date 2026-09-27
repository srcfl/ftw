package state

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestHistoryRollupLockWaitHonorsCancellation(t *testing.T) {
	s := freshStore(t)
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.rollupEnergyLedgerWidth(ctx, 0, EnergyLedgerRollupBucketMS, EnergyLedgerRollupBucketMS)
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
