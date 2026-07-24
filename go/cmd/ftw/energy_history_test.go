package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestBuildEnergyObservationsUsesStableAssetsAndDirectionalCounters(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	deviceID, err := st.RegisterDevice(state.Device{
		DriverName: "mutable-meter-name", Make: "Meter Maker", Serial: "SN-42",
	})
	if err != nil {
		t.Fatal(err)
	}
	evDeviceID, err := st.RegisterDevice(state.Device{
		DriverName: "mutable-ev-name", Make: "Charger Maker", Serial: "EV-42",
	})
	if err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("mutable-meter-name")
	tel.EnsureDriverHealth("mutable-ev-name")
	tel.Update("mutable-meter-name", telemetry.DerMeter, 500, nil,
		json.RawMessage(`{"import_wh":1200,"export_wh":340}`))
	tel.Update("mutable-ev-name", telemetry.DerEV, 3200, nil,
		json.RawMessage(`{"session_wh":800}`))
	ctrl := &control.State{SiteMeterDriver: "mutable-meter-name"}
	observations := buildEnergyObservations(st, tel, ctrl, state.HistoryPoint{LoadW: 450})

	wantAssetID := state.HardwareEnergyAssetID(deviceID, state.AssetGridMeter)
	wantEVAssetID := state.HardwareEnergyAssetID(evDeviceID, state.AssetVehicleCharger)
	var sawImport, sawExport, sawConsumer, sawVehicleCharge bool
	for _, o := range observations {
		switch o.Flow {
		case state.FlowGridImport:
			sawImport = o.AssetID == wantAssetID && o.CounterWh != nil && *o.CounterWh == 1200 && o.PowerW != nil && *o.PowerW == 500
		case state.FlowGridExport:
			sawExport = o.AssetID == wantAssetID && o.CounterWh != nil && *o.CounterWh == 340 && o.PowerW != nil && *o.PowerW == 0
		case state.FlowConsumerUse:
			if o.AssetKind == state.AssetObservedConsumer {
				sawConsumer = o.AssetID == observedConsumerAssetID && o.ReadOnly && o.DeviceID == ""
			} else {
				t.Fatalf("consumer_use must be reserved for observed household load: %+v", o)
			}
		case state.FlowVehicleCharge:
			sawVehicleCharge = o.AssetID == wantEVAssetID && o.CounterWh != nil && *o.CounterWh == 800
		}
	}
	if !sawImport || !sawExport || !sawConsumer || !sawVehicleCharge {
		t.Fatalf("observations missing stable directional assets: %+v", observations)
	}
}

func TestBuildHistoryPointExcludesUnavailableTelemetry(t *testing.T) {
	tel := telemetry.NewStore()
	for _, name := range []string{"site-meter", "stale-pv", "live-pv", "stale-battery", "live-battery"} {
		tel.EnsureDriverHealth(name)
	}
	tel.Update("site-meter", telemetry.DerMeter, 1200, nil, nil)
	tel.Update("stale-pv", telemetry.DerPV, -800, nil, nil)
	tel.Update("live-pv", telemetry.DerPV, -300, nil, nil)
	tel.Update("stale-battery", telemetry.DerBattery, 250, nil, nil)
	tel.Update("live-battery", telemetry.DerBattery, 50, nil, nil)
	now := time.Now()
	tel.DriverHealthMut("site-meter").SetOffline()
	tel.Get("stale-pv", telemetry.DerPV).UpdatedAt = now.Add(-2 * time.Minute)
	tel.DriverHealthMut("stale-battery").SetOffline()

	point, available := buildHistoryPoint(tel, &control.State{SiteMeterDriver: "site-meter"},
		now.UnixMilli(), time.Minute)
	if available {
		t.Fatalf("offline site meter produced history: %+v", point)
	}

	tel.Update("site-meter", telemetry.DerMeter, 800, nil, nil)
	tel.RecordDriverSuccess("site-meter")
	point, available = buildHistoryPoint(tel, &control.State{SiteMeterDriver: "site-meter"},
		now.UnixMilli(), time.Minute)
	if !available {
		t.Fatalf("recovered site meter unavailable: %+v", point)
	}

	livePV := tel.Get("live-pv", telemetry.DerPV).SmoothedW
	liveBattery := tel.Get("live-battery", telemetry.DerBattery).SmoothedW
	if point.PVW != livePV || point.BatW != liveBattery {
		t.Errorf("recovered history point includes stale DER telemetry: %+v", point)
	}

	var detail struct {
		Drivers map[string]map[string]float64 `json:"drivers"`
	}
	if err := json.Unmarshal([]byte(point.JSON), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Drivers["stale-pv"]) != 0 ||
		len(detail.Drivers["stale-battery"]) != 0 {
		t.Fatalf("history JSON retained stale driver values: %+v", detail.Drivers)
	}
	if detail.Drivers["live-pv"]["pv_w"] != livePV ||
		detail.Drivers["live-battery"]["bat_w"] != liveBattery {
		t.Fatalf("history JSON lost live driver values: %+v", detail.Drivers)
	}

	zeroTel := telemetry.NewStore()
	zeroTel.EnsureDriverHealth("zero-meter")
	zeroTel.Update("zero-meter", telemetry.DerMeter, 0, nil, nil)
	zeroTel.RecordDriverSuccess("zero-meter")
	zero, zeroAvailable := buildHistoryPoint(zeroTel,
		&control.State{SiteMeterDriver: "zero-meter"}, time.Now().UnixMilli(), time.Minute)
	if !zeroAvailable || zero.GridW != 0 {
		t.Fatalf("fresh 0 W site meter unavailable: point=%+v available=%v", zero, zeroAvailable)
	}
}

func TestBuildHistoryPointExcludesAgedEVAndV2XFromTotals(t *testing.T) {
	tel := telemetry.NewStore()
	for _, name := range []string{"site-meter", "stale-ev", "live-ev", "stale-v2x", "live-v2x"} {
		tel.EnsureDriverHealth(name)
	}
	now := time.Now()
	tel.Update("site-meter", telemetry.DerMeter, 5000, nil, nil)
	tel.RecordDriverSuccess("site-meter")
	tel.Update("stale-ev", telemetry.DerEV, 1000, nil, nil)
	tel.Update("live-ev", telemetry.DerEV, 600, nil, nil)
	tel.Update("stale-v2x", telemetry.DerV2X, -500, nil, nil)
	tel.Update("live-v2x", telemetry.DerV2X, 300, nil, nil)
	tel.Get("stale-ev", telemetry.DerEV).UpdatedAt = now.Add(-2 * time.Minute)
	tel.Get("stale-v2x", telemetry.DerV2X).UpdatedAt = now.Add(-2 * time.Minute)

	// Other live metrics keep each driver healthy while its EV/V2X reading
	// ages out. History must apply freshness per reading, not per driver.
	tel.EmitMetric("stale-ev", "temperature_c", 20, "C", "", "")
	tel.EmitMetric("stale-v2x", "temperature_c", 21, "C", "", "")
	for _, name := range []string{"stale-ev", "live-ev", "stale-v2x", "live-v2x"} {
		tel.RecordDriverSuccess(name)
	}

	point, available := buildHistoryPoint(tel, &control.State{SiteMeterDriver: "site-meter"},
		now.UnixMilli(), time.Minute)
	if !available {
		t.Fatal("fresh site meter did not produce history")
	}
	if point.LoadW != 4100 {
		t.Fatalf("load includes aged EV/V2X readings: got %v W, want 4100 W", point.LoadW)
	}

	var detail struct {
		Drivers    map[string]map[string]float64 `json:"drivers"`
		EVW        float64                       `json:"ev_w"`
		V2XW       float64                       `json:"v2x_w"`
		LoadHouseW float64                       `json:"load_house_w"`
	}
	if err := json.Unmarshal([]byte(point.JSON), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.EVW != 600 || detail.V2XW != 300 || detail.LoadHouseW != point.LoadW {
		t.Fatalf("top-level history disagrees with fresh readings: %+v", detail)
	}
	if len(detail.Drivers["stale-ev"]) != 0 || len(detail.Drivers["stale-v2x"]) != 0 {
		t.Fatalf("per-driver history retained aged readings: %+v", detail.Drivers)
	}
	if detail.Drivers["live-ev"]["ev_w"] != detail.EVW ||
		detail.Drivers["live-v2x"]["v2x_w"] != detail.V2XW {
		t.Fatalf("top-level and per-driver history disagree: %+v", detail)
	}
}

func TestStaleMeterTickKeepsSamplesAndIndependentLedgerWithoutDispatch(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pvDeviceID, err := st.RegisterDevice(state.Device{
		DriverName: "solar", Make: "PV Maker", Serial: "PV-42",
	})
	if err != nil {
		t.Fatal(err)
	}

	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("meter")
	tel.EnsureDriverHealth("solar")
	now := time.Now().Truncate(time.Second)
	base := now.Add(-5 * time.Minute)
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	tel.Get("meter", telemetry.DerMeter).UpdatedAt = base
	tel.RecordDriverSuccess("meter")
	tel.Update("solar", telemetry.DerPV, -400, nil, json.RawMessage(`{"generation_wh":100}`))
	tel.Get("solar", telemetry.DerPV).UpdatedAt = now.Add(-30 * time.Second)
	tel.RecordDriverSuccess("solar")

	ctrl := &control.State{
		SiteMeterDriver: "meter",
		LastTargets:     []control.DispatchTarget{{Driver: "battery", TargetW: 321}},
	}
	freshness := evaluateSiteDispatchFreshnessAt(tel, "meter", 16, 3, time.Minute, now)
	if freshness.Allowed() || freshness.Reason != siteDispatchMeterStale {
		t.Fatalf("stale meter dispatch decision = %+v", freshness)
	}
	if _, err := persistTelemetryTick(st, tel, ctrl, now.UnixMilli(), time.Minute); err != nil {
		t.Fatal(err)
	}

	tel.Update("solar", telemetry.DerPV, -400, nil, json.RawMessage(`{"generation_wh":110}`))
	tel.Get("solar", telemetry.DerPV).UpdatedAt = now
	tel.RecordDriverSuccess("solar")
	if _, err := persistTelemetryTick(st, tel, ctrl, now.Add(time.Second).UnixMilli(), time.Minute); err != nil {
		t.Fatal(err)
	}

	history, err := st.LoadHistory(base.UnixMilli(), now.Add(time.Second).UnixMilli(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("stale meter wrote legacy history: %+v", history)
	}
	samples, err := st.LoadSeries("solar", "pv_w", 0, time.Now().Add(time.Second).UnixMilli(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) == 0 {
		t.Fatal("stale meter dropped PV samples")
	}

	pvAssetID := state.HardwareEnergyAssetID(pvDeviceID, state.AssetPV)
	firstPVAt := now.Add(-30 * time.Second).UnixMilli()
	bucketStart := (firstPVAt / state.EnergyLedgerBucketMS) * state.EnergyLedgerBucketMS
	points, _, err := st.LoadEnergyHistory(state.EnergyHistoryQuery{
		AssetID: pvAssetID, SinceMS: bucketStart,
		UntilMS:  bucketStart + 2*state.EnergyLedgerBucketMS,
		BucketMS: state.EnergyLedgerBucketMS, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	var generatedWh float64
	for _, point := range points {
		if point.Flow == state.FlowPVGeneration {
			generatedWh += point.EnergyWh
		}
	}
	if generatedWh != 10 {
		t.Fatalf("stale meter independent PV ledger = %v Wh, want 10", generatedWh)
	}
	if len(ctrl.LastTargets) != 1 || ctrl.LastTargets[0].Driver != "battery" ||
		ctrl.LastTargets[0].TargetW != 321 {
		t.Fatalf("stale persistence changed dispatch targets: %+v", ctrl.LastTargets)
	}
}
