package control

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// runCarBeforeBattery drives planner_self in a charge slot against a plant
// whose meter is the site without the battery plus the battery's last
// target. siteW is house plus car minus PV, site-signed. It returns every
// target the battery received.
func runCarBeforeBattery(t *testing.T, coversEV bool, siteW, carW, startBatteryW float64) []float64 {
	t.Helper()
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 350, // a PV charge slot: 1.4 kW for 15 minutes
		Strategy:        "self_consumption",
	}
	store := seedStore(siteW+startBatteryW, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", startBatteryW, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }
	st.EVChargingW = carW
	st.BatteryCoversEV = coversEV
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	const fuseW = 3 * 25 * 230 // keep the fuse out of this policy check
	batteryW := startBatteryW
	var got []float64
	for i := 0; i < 20; i++ {
		targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), fuseW)
		if len(targets) == 1 {
			batteryW = targets[0].TargetW
			got = append(got, batteryW)
		}
		store.Update("ferroamp", telemetry.DerBattery, batteryW, ptrF64(0.5), nil)
		store.Update("ferroamp", telemetry.DerMeter, siteW+batteryW, nil, nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
	}
	return got
}

func lastOr(v []float64, def float64) float64 {
	if len(v) == 0 {
		return def
	}
	return v[len(v)-1]
}

// Seeds 207535, 207539 and 207530: PV 5 kW, house 3.6 kW, car 11 kW. The
// site imports about 10 kW, yet with battery_covers_ev off the battery
// charged from the PV the car was using. The car comes first: the battery
// charges only from what is left after the car, and is never discharged
// for the car.
func TestCarBeforeBatteryChargeWhenCoverIsOff(t *testing.T) {
	const pvW, houseW, carW = 5000.0, 3611.0, 11040.0
	got := runCarBeforeBattery(t, false, houseW+carW-pvW, carW, 632)
	for i, w := range got {
		if w > 632+1 {
			t.Fatalf("tick %d: battery charge rose to %.0f W while the site imports for the car", i, w)
		}
		if w < -1 {
			t.Fatalf("tick %d: battery discharged %.0f W into the car with battery_covers_ev off", i, w)
		}
	}
	if final := lastOr(got, 632); math.Abs(final) > 1 {
		t.Errorf("battery settled at %.0f W, want 0 W: the car uses all PV", final)
	}
}

// With PV beyond house and car, the battery takes exactly the surplus the
// car leaves.
func TestCarBeforeBatteryChargesOnlyTheSurplusAfterTheCar(t *testing.T) {
	for _, coversEV := range []bool{false, true} {
		const pvW, houseW, carW = 16000.0, 3000.0, 11040.0
		got := runCarBeforeBattery(t, coversEV, houseW+carW-pvW, carW, 0)
		const wantW = pvW - houseW - carW
		for i, w := range got {
			if w > wantW+60 {
				t.Fatalf("covers_ev=%v tick %d: battery charge %.0f W takes PV the car uses (surplus after car %.0f W)", coversEV, i, w, wantW)
			}
		}
		if final := lastOr(got, 0); math.Abs(final-wantW) > 60 {
			t.Errorf("covers_ev=%v: battery settled at %.0f W, want about %.0f W", coversEV, final, wantW)
		}
	}
}

// battery_covers_ev off still covers the house, never the car.
func TestCarBeforeBatteryCoverOffDischargesForHouseOnly(t *testing.T) {
	const pvW, houseW, carW = 1000.0, 3000.0, 11040.0
	got := runCarBeforeBattery(t, false, houseW+carW-pvW, carW, 0)
	const wantW = pvW - houseW // -2000 W: the house, not the car
	for i, w := range got {
		if w < wantW-60 {
			t.Fatalf("tick %d: battery discharged %.0f W, past the house's %.0f W", i, w, wantW)
		}
	}
	if final := lastOr(got, 0); math.Abs(final-wantW) > 60 {
		t.Errorf("battery settled at %.0f W, want about %.0f W", final, wantW)
	}
}

// battery_covers_ev on lets the battery feed the car instead of charging.
func TestCarBeforeBatteryCoverOnDischargesForTheCar(t *testing.T) {
	const pvW, houseW, carW = 5000.0, 3611.0, 11040.0
	got := runCarBeforeBattery(t, true, houseW+carW-pvW, carW, 632)
	for i, w := range got {
		if w > 632+1 {
			t.Fatalf("tick %d: battery charge rose to %.0f W while the site imports", i, w)
		}
	}
	if final := lastOr(got, 632); final > -1000 {
		t.Errorf("battery settled at %.0f W, want a discharge that covers the car", final)
	}
}

// The energy path follows the same rule: a planned PV charge takes only
// the surplus the car leaves, while a planned purchase keeps its charge.
func TestCarBeforeBatteryEnergyPathPVChargeOnly(t *testing.T) {
	const pvW, houseW, carW = 8000.0, 500.0, 6000.0
	for _, tc := range []struct {
		name     string
		plannedW float64
		wantW    float64
	}{
		{"pv charge", -1000, pvW - houseW - carW},
		{"planned purchase", 2000, 4000},
	} {
		for _, coversEV := range []bool{false, true} {
			now := time.Now()
			dir := SlotDirective{
				SlotStart:       now,
				SlotEnd:         now.Add(15 * time.Minute),
				BatteryEnergyWh: 1000, // 4 kW for 15 minutes
				Strategy:        "cheap",
				PlannedGridW:    tc.plannedW,
				HasPlannedGridW: true,
			}
			store := seedStore(houseW+carW-pvW, []struct {
				name          string
				currentW, soc float64
			}{
				{"ferroamp", 0, 0.5},
			})
			st := NewState(0, 50, "ferroamp")
			st.Mode = ModePlannerCheap
			st.UseEnergyDispatch = true
			st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }
			st.EVChargingW = carW
			st.BatteryCoversEV = coversEV
			st.SlewRateW = 100000
			targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 3*25*230)
			if len(targets) != 1 {
				t.Fatalf("%s covers_ev=%v: want 1 target, got %+v", tc.name, coversEV, targets)
			}
			got := targets[0].TargetW
			if coversEV {
				// The plan's grid check sees the car on the meter and
				// backs the charge off further; only the car-first bound
				// matters here.
				if tc.plannedW < 100 && got > tc.wantW+1 {
					t.Errorf("%s covers_ev=true: battery target %.0f W takes PV the car uses (want at most %.0f W)", tc.name, got, tc.wantW)
				}
				continue
			}
			if math.Abs(got-tc.wantW) > 1 {
				t.Errorf("%s covers_ev=false: battery target %.0f W, want %.0f W", tc.name, got, tc.wantW)
			}
		}
	}
}
