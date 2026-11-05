package control

import (
	"math/rand"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// coverLoadSite is a site whose only battery covers the house load in a
// discharge slot planned to land the grid near 0 W, with no PV.
func coverLoadSite(mode Mode, toleranceW, gridW, batteryW float64) (*telemetry.Store, *State) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
		BatteryEnergyWh: -600, PlannedGridW: 0, HasPlannedGridW: true,
	}
	store := seedStore(gridW, []struct {
		name          string
		currentW, soc float64
	}{{"ferroamp", batteryW, 0.6}})
	st := NewState(0, toleranceW, "ferroamp")
	st.Mode = mode
	st.UseEnergyDispatch = true
	st.SlewRateW = 100_000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }
	return store, st
}

func batteryTarget(t *testing.T, store *telemetry.Store, st *State, holdW float64) float64 {
	t.Helper()
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 20000}), 40000)
	if len(targets) == 0 {
		return holdW // the dead band keeps the current command
	}
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %+v", targets)
	}
	return targets[0].TargetW
}

// A field report on passive arbitrage: the battery covered 2.4 kW of house
// load and the meter showed 100–270 W of export about half the time. A
// battery that may not export now trims its discharge until the site
// imports, and leaves a small import alone.
func TestNoExportDischargeHoldsTheBandAboveZero(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   Mode
		gridW  float64
		wantUp bool // the discharge is trimmed
	}{
		{"passive, report snapshot", ModePlannerPassiveArbitrage, -164, true},
		{"passive, export inside the old band", ModePlannerPassiveArbitrage, -20, true},
		{"passive, small import", ModePlannerPassiveArbitrage, 30, false},
		{"planner_self, export inside the old band", ModePlannerSelf, -20, true},
		{"arbitrage may export, band unchanged", ModePlannerArbitrage, -20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const batteryW = -2571.0
			store, st := coverLoadSite(tc.mode, 42, tc.gridW, batteryW)
			load, battery := tc.gridW-batteryW, batteryW
			for range 20 { // the house load holds still
				grid, soc := load+battery, 0.6
				store.Update("ferroamp", telemetry.DerMeter, grid, nil, nil)
				store.Update("ferroamp", telemetry.DerBattery, battery, &soc, nil)
				battery = batteryTarget(t, store, st, battery)
				if !tc.wantUp && battery != batteryW {
					t.Fatalf("target %.0f W, want the %.0f W discharge kept", battery, batteryW)
				}
			}
			if grid := load + battery; tc.wantUp && (grid < 0 || grid > 2*42) {
				t.Fatalf("settled at %.0f W discharge with the grid at %.0f W, want 0–84 W import", battery, grid)
			}
		})
	}
}

// Surplus that appears while the battery discharges is charged at once.
// The bias may not turn that into grid import beyond the dead band.
func TestNoExportBiasChargesSurplusWithoutImport(t *testing.T) {
	store, st := coverLoadSite(ModePlannerSelf, 42, -1500, -100)
	load, battery := -1500.0+100, -100.0
	for i := range 20 {
		grid, soc := load+battery, 0.6
		store.Update("ferroamp", telemetry.DerMeter, grid, nil, nil)
		store.Update("ferroamp", telemetry.DerBattery, battery, &soc, nil)
		battery = batteryTarget(t, store, st, battery)
		if grid := load + battery; grid > 42 {
			t.Fatalf("tick %d: charging %.0f W imports %.0f W", i, battery, grid)
		}
	}
	if battery < 1300 {
		t.Fatalf("settled at %.0f W, want the 1.4 kW surplus charged", battery)
	}
}

// Closed loop: a battery that follows its command within one tick covers a
// noisy house load with occasional steps. Aimed at 0 W, the site exported
// about as much as it imported; held above 0 W, it exports far less.
func TestNoExportCoverLoadExportsLessThanItImports(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	store, st := coverLoadSite(ModePlannerPassiveArbitrage, 42, 0, -2400)
	load, battery := 2400.0, -2400.0
	var exportTicks int
	var exportWh, importWh float64
	const ticks, tickS = 3000, 5.0
	for range ticks {
		load += rng.NormFloat64() * 60
		if rng.Float64() < 0.01 {
			load += (rng.Float64() - 0.5) * 1500
		}
		load = min(max(load, 600), 4500)
		grid, soc := load+battery, 0.6
		store.Update("ferroamp", telemetry.DerMeter, grid, nil, nil)
		store.Update("ferroamp", telemetry.DerBattery, battery, &soc, nil)
		battery = batteryTarget(t, store, st, battery)
		if grid < -50 {
			exportTicks++
		}
		if grid < 0 {
			exportWh -= grid * tickS / 3600
		} else {
			importWh += grid * tickS / 3600
		}
	}
	t.Logf("export below -50 W in %d of %d ticks; exported %.0f Wh, imported %.0f Wh", exportTicks, ticks, exportWh, importWh)
	if exportWh > importWh/2 || exportTicks > ticks*15/100 {
		t.Fatalf("exported %.0f Wh against %.0f Wh imported, %d of %d ticks below -50 W", exportWh, importWh, exportTicks, ticks)
	}
}
