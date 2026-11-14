package control

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestBatteryBoostEnergyDispatchCoversOnlyLeasedLoadpointWatts(t *testing.T) {
	store := seedStore(10000, []struct {
		name          string
		currentW, soc float64
	}{{"battery", 0, 0.8}})
	store.Update("leased-ev", telemetry.DerEV, 3000, nil, nil)
	store.DriverHealthMut("leased-ev").RecordSuccess()
	store.Update("plain-ev", telemetry.DerEV, 6000, nil, nil)
	store.DriverHealthMut("plain-ev").RecordSuccess()
	now := time.Now()
	state := NewState(0, 0, "ferroamp")
	state.Mode = ModePlannerArbitrage
	state.UseEnergyDispatch = true
	state.SlewRateW = 100000
	state.MinDispatchIntervalS = 0
	state.BatteryBoostEVChargingW = 3000
	state.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{
			SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
			BatteryEnergyWh: -2250, // stale plan asks for -9 kW
		}, true
	}
	targets := ComputeDispatch(store, state, map[string]float64{"battery": 10000}, 50000)
	if len(targets) != 1 || targets[0].TargetW != -4000 {
		t.Fatalf("targets = %+v, want leased 3 kW + house 1 kW only", targets)
	}
}

func TestBatteryBoostCoverageIsLimitedToAuthorisedLiveWatts(t *testing.T) {
	state := &State{EVChargingW: 9000, BatteryBoostEVChargingW: 3000}
	if got := state.coveredEVChargingW(); got != 3000 {
		t.Fatalf("covered = %.0f, want 3000", got)
	}
	if got := state.uncoveredEVChargingW(); got != 6000 {
		t.Fatalf("uncovered = %.0f, want 6000", got)
	}
	state.BatteryCoversEV = true
	if got := state.coveredEVChargingW(); got != 9000 {
		t.Fatalf("legacy covered = %.0f, want 9000", got)
	}
}

func TestBatteryBoostReserveClampsNormalDischarge(t *testing.T) {
	store := telemetry.NewStore()
	soc := 0.30
	store.Update("battery", telemetry.DerBattery, 0, &soc, nil)
	state := &State{BatteryBoostReserveSoC: 0.30}
	got := applyBatteryBoostReserve([]DispatchTarget{{Driver: "battery", TargetW: -4000}}, store, state, map[string]float64{"battery": 10000})
	if len(got) != 1 || got[0].TargetW != 0 || !got[0].Clamped {
		t.Fatalf("reserve clamp = %+v", got)
	}

	soc = 0.31
	store.Update("battery", telemetry.DerBattery, 0, &soc, nil)
	got = applyBatteryBoostReserve([]DispatchTarget{{Driver: "battery", TargetW: -4000}}, store, state, map[string]float64{"battery": 10000})
	if got[0].TargetW != -4000 {
		t.Fatalf("above reserve target = %.0f, want -4000", got[0].TargetW)
	}
}

func TestBatteryBoostReserveFailsClosedWithoutSoC(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("battery", telemetry.DerBattery, 0, nil, nil)
	state := &State{BatteryBoostReserveSoC: 0.30}
	got := applyBatteryBoostReserve([]DispatchTarget{{Driver: "battery", TargetW: -1000}}, store, state, map[string]float64{"battery": 10000})
	if got[0].TargetW != 0 {
		t.Fatalf("missing SoC target = %.0f, want 0", got[0].TargetW)
	}
}

func TestBatteryBoostReserveCapsBetweenTickOvershoot(t *testing.T) {
	store := telemetry.NewStore()
	soc := 0.3001 // only 1 Wh above a 30% reserve on a 10 kWh pack
	store.Update("battery", telemetry.DerBattery, 0, &soc, nil)
	state := &State{BatteryBoostReserveSoC: 0.30, MinDispatchIntervalS: 5}
	got := applyBatteryBoostReserve(
		[]DispatchTarget{{Driver: "battery", TargetW: -4000}}, store, state,
		map[string]float64{"battery": 10000},
	)
	// 1 Wh headroom × 0.9 efficiency × 3600 / 5 s = 648 W.
	if got[0].TargetW < -649 || got[0].TargetW > -647 {
		t.Fatalf("near-reserve target = %.1f W, want about -648 W", got[0].TargetW)
	}
}

func TestFuseEmergencyRemainsSuperiorToBatteryBoostReserve(t *testing.T) {
	store := seedStore(7000, []struct {
		name          string
		currentW, soc float64
	}{{"battery", 0, 0.30}})
	state := NewState(0, 0, "ferroamp")
	state.BatteryBoostReserveSoC = 0.30
	state.DriverLimits = map[string]PowerLimits{"battery": {MaxDischargeW: 5000}}
	targets := applyDispatchSafetyPipeline(
		[]DispatchTarget{{Driver: "battery", TargetW: -1000}},
		store, state, map[string]float64{"battery": 10000}, 5000,
		dispatchSafetyOptions{},
	)
	if len(targets) != 1 || targets[0].TargetW >= 0 {
		t.Fatalf("fuse emergency did not override lease reserve: %+v", targets)
	}
}

// A boost covers the house and the boosted car even when the arbitrage plan
// keeps the battery for later (idle slot) or charges it (charge slot). The
// battery stops at grid zero: car draw the charger still reports but the
// meter no longer shows is never exported.
func TestBatteryBoostCoversCarWhatTheSlotSays(t *testing.T) {
	slots := map[string]SlotDirective{
		"idle":   {BatteryEnergyWh: 0, HasPlannedGridW: true, PlannedGridW: 500},
		"charge": {BatteryEnergyWh: 500, HasPlannedGridW: true, PlannedGridW: 0},
	}
	for name, slot := range slots {
		for _, tc := range []struct {
			gridW, boostW, want float64
		}{
			{8500, 0, 0},        // no boost: the battery does not cover the car
			{8500, 8000, -8500}, // car 8 kW + house 0.5 kW from the battery
			{500, 8000, -500},   // the car stopped; Easee still reports it
		} {
			store := seedStore(tc.gridW, []struct {
				name          string
				currentW, soc float64
			}{{"battery", 0, 0.6}})
			store.Update("easee", telemetry.DerEV, 8000, nil, nil)
			store.DriverHealthMut("easee").RecordSuccess()
			now := time.Now()
			state := NewState(0, 0, "ferroamp")
			state.Mode = ModePlannerArbitrage
			state.UseEnergyDispatch = true
			state.SlewRateW = 100000
			state.MinDispatchIntervalS = 0
			state.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 10000, MaxDischargeW: 10000}}
			state.BatteryBoostEVChargingW = tc.boostW
			dir := slot
			dir.SlotStart, dir.SlotEnd = now, now.Add(15*time.Minute)
			state.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }
			targets := ComputeDispatch(store, state, map[string]float64{"battery": 20000}, 50000)
			var got float64
			for _, tg := range targets {
				got += tg.TargetW
			}
			// Without a boost a slot may still cover the 0.5 kW house, never the car.
			if (tc.want == 0 && got < -501) || (tc.want < 0 && math.Abs(got-tc.want) > 1) {
				t.Errorf("%s slot, grid %.0f, boost %.0f: target %.0f, want %.0f (%+v)",
					name, tc.gridW, tc.boostW, got, tc.want, targets)
			}
		}
	}
}

// The battery follows a boosted car both ways within a tick: down when the
// car slows, and straight to the house (or the plan's idle) when it stops,
// instead of exporting while a controller eases it down.
func TestBatteryBoostFollowsTheCarBothWays(t *testing.T) {
	now := time.Now()
	state := NewState(0, 0, "ferroamp")
	state.Mode = ModePlannerArbitrage
	state.UseEnergyDispatch = true
	state.SlewRateW = 100000
	state.MinDispatchIntervalS = 0
	state.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	state.clock = func() time.Time { return now }
	dir := SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), HasPlannedGridW: true, PlannedGridW: 500}
	state.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	tick := func(batteryW, gridW, carW, boostW float64) float64 {
		t.Helper()
		store := seedStore(gridW, []struct {
			name          string
			currentW, soc float64
		}{{"battery", batteryW, 0.6}})
		store.Update("easee", telemetry.DerEV, carW, nil, nil)
		store.DriverHealthMut("easee").RecordSuccess()
		state.BatteryBoostEVChargingW = boostW
		var sum float64
		for _, tg := range ComputeDispatch(store, state, map[string]float64{"battery": 20000}, 50000) {
			sum += tg.TargetW
		}
		now = now.Add(5 * time.Second)
		return sum
	}

	// House 0.5 kW, car 8 kW: the battery takes both.
	if got := tick(0, 8500, 8000, 8000); math.Abs(got+8500) > 1 {
		t.Fatalf("car 8 kW: target %.0f, want -8500", got)
	}
	// The car slows to 5 kW: the meter shows 3 kW export; back to -5.5 kW now.
	if got := tick(-8500, -3000, 5000, 5000); math.Abs(got+5500) > 1 {
		t.Fatalf("car 5 kW: target %.0f, want -5500", got)
	}
	// The car stops. The charger reading is 0 while the loadpoint still says
	// 5 kW. No export: grid zero for the house, or the idle plan's 0 W.
	if got := tick(-5500, -5000, 0, 5000); got < -501 || got > 0 {
		t.Fatalf("car stopped: target %.0f, want between -500 and 0", got)
	}
}
