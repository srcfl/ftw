package control

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
)

func TestFuseLimitedManualDemandMakesBatteryYield(t *testing.T) {
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SetEVFuseDemands([]loadpoint.FuseDemand{{DriverName: "car", PowerW: 10350, PhaseMode: "3p"}})
	// A zero final offer must not erase the demand that asks the battery to
	// back off. The next charger offer still needs measured fuse headroom.
	st.EVCommandedW = 0
	if got := scheduledEVPowerW(st); got != 10350 {
		t.Fatalf("clamped manual demand lost: %v W", got)
	}
	store := seedStore(-6000, []struct {
		name          string
		currentW, soc float64
	}{{"battery", 0, .5}})
	describeFuse(st, 5000)
	targets := ComputeDispatch(store, st, caps(map[string]float64{"battery": 10000}), 5000)
	if len(targets) != 1 || targets[0].TargetW > 650.001 {
		t.Fatalf("battery used space requested by the car: %v", targets)
	}
	st.SetEVFuseDemands(nil)
	if got := scheduledEVPowerW(st); got != 0 {
		t.Fatalf("cleared demand still reserves %v W", got)
	}
}

func TestFuseDemandSnapshotDoesNotDoubleCountOrAlias(t *testing.T) {
	st := NewState(0, 0, "meter")
	demands := []loadpoint.FuseDemand{
		{DriverName: "car", PowerW: 6900, PhaseMode: "3p"},
		{DriverName: "car", PowerW: 10350, PhaseMode: "1p"},
		{DriverName: "other", PowerW: 1380, PhaseMode: "unknown"},
		{DriverName: "", PowerW: 5000},
		{DriverName: "invalid", PowerW: math.NaN()},
		{DriverName: "invalid", PowerW: math.Inf(1)},
		{DriverName: "invalid", PowerW: -1},
	}
	st.SetEVFuseDemands(demands)
	demands[0].PowerW = 100000
	if got := scheduledEVPowerW(st); got != 11730 {
		t.Fatalf("deduplicated demand = %v W", got)
	}
	if len(st.evFuseDemands) != 2 || st.evFuseDemands[0].phaseMode != "auto" || st.evFuseDemands[1].phaseMode != "auto" {
		t.Fatalf("mixed or unknown phase must stay conservative: %+v", st.evFuseDemands)
	}
}

func TestLiveFuseDemandReplacesOldPlanReservation(t *testing.T) {
	now := time.Now()
	st := newStateWithEnergyDispatch(SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{"car": 100}, LoadpointMaxPowerW: map[string]float64{"car": 10350}}, "meter")
	st.SetEVFuseDemands(nil)
	if got := scheduledEVPowerW(st); got != 0 {
		t.Fatalf("stopped/completed/duty-off car still reserves %v W from old plan", got)
	}
	st.SetEVFuseDemands([]loadpoint.FuseDemand{{DriverName: "car", PowerW: 4140, PhaseMode: "3p"}})
	if got := scheduledEVPowerW(st); got != 4140 {
		t.Fatalf("live request must replace old on-power: %v W", got)
	}
}

func TestScheduledEVKeepsBudgetBeforeBatteryCharge(t *testing.T) {
	for _, draw := range []float64{0, 4000, 8000} {
		t.Run(fmt.Sprint(draw), func(t *testing.T) {
			now := time.Now()
			dir := SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 1250, LoadpointEnergyWh: map[string]float64{"garage": 2000}, Strategy: "arbitrage"}
			store := seedStore(200+draw, []struct {
				name          string
				currentW, soc float64
			}{{"ferroamp", 0, .5}})
			st := newStateWithEnergyDispatch(dir, "ferroamp")
			st.EVChargingW = draw
			st.BatteryCoversEV = true
			describeFuse(st, 11000)
			targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11000)
			if len(targets) != 1 {
				t.Fatalf("targets %v", targets)
			}
			if !st.FuseSaturated {
				t.Fatalf("EV budget lost while drawing %.0f", draw)
			}
			// 11,000 W less the 1 A margin on three phases, the house and the car.
			if roomW := 11000 - 690 - 200 - 8000.0; targets[0].TargetW < 0 || targets[0].TargetW > roomW+0.1 {
				t.Fatalf("optional battery charge exceeds remaining %.0f W: %v", roomW, targets)
			}
		})
	}
}

func TestScheduledEVRejectsExpiredDemand(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{SlotStart: now.Add(-30 * time.Minute), SlotEnd: now.Add(-15 * time.Minute), LoadpointEnergyWh: map[string]float64{"garage": 2000}}
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	if got := scheduledEVPowerW(st); got != 0 {
		t.Fatalf("stale demand reserved %.0f W", got)
	}
}

func TestScheduledEVReservesOnPowerNotMean(t *testing.T) {
	now := time.Now()
	d := mpc.SlotDirective{
		SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
		BatteryEnergyWh:    1250,
		GridW:              10600,
		LoadpointEnergyWh:  map[string]float64{"easee": 100},
		LoadpointMaxPowerW: map[string]float64{"easee": 10350},
		Strategy:           mpc.ModeArbitrage,
	}
	st := newStateWithEnergyDispatch(SlotDirectiveFromMPC(d), "ferroamp")
	if got := scheduledEVPowerW(st); got != 10350 {
		t.Fatalf("10350 W EV pulse with 100 Wh budget reserved %.0f W", got)
	}
	store := seedStore(200, []struct {
		name          string
		currentW, soc float64
	}{{"ferroamp", 0, .5}})
	describeFuse(st, 11000)
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11000)
	if len(targets) != 1 {
		t.Fatalf("targets %v", targets)
	}
	if !st.FuseSaturated {
		t.Fatal("on-power not reserved")
	}
	if targets[0].TargetW < 0 || targets[0].TargetW > 0.1 {
		t.Fatalf("optional battery charge took EV on-power: %v", targets)
	}
}

func TestScheduledEVSumsSimultaneousOnPower(t *testing.T) {
	now := time.Now()
	st := newStateWithEnergyDispatch(SlotDirective{
		SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
		LoadpointEnergyWh:  map[string]float64{"a": 50, "b": 80},
		LoadpointMaxPowerW: map[string]float64{"a": 4140, "b": 6900},
		Strategy:           "arbitrage",
	}, "ferroamp")
	if got := scheduledEVPowerW(st); got != 11040 {
		t.Fatalf("simultaneous pulses reserved %.0f W, want 11040", got)
	}
}

func TestScheduledEVFallsBackToMeanWithoutOnPower(t *testing.T) {
	now := time.Now()
	st := newStateWithEnergyDispatch(SlotDirective{
		SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{"garage": 2000},
		Strategy:          "arbitrage",
	}, "ferroamp")
	if got := scheduledEVPowerW(st); math.Abs(got-8000) > 1e-6 {
		t.Fatalf("mean reservation = %.0f W, want 8000", got)
	}
}

func TestChargerOfferReservesFuseBeforeOptionalBatteryCharge(t *testing.T) {
	for _, mode := range []Mode{ModeSelfConsumption, ModePlannerSelf, ModePlannerArbitrage} {
		t.Run(string(mode), func(t *testing.T) {
			now := time.Now()
			dir := SlotDirective{SlotStart: now, SlotEnd: now.Add(time.Minute), EVOnly: true,
				LoadpointEnergyWh: map[string]float64{"manual": 11000.0 / 60}, LoadpointMaxPowerW: map[string]float64{"manual": 11000}}
			store := seedStore(-6000, []struct {
				name          string
				currentW, soc float64
			}{{"battery", 0, .5}})
			st := newStateWithEnergyDispatch(dir, "ferroamp")
			st.Mode = mode
			st.EVCommandedW = 11000 // Final manual/reserve offer, still 0 W measured.
			if got := scheduledEVPowerW(st); got != 11000 {
				t.Fatalf("ramping offer lost in %s: %v", mode, got)
			}
			describeFuse(st, 5000)
			targets := ComputeDispatch(store, st, caps(map[string]float64{"battery": 10000}), 5000)
			if len(targets) != 1 || targets[0].TargetW > 0.001 {
				t.Fatalf("battery took the EV's offered room: targets=%v", targets)
			}
			if mode.IsPlannerMode() && !st.PlanStale {
				t.Fatal("EV-only directive claimed a fresh battery plan")
			}
		})
	}
}

func TestBatterylessGoalIntentPublishesJointFuseBudget(t *testing.T) {
	now := time.Now()
	st := newStateWithEnergyDispatch(SlotDirective{SlotStart: now, SlotEnd: now.Add(time.Minute), EVOnly: true,
		LoadpointEnergyWh: map[string]float64{"car": 11000.0 / 60}, LoadpointMaxPowerW: map[string]float64{"car": 11000}}, "ferroamp")
	st.EVCommandedW = 11000
	store := seedStore(4000, nil)
	describeFuse(st, 10000)
	if targets := ComputeDispatch(store, st, nil, 10000); len(targets) != 0 {
		t.Fatalf("invented a battery: %v", targets)
	}
	if !st.FuseSaturated {
		t.Fatal("11 kW of EV demand beside 4 kW of house fits a 10 kW site")
	}
}
