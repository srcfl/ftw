package loadpoint

import (
	"context"
	"slices"
	"testing"
	"time"
)

// srcfl/ftw#1861, v0.155.1-beta.1: a loadpoint pinned to 3p with min = max =
// 3,800 W and steps [0, 3800]. Three phases cannot run below 6 A each,
// 4,140 W at 230 V, so the order's minimum rose above its maximum and every
// planned slot went out as 0 W with the reason "plan". The car did not charge
// all weekend. The order is now the lowest level the charger can run on its
// phases, and the planner plans with that level.
var issue1861Site = SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3}

func issue1861Loadpoint(mode string, steps []float64) Config {
	return Config{ID: "rav4", DriverName: "easee", MinChargeW: 3800, MaxChargeW: 3800, PhaseMode: mode, AllowedStepsW: steps}
}

func TestSnapChargeWMaximumBelowMinimumRunsAtMinimum(t *testing.T) {
	for _, c := range []struct {
		name  string
		steps []float64
		want  float64
	}{
		{"no table", nil, 4140},
		{"a table with the minimum", []float64{0, 3800, 4140}, 4140},
		{"a table without it", []float64{0, 3800}, 0},
	} {
		if got := SnapChargeW(3800, 4140, 3800, c.steps); got != c.want {
			t.Errorf("%s: SnapChargeW(3800, min 4140, max 3800) = %.0f W, want %.0f W", c.name, got, c.want)
		}
	}
	if got := SnapChargeW(0, 4140, 3800, nil); got != 0 {
		t.Errorf("no request snapped to %.0f W, want off", got)
	}
}

func TestPhasePinBelowThreePhaseMinimumPlansTheLowestLevel(t *testing.T) {
	for _, steps := range [][]float64{{0, 3800}, nil} {
		cfg := issue1861Loadpoint("3p", steps)
		if got := PlanningSteps(cfg, issue1861Site); !slices.Equal(got, []float64{0, 4140}) {
			t.Errorf("steps %v: planner steps %v, want [0 4140]", steps, got)
		}
		st := State{ID: cfg.ID, MinChargeW: cfg.MinChargeW, MaxChargeW: cfg.MaxChargeW, AllowedStepsW: steps}
		minW, maxW, got := PlanningLimits(st, cfg, issue1861Site)
		if minW != 4140 || maxW != 4140 || !slices.Equal(got, []float64{0, 4140}) {
			t.Errorf("steps %v: planning limits %.0f-%.0f W %v, want 4140-4140 W [0 4140]", steps, minW, maxW, got)
		}
		// A one-phase car takes 6 A on its one phase.
		st.CarPhases = 1
		if minW, maxW, got := PlanningLimits(st, cfg, issue1861Site); minW != 1380 || maxW != 1380 || !slices.Equal(got, []float64{0, 1380}) {
			t.Errorf("steps %v, one-phase car: planning limits %.0f-%.0f W %v, want 1380-1380 W [0 1380]", steps, minW, maxW, got)
		}
	}
	// One phase allowed: the planner gets the level one phase can run, 16 A.
	cfg := issue1861Loadpoint("", nil)
	st := State{ID: cfg.ID, MinChargeW: cfg.MinChargeW, MaxChargeW: cfg.MaxChargeW}
	if minW, maxW, got := PlanningLimits(st, cfg, issue1861Site); minW != 3680 || maxW != 3800 || !slices.Equal(got, []float64{0, 3680}) {
		t.Errorf("unset mode, no table: planning limits %.0f-%.0f W %v, want 3680-3800 W [0 3680]", minW, maxW, got)
	}
}

// issue1861Tick runs one controller tick and returns the order and its reason.
func issue1861Tick(t *testing.T, cfg Config, dir *Directive, hold *ManualHold) (sentCommand, string) {
	t.Helper()
	now := time.Now()
	if dir != nil {
		dir.SlotStart, dir.SlotEnd = now.Add(-time.Second), now.Add(15*time.Minute)
	}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, dir, map[string]EVSample{cfg.DriverName: {Connected: true}}, sender)
	c.SetSiteFuse(issue1861Site)
	if hold != nil {
		hold.StartedAt, hold.UpdatedAt, hold.Persistent = now, now, true
		c.SetManualHold(cfg.ID, *hold)
	}
	c.Tick(context.Background(), now)
	cmd, ok := lastSetCurrent(sender.calls)
	if !ok {
		t.Fatal("no order went out")
	}
	st, _ := c.manager.State(cfg.ID)
	return cmd, st.CommandedReason
}

func TestPhasePinBelowThreePhaseMinimumCharges(t *testing.T) {
	for _, steps := range [][]float64{{0, 3800}, nil} {
		cfg := issue1861Loadpoint("3p", steps)
		// The slot the old planner sent: 3,800 W for the whole quarter hour.
		dir := &Directive{LoadpointEnergyWh: map[string]float64{cfg.ID: 950}, LoadpointMaxPowerW: map[string]float64{cfg.ID: 3800}}
		if cmd, reason := issue1861Tick(t, cfg, dir, nil); cmd.power != 4140 || cmd.phaseMode != "3p" || reason != "plan" {
			t.Errorf("steps %v, old plan: sent %.0f W %q (%s), want 4140 W \"3p\" (plan)", steps, cmd.power, cmd.phaseMode, reason)
		}
		// The plan with the levels it now gets.
		dir = &Directive{LoadpointEnergyWh: map[string]float64{cfg.ID: 1035}, LoadpointMaxPowerW: map[string]float64{cfg.ID: 4140}}
		if cmd, reason := issue1861Tick(t, cfg, dir, nil); cmd.power != 4140 || cmd.phaseMode != "3p" || reason != "plan" {
			t.Errorf("steps %v: sent %.0f W %q (%s), want 4140 W \"3p\" (plan)", steps, cmd.power, cmd.phaseMode, reason)
		}
		// Charge now from the app asks for the loadpoint's maximum.
		hold := &ManualHold{PowerW: 3800, PhaseMode: "3p"}
		if cmd, reason := issue1861Tick(t, cfg, nil, hold); cmd.power != 4140 || cmd.phaseMode != "3p" || reason != "manual_hold" {
			t.Errorf("steps %v, Charge now: sent %.0f W %q (%s), want 4140 W \"3p\" (manual_hold)", steps, cmd.power, cmd.phaseMode, reason)
		}
	}
}

// One phase allowed and the level fits: the order runs on one phase, as
// before. The charger's 16 A on one phase holds it to 3,680 W.
func TestBelowThreePhaseMinimumUsesOnePhaseWhereAllowed(t *testing.T) {
	for _, mode := range []string{"", "auto"} {
		cfg := issue1861Loadpoint(mode, []float64{0, 3800})
		dir := &Directive{LoadpointEnergyWh: map[string]float64{cfg.ID: 950}, LoadpointMaxPowerW: map[string]float64{cfg.ID: 3800}}
		if cmd, _ := issue1861Tick(t, cfg, dir, nil); cmd.power != 3680 || cmd.phaseMode != "1p" {
			t.Errorf("%q: sent %.0f W %q, want 3680 W \"1p\"", mode, cmd.power, cmd.phaseMode)
		}
	}
	// The app names three phases for a three-phase loadpoint. A level below
	// 6 A on each goes out where the loadpoint allows one phase.
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 11000, PhaseMode: "auto", AllowedStepsW: ftwStepSet}
	if cmd, reason := issue1861Tick(t, cfg, nil, &ManualHold{PowerW: 2760, PhaseMode: "3p"}); cmd.power != 2760 || cmd.phaseMode != "auto" || reason != "manual_hold" {
		t.Errorf("auto, 2,760 W from the app: sent %.0f W %q (%s), want 2760 W \"auto\" (manual_hold)", cmd.power, cmd.phaseMode, reason)
	}
}

// No valid order: a hold below 6 A on each phase of a loadpoint pinned to
// three. A hold is a ceiling, so it is not raised; the reason says why the
// car gets nothing.
func TestHoldBelowPhaseMinimumSaysSo(t *testing.T) {
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 11000, PhaseMode: "3p", AllowedStepsW: ftwStepSet}
	cmd, reason := issue1861Tick(t, cfg, nil, &ManualHold{PowerW: 2760, PhaseMode: "3p"})
	if cmd.power != 0 || reason != "below_phase_minimum" {
		t.Errorf("3p pin, 2,760 W hold: sent %.0f W %q (%s), want 0 W (below_phase_minimum)", cmd.power, cmd.phaseMode, reason)
	}
}

// Surplus, a wake-kick and the car's own completion order the loadpoint's
// maximum or a surplus level; on three phases below 6 A each those run at
// the lowest three-phase level too. The order's current cap names 6 A.
func TestBelowThreePhaseMinimumEveryOrderRuns(t *testing.T) {
	for _, steps := range [][]float64{{0, 3800}, nil} {
		cfg := issue1861Loadpoint("3p", steps)
		cfg.SurplusOnly = true
		now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
		sender := &fakeSender{}
		c := newTestController(t, []Config{cfg}, nil, map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}, sender)
		c.SetSiteFuse(issue1861Site)
		c.SetSiteSurplusForEV(func() (float64, bool) { return 10000, true })
		c.SetPeakRemainingSurplusW(func() (float64, bool) { return 10000, true })
		c.SetFuseEVMax(meteredEVCap(c, func() float64 { return 3 }))
		for i := range 12 {
			c.Tick(context.Background(), now.Add(time.Duration(i)*5*time.Second))
		}
		cmd, _ := lastSetCurrent(sender.calls)
		st, _ := c.manager.State(cfg.ID)
		if cmd.power != 4140 || cmd.phaseMode != "3p" || st.CommandedReason != "pv_surplus" {
			t.Errorf("steps %v, 10 kW surplus: sent %.0f W %q (%s), want 4140 W \"3p\" (pv_surplus)", steps, cmd.power, cmd.phaseMode, st.CommandedReason)
		}
		if cmd.capAmpsPerPhase == nil {
			t.Errorf("steps %v: no cap_amps_per_phase", steps)
		} else if *cmd.capAmpsPerPhase < 6 {
			t.Errorf("steps %v: cap_amps_per_phase %.2f A under the 6 A order", steps, *cmd.capAmpsPerPhase)
		}
	}
}

// A plan level below the least order its phases run goes out at that
// order, on only as long as the slot's energy lasts: 2,070 Wh at 4,140 W
// is 30 min, not the 34.5 min the 3,600 W level would take.
func TestPlanLevelBelowPhaseMinimumKeepsTheSlotsEnergy(t *testing.T) {
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040, PhaseMode: "3p"}
	start := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	dir := &Directive{SlotStart: start, SlotEnd: start.Add(time.Hour),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 2070}, LoadpointMaxPowerW: map[string]float64{cfg.ID: 3600}}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(issue1861Site)
	c.Tick(context.Background(), start)
	if cmd, _ := lastSetCurrent(sender.calls); cmd.power != 4140 {
		t.Fatalf("a 3,600 W level sent %.0f W, want 4140 W", cmd.power)
	}
	samples[cfg.DriverName] = EVSample{Connected: true, PowerW: 4140, SessionWh: 2070}
	c.Tick(context.Background(), start.Add(32*time.Minute))
	if cmd, _ := lastSetCurrent(sender.calls); cmd.power != 0 {
		t.Errorf("slot energy spent at 32 min, still sent %.0f W", cmd.power)
	}
}
