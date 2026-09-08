package state

import (
	"context"
	"testing"
	"time"
)

func TestLiveWriterRecoversWholeTickAfterOutOfMemory(t *testing.T) {
	s := freshStore(t)
	base := int64(1_800_000_000_000 / EnergyLedgerBucketMS * EnergyLedgerBucketMS)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	put := func(at int64, counter float64) {
		t.Helper()
		p := HistoryPoint{TsMs: at, JSON: "{}"}
		samples := []Sample{{TsMs: at, Driver: "meter", Metric: "power", Value: counter}}
		obs := []EnergyObservation{ledgerObservation("test-charger", AssetVehicleCharger, FlowVehicleCharge, at, energyPtr(counter), energyPtr(1200))}
		if err := s.EnqueueTelemetryTick(&p, samples, obs); err != nil {
			t.Fatal(err)
		}
	}
	put(base, 100)
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckpointHistory(ctx); err != nil {
		t.Fatal(err)
	}
	// A real DuckDB allocation failure exercises rollback and the writer's
	// retained batch. Rotation reopens with the configured normal budget.
	if _, err := s.history.Exec(`SET memory_limit='2MB'`); err != nil {
		t.Fatal(err)
	}
	put(base+60_000, 120)
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatalf("writer did not recover: %+v: %v", s.HistoryWriterStatus(), err)
	}
	st := s.HistoryWriterStatus()
	if st.Accepted != 2 || st.Committed != 2 || st.Pending != 0 || st.Rejected != 0 || st.LastError != "" || st.MaintenanceError != "" || st.MaintenanceRuns == 0 {
		t.Fatalf("OOM recovery status=%+v", st)
	}
	var historyRows, sampleRows int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&historyRows); err != nil {
		t.Fatal(err)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&sampleRows); err != nil {
		t.Fatal(err)
	}
	var energy, counter float64
	if err := s.history.QueryRow(`SELECT SUM(energy_wh) FROM energy_ledger_entries WHERE asset_id='test-charger'`).Scan(&energy); err != nil {
		t.Fatal(err)
	}
	if err := s.history.QueryRow(`SELECT value FROM energy_ledger_cursors WHERE asset_id='test-charger' AND cursor_kind='counter'`).Scan(&counter); err != nil {
		t.Fatal(err)
	}
	if historyRows != 2 || sampleRows != 2 || energy != 20 || counter != 120 {
		t.Fatalf("partial or duplicate tick: history=%d samples=%d energy=%v counter=%v", historyRows, sampleRows, energy, counter)
	}
}
