package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sync"
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
	ctrl := tickPersistControl{SiteMeterDriver: "mutable-meter-name"}
	observations := buildEnergyObservations(st, tel, ctrl, state.HistoryPoint{LoadW: 450}, testEnergyIdentity(st))

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

	point, available := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "site-meter"},
		now.UnixMilli(), time.Minute)
	if !available || point.Missing&state.MissingGrid == 0 {
		t.Fatalf("offline site meter became known or lost live devices: %+v", point)
	}

	tel.Update("site-meter", telemetry.DerMeter, 800, nil, nil)
	tel.RecordDriverSuccess("site-meter")
	point, available = buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "site-meter"},
		now.UnixMilli(), time.Minute)
	if !available || point.Missing&(state.MissingPV|state.MissingBattery|state.MissingLoad) == 0 {
		t.Fatalf("missing PV/battery became known or lost live meter: %+v", point)
	}
	// The aggregate becomes known only after every significant flow recovers.
	tel.Update("stale-pv", telemetry.DerPV, -800, nil, nil)
	tel.RecordDriverSuccess("stale-pv")
	tel.Update("stale-battery", telemetry.DerBattery, 250, nil, nil)
	tel.RecordDriverSuccess("stale-battery")
	point, available = buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "site-meter"},
		time.Now().Add(time.Millisecond).UnixMilli(), time.Minute)
	if !available || point.LoadW != 1600 || point.PVW != -1100 || point.BatW != 300 {
		t.Fatalf("recovered complete raw balance = %+v, available=%v", point, available)
	}

	zeroTel := telemetry.NewStore()
	zeroTel.EnsureDriverHealth("zero-meter")
	zeroTel.Update("zero-meter", telemetry.DerMeter, 0, nil, nil)
	zeroTel.RecordDriverSuccess("zero-meter")
	zero, zeroAvailable := buildHistoryPoint(zeroTel,
		tickPersistControl{SiteMeterDriver: "zero-meter"}, time.Now().Add(time.Millisecond).UnixMilli(), time.Minute)
	if !zeroAvailable || zero.GridW != 0 {
		t.Fatalf("fresh 0 W site meter unavailable: point=%+v available=%v", zero, zeroAvailable)
	}
}

func TestBuildHistoryPointRequiresFreshEVAndV2X(t *testing.T) {
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

	point, available := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "site-meter"},
		now.UnixMilli(), time.Minute)
	if !available || point.Missing&(state.MissingEV|state.MissingV2X|state.MissingLoad) == 0 {
		t.Fatalf("aged EV/V2X became known or lost live meter: %+v", point)
	}

}

func TestPersistTelemetryTickUsesPersistenceFreshness(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sampleAge time.Duration
		wantSaved bool
	}{
		{"poll after tick start", 0, true},
		{"stale reading", -2 * time.Minute, false},
		{"future reading", time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.EnableHistoryAggregation(); err != nil {
				t.Fatal(err)
			}
			tickMS := time.Now().Add(-5 * time.Second).UnixMilli()
			tel := telemetry.NewStore()
			tel.EnsureDriverHealth("meter")
			tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
			tel.RecordDriverSuccess("meter")
			sampleAt := time.Now().Add(tc.sampleAge)
			tel.Get("meter", telemetry.DerMeter).UpdatedAt = sampleAt
			ctrl := tickPersistControl{SiteMeterDriver: "meter"}
			if _, err := persistTelemetryTick(st, tel, ctrl, tickMS, time.Minute, testEnergyIdentity(st)); err != nil {
				t.Fatal(err)
			}
			if err := st.FlushHistory(context.Background()); err != nil {
				t.Fatal(err)
			}
			history, err := st.LoadHistory(tickMS-1, tickMS+1, 0)
			if err != nil {
				t.Fatal(err)
			}
			if (len(history) == 1) != tc.wantSaved {
				t.Fatalf("history = %+v, want saved=%v", history, tc.wantSaved)
			}
			if tc.wantSaved && (history[0].TsMs != tickMS || history[0].GridW != 1200 || history[0].Missing&state.MissingLoad == 0) {
				t.Fatal("fresh grid receipt lost or instantaneous Load entered history")
			}
			if tc.wantSaved {
				p, _ := buildHistoryPoint(tel, ctrl, tickMS, time.Minute)
				if p.Sources.AtMS < sampleAt.UnixMilli() || p.Sources.Missing != 0 {
					t.Fatal("fresh source snapshot predates receipt or became unknown")
				}
			}
			assets, err := st.EnergyAssets()
			if err != nil {
				t.Fatal(err)
			}
			consumerListed := false
			for _, asset := range assets {
				if asset.AssetID == observedConsumerAssetID {
					consumerListed = true
					if asset.Kind != state.AssetObservedConsumer || !asset.ReadOnly || asset.LastSeenMS < sampleAt.UnixMilli() {
						t.Fatal("consumer metadata lost its kind, read-only status or snapshot time")
					}
				}
			}
			if consumerListed != tc.wantSaved {
				t.Fatalf("consumer listed=%v, want %v", consumerListed, tc.wantSaved)
			}
			consumer, _, err := st.LoadEnergyHistory(state.EnergyHistoryQuery{
				AssetID: observedConsumerAssetID, SinceMS: tickMS - 1,
				UntilMS: time.Now().Add(5 * time.Minute).UnixMilli(), BucketMS: state.EnergyLedgerBucketMS, Limit: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(consumer) != 0 {
				t.Fatal("single instantaneous reading entered consumer interval history")
			}
		})
	}
}

// A config reload rewrites ctrl.SiteMeterDriver under ctrlMu from an HTTP
// goroutine while the tick persists. Persistence works from a copy taken
// under the same lock, so `go test -race` stays clean, and the copy does not
// share the targets slice that the next dispatch rewrites.
func TestTickPersistenceSnapshotsControlUnderLock(t *testing.T) {
	ctrl := &control.State{
		SiteMeterDriver: "meter",
		LastTargets:     []control.DispatchTarget{{Driver: "battery", TargetW: 321}},
	}
	ctrlMu := &sync.Mutex{}
	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("meter")
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	tel.RecordDriverSuccess("meter")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			ctrlMu.Lock()
			ctrl.SiteMeterDriver = []string{"meter", "replacement-meter"}[i%2]
			ctrl.LastTargets = []control.DispatchTarget{{Driver: "battery", TargetW: float64(i)}}
			ctrlMu.Unlock()
		}
	}()
	for i := 0; i < 200; i++ {
		buildHistoryPoint(tel, snapshotTickPersistControl(ctrl, ctrlMu), time.Now().UnixMilli(), time.Minute)
	}
	<-done

	snap := snapshotTickPersistControl(ctrl, ctrlMu)
	ctrlMu.Lock()
	ctrl.LastTargets[0].TargetW = -1
	ctrlMu.Unlock()
	if snap.LastTargets[0].TargetW == -1 {
		t.Fatal("persistence snapshot shares the dispatch targets slice")
	}
}

func TestStaleMeterTickKeepsSamplesAndIndependentLedgerWithoutDispatch(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnableHistoryAggregation(); err != nil {
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

	ctrl := tickPersistControl{
		SiteMeterDriver: "meter",
		LastTargets:     []control.DispatchTarget{{Driver: "battery", TargetW: 321}},
	}
	freshness := evaluateSiteDispatchFreshnessAt(tel, "meter", 16, 3, time.Minute, now)
	if freshness.Allowed() || freshness.Reason != siteDispatchMeterStale {
		t.Fatalf("stale meter dispatch decision = %+v", freshness)
	}
	if _, err := persistTelemetryTick(st, tel, ctrl, now.UnixMilli(), time.Minute, testEnergyIdentity(st)); err != nil {
		t.Fatal(err)
	}

	tel.Update("solar", telemetry.DerPV, -400, nil, json.RawMessage(`{"generation_wh":110}`))
	tel.Get("solar", telemetry.DerPV).UpdatedAt = now
	tel.RecordDriverSuccess("solar")
	if _, err := persistTelemetryTick(st, tel, ctrl, time.Now().UnixMilli(), time.Minute, testEnergyIdentity(st)); err != nil {
		t.Fatal(err)
	}

	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	history, err := st.LoadHistory(base.UnixMilli(), time.Now().Add(time.Second).UnixMilli(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 || history[0].Missing&state.MissingGrid == 0 {
		t.Fatalf("stale meter lost independent history or became known: %+v", history)
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

func testEnergyIdentity(st *state.Store) energyIdentityLookup {
	return func(name string) state.Device {
		if d := st.LookupDeviceByDriverName(name); d != nil {
			return *d
		}
		return state.Device{}
	}
}

func TestChargerEnergyCounter(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  string
		want *float64
	}{
		{"a session counter", `{"session_wh":800}`, ptrFloat(800)},
		{"a lifetime counter", `{"total_charge_wh":5000}`, ptrFloat(5000)},
		{"a coarse session counter is left out", `{"session_wh":800,"session_wh_coarse":true}`, nil},
		{"a real counter beside a coarse one stays", `{"session_wh":800,"session_wh_coarse":true,"total_charge_wh":5000}`, ptrFloat(5000)},
		{"the flag off changes nothing", `{"session_wh":800,"session_wh_coarse":false}`, ptrFloat(800)},
		{"no counter", `{}`, nil},
	} {
		raw := json.RawMessage(c.raw)
		got := chargerEnergyCounter(raw, energyNumbers(raw))
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

// 8 November 2026: a charger whose session counter moves in 1 kWh steps
// delivered 24 kWh, and readings seconds apart made the record reject most of
// the steps. With the counter left out it books the power.
func TestCoarseChargerIsBookedFromPower(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	base := time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC).UnixMilli()
	const stepMS, hourMS, chargeW = int64(2_000), int64(3_600_000), 10_000.0
	for at := int64(0); at <= hourMS; at += stepMS {
		power := chargeW
		counter := float64(at) / float64(hourMS) * chargeW
		counter -= math.Mod(counter, 1000) // whole kWh, as Easee reports it
		raw := json.RawMessage(fmt.Sprintf(`{"session_wh":%f,"session_wh_coarse":true}`, counter))
		if err := st.RecordTickWithEnergy(state.HistoryPoint{TsMs: base + at}, nil, []state.EnergyObservation{{
			AssetID: "charger", DeviceID: "charger", AssetKind: state.AssetVehicleCharger, Label: "charger",
			Flow: state.FlowVehicleCharge, AtMs: base + at, PowerW: &power,
			CounterWh: chargerEnergyCounter(raw, energyNumbers(raw)),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	points, _, err := st.LoadEnergyHistory(state.EnergyHistoryQuery{
		AssetID: "charger", SinceMS: base, UntilMS: base + hourMS + 60_000, BucketMS: 30 * 60_000, Limit: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, p := range points {
		if p.Quality == "invalid" || p.Quality == "reset" {
			t.Fatalf("a rejected reading: %+v", p)
		}
		total += p.EnergyWh
	}
	if math.Abs(total-10_000) > 1 {
		t.Fatalf("booked %.0f Wh, want 10000", total)
	}
}

func ptrFloat(v float64) *float64 { return &v }
