package state

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestCounterReturnAfterLongGapCommitsWholeTick(t *testing.T) {
	s := freshStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// An old hardware identity can return at startup. A legitimate meter outage
	// has the same shape: preserve the counter delta across every touched bucket.
	from := int64(1_800_000_000_000/EnergyLedgerBucketMS*EnergyLedgerBucketMS + 60_000)
	to := from + int64(24*time.Hour/time.Millisecond) + 60_000
	observations := []EnergyObservation{
		ledgerObservation("returning-battery", AssetBattery, FlowBatteryCharge, from, energyPtr(100), energyPtr(0)),
		ledgerObservation("returning-battery", AssetBattery, FlowBatteryDischarge, from, energyPtr(100), energyPtr(0)),
		ledgerObservation("returning-grid", AssetGridMeter, FlowGridImport, from, energyPtr(100), energyPtr(0)),
		ledgerObservation("returning-grid", AssetGridMeter, FlowGridExport, from, energyPtr(100), energyPtr(0)),
		ledgerObservation("returning-pv", AssetPV, FlowPVGeneration, from, energyPtr(100), energyPtr(0)),
	}
	baseline, err := s.recordHistoryBatch(ctx, "baseline", "baseline-hash", &HistoryPoint{TsMs: from}, nil, observations, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckpointHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`SET memory_limit='256MB'`); err != nil {
		t.Fatal(err)
	}
	for i := range observations {
		observations[i].AtMs, observations[i].CounterWh = to, energyPtr(7300)
	}
	samples := make([]Sample, 161)
	for i := range samples {
		samples[i] = Sample{TsMs: to, Driver: "returning-meter", Metric: fmt.Sprintf("metric-%d", i), Value: float64(i)}
	}
	point := &HistoryPoint{TsMs: to, JSON: "{}"}
	invalid := append(append([]EnergyObservation(nil), observations...), ledgerObservation("invalid", AssetBattery, FlowBatteryCharge, to, energyPtr(-1), nil))
	if _, err := s.recordHistoryBatch(ctx, "gap", "gap-hash", point, samples, invalid, baseline); err == nil {
		t.Fatal("invalid last observation committed a partial tick")
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM history_hot`:                                                     1,
		`SELECT COUNT(*) FROM ts_samples`:                                                      0,
		`SELECT COUNT(*) FROM energy_ledger_entries WHERE provenance='counter_gap'`:            0,
		`SELECT COUNT(*) FROM energy_ledger_cursors WHERE cursor_kind='counter' AND value=100`: 5,
		`SELECT COUNT(*) FROM history_receipts`:                                                1,
	} {
		var got int
		if err := s.history.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("rollback %s = %d, want %d: %v", query, got, want, err)
		}
	}
	sequence, err := s.recordHistoryBatch(ctx, "gap", "gap-hash", point, samples, observations, baseline)
	if err != nil {
		t.Fatalf("full tick after a day-long counter gap: %v", err)
	}
	// A lost acknowledgement must not apply the recovered energy a second time.
	if retry, err := s.recordHistoryBatch(ctx, "gap", "gap-hash", point, samples, observations, baseline); err != nil || retry != sequence {
		t.Fatalf("same-batch retry: sequence=%d err=%v", retry, err)
	}
	var total, first, last, counter float64
	var buckets, count, history, scalar, receipts, allBuckets, counters int
	if err := s.history.QueryRow(`SELECT COUNT(*), SUM(energy_wh), SUM(sample_count), arg_min(energy_wh,bucket_start_ms), arg_max(energy_wh,bucket_start_ms)
		FROM energy_ledger_entries WHERE provenance='counter_gap' AND asset_id='returning-battery' AND flow='battery_charge'`).Scan(&buckets, &total, &count, &first, &last); err != nil {
		t.Fatal(err)
	}
	if err := s.history.QueryRow(`SELECT value FROM energy_ledger_cursors WHERE asset_id='returning-battery' AND flow='battery_charge' AND cursor_kind='counter'`).Scan(&counter); err != nil {
		t.Fatal(err)
	}
	for query, dest := range map[string]*int{
		`SELECT COUNT(*) FROM history_hot`:                                                      &history,
		`SELECT COUNT(*) FROM ts_samples`:                                                       &scalar,
		`SELECT COUNT(*) FROM history_receipts`:                                                 &receipts,
		`SELECT COUNT(*) FROM energy_ledger_entries WHERE provenance='counter_gap'`:             &allBuckets,
		`SELECT COUNT(*) FROM energy_ledger_cursors WHERE cursor_kind='counter' AND value=7300`: &counters,
	} {
		if err := s.history.QueryRow(query).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	duration := float64(to - from)
	if buckets != 289 || allBuckets != 5*289 || counters != 5 || count != 289 || math.Abs(total-7200) > 1e-8 || math.Abs(first-7200*240_000/duration) > 1e-10 || math.Abs(last-7200*120_000/duration) > 1e-10 || counter != 7300 || history != 2 || scalar != 161 || receipts != 1 {
		t.Fatalf("gap result buckets=%d/%d counters=%d total=%v count=%d edge=%v/%v cursor=%v history=%d samples=%d receipts=%d", buckets, allBuckets, counters, total, count, first, last, counter, history, scalar, receipts)
	}
}
