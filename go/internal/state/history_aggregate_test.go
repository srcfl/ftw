package state

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAggregateHistoryPreservesCountsEnvelopeAndLast(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute).UnixMilli()
	for i, v := range []float64{100, 100, 9100, 100} {
		p := HistoryPoint{TsMs: base + int64(i)*1000, GridW: v, JSON: `{"status":"charging"}`}
		if err := s.EnqueueTelemetryTick(&p, []Sample{{Driver: "ev", Metric: "power", TsMs: p.TsMs, Value: v}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.FlushHistory(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var raw, n int64
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&raw); err != nil || raw != 0 {
		t.Fatal("raw poll rows retained", raw, err)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets`).Scan(&n); err != nil || n != 2 {
		t.Fatal("expected one 10s and one minute summary", n, err)
	}
	rows, err := s.LoadSeriesBucketsOrRaw("ev", "power", base, base+60000, 0)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	p := rows[0]
	if p.N != 4 || p.Min != 100 || p.Max != 9100 || p.V != 2350 || p.Last == nil || *p.Last != 100 || p.ResolutionMS != 10000 || p.FirstMS != base || p.TsMs != base+3000 {
		t.Fatalf("lost evidence: %+v", p)
	}

}

func TestAggregateRetryRestartDuplicatesAndOutOfOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Truncate(time.Minute).UnixMilli()
	samples := []Sample{{Driver: "ev", Metric: "power", TsMs: base + 9000, Value: 900}, {Driver: "ev", Metric: "power", TsMs: base + 1000, Value: 100}, {Driver: "ev", Metric: "power", TsMs: base + 1000, Value: 999}}
	seq, err := s.recordHistoryBatch(context.Background(), "uncertain", "same", nil, samples, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	retry, err := s.recordHistoryBatch(context.Background(), "uncertain", "same", nil, samples, nil, 0)
	if err != nil || retry != seq {
		t.Fatal(retry, seq, err)
	}
	if _, err := s.recordHistoryBatch(context.Background(), "second", "different", nil, append(samples, Sample{Driver: "ev", Metric: "power", TsMs: base + 5000, Value: 500}), nil, 0); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadSeriesBucketsOrRaw("ev", "power", base, base+60000, 0)
	if err != nil || len(rows) != 1 || rows[0].N != 3 || rows[0].V != 500 || rows[0].Min != 100 || rows[0].Max != 900 {
		t.Fatal(rows, err)
	}
	latest, err := s.LatestSample("ev", "power")
	if err != nil || latest.Value != 900 || latest.TsMs != base+9000 {
		t.Fatal(latest, err)
	}
	var n int64
	if err := s.history.QueryRow(`SELECT SUM(n) FROM ts_series_hour`).Scan(&n); err != nil || n != 3 {
		t.Fatal(n, err)
	}
}

func TestAggregateCutoffWaitsForAcceptedTick(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute)
	s.historyWriteMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.historyWriteMu.Unlock()
		}
	}()
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "ev", Metric: "power", TsMs: base.UnixMilli() + 1000, Value: 500}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := s.reserveAggregateCutoff(base.Add(time.Hour).UnixMilli()); got != base.UnixMilli() {
		t.Fatal("archived a pending tick", got)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "ev", Metric: "power", TsMs: base.UnixMilli() - 1000, Value: 700}}, nil); err == nil {
		t.Fatal("accepted data behind archive boundary")
	}
	s.historyWriteMu.Unlock()
	locked = false
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.reserveAggregateCutoff(base.Add(time.Hour).UnixMilli()); got != base.Add(time.Hour).UnixMilli() {
		t.Fatal(got)
	}
	status := s.HistoryWriterStatus()
	if status.Committed != 1 || status.Rejected != 1 || status.Pending != 0 {
		t.Fatal(status)
	}
}

func TestAggregatePreservesOriginalEnergyObservations(t *testing.T) {
	raw, agg := freshStore(t), freshStore(t)
	if err := agg.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Hour).UnixMilli()
	asset := HardwareEnergyAssetID("maker:car", AssetVehicleCharger)
	for i, off := range []int64{0, 1000, 9000, 11000, 61000, 120000, 3600000, 3601000} {
		at := base + off
		observations := []EnergyObservation{ledgerObservation(asset, AssetVehicleCharger, FlowVehicleCharge, at, energyPtr(float64(i)*100), energyPtr(float64(i%2)*7000))}
		samples := []Sample{{Driver: "ev", Metric: "ev_w", TsMs: at, Value: float64(i%2) * 7000}}
		if err := raw.RecordTickWithOptionalHistory(nil, samples, observations); err != nil {
			t.Fatal(err)
		}
		if err := agg.EnqueueTelemetryTick(nil, samples, observations); err != nil {
			t.Fatal(err)
		}
	}
	if err := agg.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := loadLedgerTestPoints(t, raw, asset, base, base+7200000)
	got := loadLedgerTestPoints(t, agg, asset, base, base+7200000)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("energy changed through gauge aggregation:\ngot %+v\nwant %+v", got, want)
	}
}
