package control

import (
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
