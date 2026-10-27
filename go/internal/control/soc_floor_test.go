package control

import (
	"testing"
	"time"
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
