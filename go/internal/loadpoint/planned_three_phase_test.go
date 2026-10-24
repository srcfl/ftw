package loadpoint

import (
	"context"
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
// pinned to one phase or its charger cannot reach 6 A on three.
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
	// A table with nothing at 6 A on three phases is a one-phase charger.
	oneOnly := []float64{0, 1380, 2300, 3680}
	if got := PlanningSteps(Config{MinChargeW: 1380, MaxChargeW: 5520, PhaseMode: "auto", AllowedStepsW: oneOnly}, site); !slices.Equal(got, oneOnly) {
		t.Fatalf("one-phase-only table: %v, want it kept", got)
	}
	// So is a maximum below 6 A on three phases, with no table.
	if got := PlanningSteps(Config{MinChargeW: 1380, MaxChargeW: 3680}, site); !slices.Equal(got, []float64{0, 1380, 1610, 1840, 2070, 2300, 2530, 2760, 2990, 3220, 3450, 3680}) {
		t.Fatalf("16 A one-phase charger without a table: %v, want its one-phase ladder", got)
	}
}

// What goes out for a plan slot, by the charger's table and phase mode.
// A one-phase-only table runs on one phase (master sent auto and charged);
// unset is auto, raised to the three-phase minimum on a charger that can.
func TestPlannedOrderFollowsTheChargersPhases(t *testing.T) {
	site := SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3}
	for _, c := range []struct {
		name, mode string
		steps      []float64
		maxW, wh   float64
		wantW      float64
		wantMode   string
	}{
		{"one-phase-only table, auto", "auto", []float64{0, 1380, 2300, 3680}, 5520, 4140, 3680, "1p"},
		{"one-phase-only table, unset", "", []float64{0, 1380, 2300, 3680}, 5520, 2300, 2300, "1p"},
		{"unset, low slot", "", []float64{0, 1380, 2300, 3680, 4140, 6900, 11000}, 11000, 2300, 4140, "3p"},
		{"auto, low slot", "auto", []float64{0, 1380, 2300, 3680, 4140, 6900, 11000}, 11000, 2300, 4140, "3p"},
	} {
		cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: c.maxW, PhaseMode: c.mode, AllowedStepsW: c.steps}
		cmd := runPhaseTick(t, cfg, site, c.wh)
		if cmd.power != c.wantW || cmd.phaseMode != c.wantMode {
			t.Errorf("%s: plan %.0f Wh/h sent %.0f W %q, want %.0f W %q", c.name, c.wh, cmd.power, cmd.phaseMode, c.wantW, c.wantMode)
		}
	}
}

// "Only spare solar" with a goal stays a solar charge: 2.5 kW of surplus on
// a low-sun day goes out on one phase, not as a three-phase order an Easee
// turns into 3.3 A and stops.
func TestSolarOnlyWithGoalKeepsOnePhaseSurplus(t *testing.T) {
	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 11000, PhaseMode: "auto", SurplusOnly: true,
		AllowedStepsW: []float64{0, 1380, 1840, 2300, 2760, 3220, 3680, 4140, 6900, 11000}}
	sender := &fakeSender{}
	dir := &Directive{SlotStart: base.Add(-time.Second), SlotEnd: base.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{cfg.ID: 0}}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3})
	c.manager.SetSchedule(cfg.ID, Schedule{SoC: 0.8, TimeOfDayMinUTC: 7 * 60, Recurring: true})
	c.SetSiteSurplusForEV(func() (float64, bool) { return 2500, true })
	c.SetPeakRemainingSurplusW(func() (float64, bool) { return 3000, true })
	for i := range 3 {
		c.Tick(context.Background(), base.Add(time.Duration(i)*5*time.Second))
	}
	cmd, _ := lastSetCurrent(sender.calls)
	if cmd.power <= 0 || cmd.power > 2500 || (cmd.phaseMode != "1p" && cmd.phaseMode != "auto") {
		t.Fatalf("2.5 kW of surplus sent %.0f W %q, want a one-phase order", cmd.power, cmd.phaseMode)
	}
}

func TestChargeNowRunsOnThreePhases(t *testing.T) {
	site := SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3}
	for _, c := range []struct {
		name, cfgMode, holdMode string
		powerW, maxW            float64
		want                    string
	}{
		{"auto, full power", "auto", "", 11100, 11100, "3p"},
		{"auto, below the 3p minimum", "auto", "", 1380, 11100, "auto"},
		{"unset, low hold", "", "", 1380, 11100, "auto"},
		{"unset, full power", "", "", 11100, 11100, "3p"},
		{"hold pinned to 1p", "auto", "1p", 3680, 11100, "1p"},
		{"loadpoint pinned to 1p", "1p", "", 3680, 11100, "1p"},
		{"hold auto, full power", "", "auto", 11100, 11100, "3p"},
		{"one-phase charger, unset", "", "", 3680, 3680, "1p"},
		// A client's 3p is not a pin on a charger that cannot run it.
		{"one-phase charger, client sends 3p", "", "3p", 3680, 3680, "1p"},
		{"one-phase charger on auto, client sends 3p", "auto", "3p", 3680, 3680, "1p"},
		{"one-phase charger pinned 3p", "3p", "3p", 3680, 3680, "3p"},
	} {
		got := holdPhaseMode(ManualHold{PowerW: c.powerW, PhaseMode: c.holdMode}, Config{MinChargeW: 1380, MaxChargeW: c.maxW, PhaseMode: c.cfgMode}, site)
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
		mode             string
		orderW           float64
		orderPhases      int
		siteCapW         float64
		roomA            float64
		wantMin, wantMax float64
	}{
		{"one-phase order, cap past the split", "auto", 3500, 1, 3730, 2.4, 3500, 3500},
		{"one-phase order, no room", "auto", 3500, 1, 3730, 0, 3500, 3500},
		// The site cap is the measured limit; keeping never beats it.
		{"one-phase order after a measured cut", "auto", 3500, 1, 2000, -3, 0, 2000},
		{"three-phase order after a measured cut", "3p", 9700, 3, 2000, -3, 0, 2000},
		// The same watts on other phases are a rise on some of them: a
		// three-phase order that now resolves to one phase gets the room
		// alone, and 5,000 W moved from one phase to three adds 7.2 A to
		// two phases that carried none of it.
		{"three-phase order moving to one phase", "auto", 4140, 3, 3000, 2.4, 0, (6 + 2.4) * 230},
		{"one-phase order moving to three phases", "3p", 5000, 1, 20000, 2, 0, 2 * 690},
	} {
		samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
		ctrl := newTestController(t, []Config{cfg}, nil, samples, &fakeSender{})
		ctrl.SetSiteFuse(SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3})
		ctrl.SetFuseEVMax(func() (float64, bool) { return c.siteCapW, true })
		ctrl.SetFusePhaseRoomA(func() (float64, bool) { return c.roomA, true })
		ctrl.manager.Observe(cfg.ID, true, c.orderW, 0, true)
		ctrl.manager.SetCommanded(cfg.ID, c.orderW, "fuse_limit")
		ctrl.manager.setCommandedPhases(cfg.ID, c.orderPhases)
		ctrl.tickChoices = map[string]tickChoice{cfg.ID: {surplusOn: true, phaseMode: c.mode}}
		ctrl.freezeFuseBudget(now, samples)
		if got := ctrl.tickFuseCaps[cfg.ID]; got < c.wantMin || got > c.wantMax {
			t.Errorf("%s: cap %.0f W, want %.0f-%.0f W", c.name, got, c.wantMin, c.wantMax)
		}
	}
}

// Field matrix, hold3p: an app sends Charge now as 3p for an auto
// loadpoint. On a 32 A one-phase charger whose table stops at 3,680 W, a
// 3p order has no step and went out as 0 W. Core runs it on one phase.
func TestChargeNowOnAOnePhaseChargerIgnoresAClients3p(t *testing.T) {
	base := time.Date(2026, 10, 22, 20, 0, 0, 0, time.UTC)
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 7360, PhaseMode: "auto",
		AllowedStepsW: []float64{0, 1380, 2300, 3680}}
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetManualHold(cfg.ID, ManualHold{PowerW: cfg.MaxChargeW, PhaseMode: "3p", Persistent: true, StartedAt: base, UpdatedAt: base})
	for i := range 3 {
		c.Tick(context.Background(), base.Add(time.Duration(i)*5*time.Second))
	}
	cmd, ok := lastSetCurrent(sender.calls)
	if !ok || cmd.power != 3680 || cmd.phaseMode != "1p" {
		t.Fatalf("Charge now sent %.0f W %q, want 3680 W 1p", cmd.power, cmd.phaseMode)
	}
}
