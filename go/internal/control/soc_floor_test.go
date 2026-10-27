package control

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A planner mode with no plan falls back to self-consumption. It must keep
// the owner's reserve (planner soc_min 10 %), not discharge to Core's 5 %.
func TestPlannerFallbackKeepsHouseReserve(t *testing.T) {
	for _, mode := range []Mode{ModePlannerSelf, ModePlannerCheap, ModePlannerPassiveArbitrage, ModePlannerArbitrage} {
		t.Run(string(mode), func(t *testing.T) {
			store := seedStore(2000, []struct {
				name          string
				currentW, soc float64
			}{
				{"ferroamp", 0, 0.08},
			})
			st := NewState(0, 50, "ferroamp")
			st.Mode = mode
			st.UseEnergyDispatch = true
			st.SlewRateW = 10000
			st.MinDispatchIntervalS = 0
			st.DriverLimits = map[string]PowerLimits{"ferroamp": {SoCMin: 0.10}}
			st.SlotDirective = func(time.Time) (SlotDirective, bool) { return SlotDirective{}, false }
			st.PlanTarget = func(time.Time) (string, float64, string, bool) { return "", 0, "", false }

			targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
			if !st.PlanStale {
				t.Fatalf("expected stale-plan fallback")
			}
			for _, tg := range targets {
				if tg.TargetW < 0 {
					t.Errorf("%s: stale plan, SoC 8%% < 10%% house reserve, TargetW=%.0f W (discharges below reserve)", tg.Driver, tg.TargetW)
				}
			}
		})
	}
}

// The hourly import ceiling (a billing limit) runs through the fuse relief.
// 25 A site, every phase at 11.6 A (far from the fuse), meter 8 kW,
// ceiling 5 kW. Neither the SoC floor nor the boost reserve may be crossed.
func importCeilingSite(soc float64) (*State, func(targets []DispatchTarget) []DispatchTarget) {
	store, st, caps := setupPerPhaseSite(3, 230, 8000, 11.6, 11.6, 11.6, soc, 10000)
	st.SiteFuseAmps = 25
	st.PeakImportCeilingW = 5000
	lim := st.DriverLimits["bat"]
	lim.SoCMin = 0.10
	st.DriverLimits["bat"] = lim
	return st, func(targets []DispatchTarget) []DispatchTarget {
		return applyDispatchSafetyPipeline(targets, store, st, caps, 25*230*3, dispatchSafetyOptions{})
	}
}

func TestImportCeilingKeepsSoCFloor(t *testing.T) {
	_, run := importCeilingSite(0.06)
	if out := run([]DispatchTarget{{Driver: "bat"}}); out[0].TargetW < 0 {
		t.Fatalf("import ceiling ordered %.0f W at SoC 6 %%, floor 10 %%", out[0].TargetW)
	}
	// Above the floor the ceiling still shaves the peak.
	_, run = importCeilingSite(0.50)
	if out := run([]DispatchTarget{{Driver: "bat"}}); out[0].TargetW > -2999 {
		t.Fatalf("import ceiling at SoC 50 %%: got %.0f W, want -3000 W", out[0].TargetW)
	}
}

func TestImportCeilingKeepsBoostReserve(t *testing.T) {
	st, run := importCeilingSite(0.20)
	st.BatteryBoostReserveSoC = 0.30
	if out := run([]DispatchTarget{{Driver: "bat"}}); out[0].TargetW < 0 {
		t.Fatalf("boost reserve 30 %%, SoC 20 %%, no fuse risk: ordered %.0f W", out[0].TargetW)
	}
}

func TestImportCeilingComputeDispatchKeepsBoostReserve(t *testing.T) {
	store, st, caps := setupPerPhaseSite(3, 230, 8000, 11.6, 11.6, 11.6, 0.06, 10000)
	st.SiteFuseAmps = 25
	st.PeakImportCeilingW = 5000
	st.BatteryBoostReserveSoC = 0.30
	st.SetGridTarget(0)
	st.MinDispatchIntervalS = 0
	for _, tg := range ComputeDispatch(store, st, caps, 25*230*3) {
		if tg.TargetW < 0 {
			t.Fatalf("ComputeDispatch (mode %v) ordered %s %.0f W at SoC 6 %%, reserve 30 %%", st.Mode, tg.Driver, tg.TargetW)
		}
	}
}

// A real fuse overage may still go past the boost reserve, but not past the
// SoC floor: below it the car is the lever.
func TestFuseReliefPassesBoostReserveButKeepsSoCFloor(t *testing.T) {
	run := func(soc float64) float64 {
		// 16 A fuse, 11040 W, meter 13 kW: about 2 kW over.
		store, st, caps := setupPerPhaseSite(3, 230, 13000, 18.8, 18.8, 18.8, soc, 10000)
		st.BatteryBoostReserveSoC = 0.30
		lim := st.DriverLimits["bat"]
		lim.SoCMin = 0.10
		st.DriverLimits["bat"] = lim
		out := applyDispatchSafetyPipeline([]DispatchTarget{{Driver: "bat"}}, store, st, caps, 11040, dispatchSafetyOptions{})
		return out[0].TargetW
	}
	if got := run(0.20); got >= 0 {
		t.Fatalf("fuse overage at SoC 20 %%, reserve 30 %%: got %.0f W, want discharge", got)
	}
	if got := run(0.08); got < 0 {
		t.Fatalf("fuse overage at SoC 8 %%, floor 10 %%: got %.0f W, want no discharge", got)
	}
}

func staleFloorState(slew float64) *State {
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerCheap
	st.UseEnergyDispatch = true
	st.SlewRateW = slew
	st.MinDispatchIntervalS = 0
	st.DriverLimits = map[string]PowerLimits{"bat": {SoCMin: 0.10}, "bat2": {SoCMin: 0.10}}
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return SlotDirective{}, false }
	st.PlanTarget = func(time.Time) (string, float64, string, bool) { return "", 0, "", false }
	return st
}

// SoC drifts under the floor while the battery already covers the house.
// Inside the deadband a tick with no targets leaves the driver on its last
// -2000 W setpoint, and outside it slew hands back -1500 W. Both must stop.
func TestStaleFloorStopsRunningDischarge(t *testing.T) {
	cases := []struct {
		name         string
		gridW, slewW float64
	}{
		{"inside deadband", 20, 10000},
		{"after slew", 2000, 500},
	}
	for _, c := range cases {
		for _, soc := range []float64{0.08, 0.04} {
			store := seedStore(c.gridW, []struct {
				name          string
				currentW, soc float64
			}{{"bat", -2000, soc}})
			st := staleFloorState(c.slewW)
			targets := ComputeDispatch(store, st, caps(map[string]float64{"bat": 15200}), 11040)
			if len(targets) == 0 {
				t.Errorf("%s, SoC %.2f: no target, the -2000 W setpoint stays", c.name, soc)
			}
			for _, tg := range targets {
				if tg.TargetW < 0 {
					t.Errorf("%s, SoC %.2f: %s %.0f W under the floor", c.name, soc, tg.Driver, tg.TargetW)
				}
			}
		}
	}
}

// A battery under the floor hands its share to a sibling above it.
func TestStaleFloorSiblingTakesShare(t *testing.T) {
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{{"bat", 0, 0.08}, {"bat2", 0, 0.50}})
	st := staleFloorState(10000)
	targets := ComputeDispatch(store, st, caps(map[string]float64{"bat": 15200, "bat2": 15200}), 11040)
	var sum float64
	for _, tg := range targets {
		if tg.Driver == "bat" && tg.TargetW < 0 {
			t.Errorf("bat under the floor discharged %.0f W", tg.TargetW)
		}
		sum += tg.TargetW
	}
	if sum > -1000 {
		t.Errorf("sibling did not take the share: sum %.0f W", sum)
	}
}

// Plain self_consumption keeps Core's 5 % floor; soc_min is a planner rule.
func TestSelfConsumptionStillDischargesUnderSoCMin(t *testing.T) {
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{{"bat", 0, 0.08}})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.DriverLimits = map[string]PowerLimits{"bat": {SoCMin: 0.10}}
	targets := ComputeDispatch(store, st, caps(map[string]float64{"bat": 15200}), 11040)
	if len(targets) == 0 || targets[0].TargetW >= 0 {
		t.Errorf("self_consumption at SoC 8 %%: %+v", targets)
	}
}

// Two batteries, one under the boost reserve. 3 kW fuse overage plus 5 kW
// that only the hourly limit asks for. The battery above the reserve takes
// the limit part; the other still serves the fuse.
func TestSplitReliefUsesBothBatteries(t *testing.T) {
	store, st, cp := setupPerPhaseSite(3, 230, 20250, 29.35, 29.35, 29.35, 0.20, 5000)
	soc := 0.60
	store.Update("bat2", telemetry.DerBattery, 0, &soc, nil)
	store.DriverHealthMut("bat2").RecordSuccess()
	cp["bat2"] = 15200
	st.DriverLimits["bat2"] = PowerLimits{MaxChargeW: 10000, MaxDischargeW: 5000}
	st.SiteFuseAmps = 25
	st.PeakImportCeilingW = 12250
	st.BatteryBoostReserveSoC = 0.30
	out := applyDispatchSafetyPipeline([]DispatchTarget{{Driver: "bat"}, {Driver: "bat2"}}, store, st, cp, 25*230*3, dispatchSafetyOptions{})
	got := map[string]float64{}
	for _, o := range out {
		got[o.Driver] = o.TargetW
	}
	if got["bat"] > -2999 || got["bat2"] > -4999 {
		t.Errorf("want bat -3000 W, bat2 -5000 W; got bat %.0f W, bat2 %.0f W", got["bat"], got["bat2"])
	}
}

// No discharge order goes out at exactly the floor (C7: rig seeds 207534
// and 207536 ordered discharge at 5.0 %).
func TestClampWithSoCBlocksDischargeAtFloor(t *testing.T) {
	if v, was := clampWithSoC(-1000, batteryInfo{soc: dischargeFloorSoC}); v != 0 || !was {
		t.Fatalf("SoC exactly 5 %%: got %.0f W clamped=%v, want 0 W", v, was)
	}
	if v, was := clampWithSoC(-1000, batteryInfo{soc: 0.051}); v != -1000 || was {
		t.Fatalf("SoC 5.1 %%: got %.0f W clamped=%v, want -1000 W", v, was)
	}
}

// A fresh plan that asks for discharge at exactly 5 % gets no discharge.
func TestFreshPlanNoDischargeAtFloor(t *testing.T) {
	for _, energy := range []bool{false, true} {
		store := seedStore(2000, []struct {
			name          string
			currentW, soc float64
		}{{"bat", 0, 0.05}})
		st := NewState(0, 50, "ferroamp")
		st.Mode = ModePlannerArbitrage
		st.UseEnergyDispatch = energy
		st.SlewRateW = 10000
		st.MinDispatchIntervalS = 0
		st.SlotDirective = func(now time.Time) (SlotDirective, bool) {
			return SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: -500, Strategy: "arbitrage"}, true
		}
		st.PlanTarget = func(time.Time) (string, float64, string, bool) { return "discharge", -2000, "discharge", true }
		targets := ComputeDispatch(store, st, caps(map[string]float64{"bat": 15200}), 11040)
		if st.PlanStale {
			t.Fatalf("energy=%v: expected a fresh plan", energy)
		}
		for _, tg := range targets {
			if tg.TargetW < 0 {
				t.Errorf("energy=%v: SoC exactly 5 %%, %s ordered %.0f W", energy, tg.Driver, tg.TargetW)
			}
		}
	}
}

// Stale plan, soc_min 10 %: just under the floor (207531, 207532 at
// 5.5-5.8 %) and exactly at it, the battery gets 0 W.
func TestStaleFloorZeroAtAndUnderFloor(t *testing.T) {
	for _, soc := range []float64{0.058, 0.10} {
		store := seedStore(2000, []struct {
			name          string
			currentW, soc float64
		}{{"bat", -1500, soc}})
		st := staleFloorState(10000)
		targets := ComputeDispatch(store, st, caps(map[string]float64{"bat": 15200}), 11040)
		if !st.PlanStale {
			t.Fatalf("SoC %.3f: expected stale-plan fallback", soc)
		}
		if len(targets) != 1 || targets[0].TargetW != 0 {
			t.Errorf("SoC %.3f, floor 10 %%: got %+v, want bat 0 W", soc, targets)
		}
	}
}

// Fuse relief and the hourly import limit stop at exactly the floor.
func TestFuseTargetBoundsAtFloor(t *testing.T) {
	soc := 0.10
	r := &telemetry.DerReading{SoC: &soc}
	if lower, _ := fuseTargetBounds(r, PowerLimits{SoCMin: 0.10}); lower != 0 {
		t.Fatalf("SoC exactly at the 10 %% floor: lower bound %.0f W, want 0", lower)
	}
	soc = 0.101
	if lower, _ := fuseTargetBounds(r, PowerLimits{SoCMin: 0.10}); lower >= 0 {
		t.Fatalf("SoC 10.1 %%, floor 10 %%: lower bound %.0f W, want discharge allowed", lower)
	}
}
