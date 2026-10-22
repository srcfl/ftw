package loadpoint

import (
	"slices"
	"testing"
	"time"
)

// The Easee step table from the 22 Oct 2026 field report: one-phase levels
// up to 3,800 W, three-phase levels from 4,800 W.
var easeeFieldSteps = []float64{0, 1600, 1900, 2100, 2400, 2600, 2800, 3100, 3300, 3500, 3800,
	4800, 5500, 6200, 6900, 7600, 8300, 9000, 9700, 10400, 11100}

// The planner spread that night at 3.1-3.5 kW, which only one phase can
// deliver. It now gets three-phase levels only, unless the loadpoint is
// pinned to one phase or cannot reach 6 A on three.
func TestPlanningStepsKeepThreePhaseLevels(t *testing.T) {
	site := SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3}
	for _, mode := range []string{"auto", "", "3p"} {
		got := PlanningSteps(Config{MinChargeW: 1600, MaxChargeW: 11100, PhaseMode: mode, AllowedStepsW: easeeFieldSteps}, site)
		if !slices.Equal(got, []float64{0, 4800, 5500, 6200, 6900, 7600, 8300, 9000, 9700, 10400, 11100}) {
			t.Fatalf("%q: planner steps %v", mode, got)
		}
		_, maxW, _ := LimitPower(State{MinChargeW: 1600, MaxChargeW: 11100}, got)
		if minW, _, _ := LimitPower(State{MinChargeW: 1600, MaxChargeW: 11100}, got); minW != 4800 || maxW != 11100 {
			t.Fatalf("%q: planner minimum %.0f W, want the lowest step", mode, minW)
		}
	}
	if got := PlanningSteps(Config{MinChargeW: 1600, MaxChargeW: 11100, PhaseMode: "1p", AllowedStepsW: easeeFieldSteps}, site); !slices.Equal(got, easeeFieldSteps) {
		t.Fatalf("a 1p pin lost its one-phase steps: %v", got)
	}
	if got := PlanningSteps(Config{MinChargeW: 1380, MaxChargeW: 3680, AllowedStepsW: []float64{0, 1380, 3680}}, site); !slices.Equal(got, []float64{0, 1380, 3680}) {
		t.Fatalf("a charger that cannot reach 6 A on three phases lost its steps: %v", got)
	}
	got := PlanningSteps(Config{MinChargeW: 1380, MaxChargeW: 5520, PhaseMode: "auto", AllowedStepsW: []float64{0, 1380, 2300, 3680}}, site)
	if !slices.Equal(got, []float64{0, 4140, 4830, 5520}) {
		t.Fatalf("one-phase-only table on a three-phase charger: %v, want the 6 A three-phase ladder", got)
	}
}

func TestChargeNowRunsOnThreePhases(t *testing.T) {
	site := SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3}
	for _, c := range []struct {
		name, cfgMode, holdMode string
		powerW                  float64
		surplus                 bool
		want                    string
	}{
		{"auto, full power", "auto", "", 11100, false, "3p"},
		{"auto, full power under surplus", "auto", "", 11100, true, "3p"},
		{"auto, below the 3p minimum", "auto", "", 1380, false, "auto"},
		{"unset, low hold under surplus", "", "", 1380, true, "auto"},
		{"unset, full power under surplus", "", "", 11100, true, ""},
		{"hold pinned to 1p", "auto", "1p", 3680, false, "1p"},
		{"loadpoint pinned to 1p", "1p", "", 3680, false, "1p"},
		{"hold auto, full power", "", "auto", 11100, false, "3p"},
	} {
		got := holdPhaseMode(ManualHold{PowerW: c.powerW, PhaseMode: c.holdMode}, Config{MinChargeW: 1380, PhaseMode: c.cfgMode}, c.surplus, site)
		if got != c.want {
			t.Errorf("%s: phase_mode %q, want %q", c.name, got, c.want)
		}
	}
}

// Field report 22 Oct 2026: a one-phase order of 3,500 W under auto, with
// 2.4 A of room left on the car's own phase. The site cap rose one step,
// past the 3,680 W split, so the split read the order as three-phase,
// credited it nothing, and cut it to the room alone: 1,600 W, then less,
// then 0, with every phase under its limit. Keeping the current order is
// never a cut; only a measured overage cuts.
func TestSplitKeepsTheCurrentOrder(t *testing.T) {
	now := time.Date(2026, 10, 22, 3, 47, 45, 0, time.UTC)
	cfg := Config{ID: "garage", DriverName: "charger", MinChargeW: 1600, MaxChargeW: 11100,
		AllowedStepsW: easeeFieldSteps, PhaseMode: "auto", SurplusOnly: true}
	for _, c := range []struct {
		name             string
		orderW           float64
		orderPhases      int
		siteCapW         float64
		roomA            float64
		wantMin, wantMax float64
	}{
		{"one-phase order, cap past the split", 3500, 1, 3730, 2.4, 3500, 3500},
		{"one-phase order, no room", 3500, 1, 3730, 0, 3500, 3500},
		// The same watts on fewer phases are a rise there: a three-phase
		// order that now resolves to one phase gets the room alone.
		{"three-phase order moving to one phase", 4140, 3, 3000, 2.4, 0, (6 + 2.4) * 230},
	} {
		samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
		ctrl := newTestController(t, []Config{cfg}, nil, samples, &fakeSender{})
		ctrl.SetSiteFuse(SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3})
		ctrl.SetFuseEVMax(func() (float64, bool) { return c.siteCapW, true })
		ctrl.SetFusePhaseRoomA(func() (float64, bool) { return c.roomA, true })
		ctrl.manager.Observe(cfg.ID, true, c.orderW, 0, true)
		ctrl.manager.SetCommanded(cfg.ID, c.orderW, "fuse_limit")
		ctrl.manager.setCommandedPhases(cfg.ID, c.orderPhases)
		ctrl.tickChoices = map[string]tickChoice{cfg.ID: {surplusOn: true, phaseMode: "auto"}}
		ctrl.freezeFuseBudget(now, samples)
		if got := ctrl.tickFuseCaps[cfg.ID]; got < c.wantMin || got > c.wantMax {
			t.Errorf("%s: cap %.0f W, want %.0f-%.0f W", c.name, got, c.wantMin, c.wantMax)
		}
	}
}
