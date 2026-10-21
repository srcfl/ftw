package loadpoint

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"
)

func TestTwoPhasePlanningAndFuseRoom(t *testing.T) {
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 2760, MaxChargeW: 6400, PhaseMode: "2p"}
	site := SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3}
	steps := PlanningSteps(cfg, site)
	if !slices.Equal(steps, []float64{0, 2760, 3220, 3680, 4140, 4600, 5060, 5520, 5980}) {
		t.Fatalf("two-phase steps = %v", steps)
	}
	c := newTestController(t, []Config{cfg}, nil, nil, &fakeSender{})
	c.SetSiteFuse(site)
	if got := c.phaseRoomCapW(cfg.ID, "2p", 6400, 0, 7); got != 3220 {
		t.Fatalf("7 A of room allowed %.0f W; want 3220 W on two phases", got)
	}
	if got := clampManualPower(cfg, ManualHold{PowerW: 6400, PhaseMode: "2p", MaxAmpsPerPhase: 10}, site); got != 4600 {
		t.Fatalf("10 A manual ceiling = %.0f W, want 4600 W", got)
	}
	steps = PlanningSteps(cfg, SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 1})
	if steps[1] != 1380 {
		t.Fatalf("two-phase car's one-phase minimum = %v, want 1380 W", steps)
	}
	if got := phaseMinChargeW(cfg, "1p", site); got != 1380 {
		t.Fatalf("explicit one-phase minimum = %.0f W", got)
	}
}

func TestTwoPhaseCarControlsTheOfferAndClearsForTheNextCar(t *testing.T) {
	now := time.Now()
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040, PhaseMode: "3p"}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, nil, map[string]EVSample{"easee": {Connected: true}}, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	c.manager.Observe("garage", true, 0, 0, true)
	c.manager.setCommandedPhases("garage", 3)
	c.manager.SetCarBinding("garage", CarBinding{ID: "volvo", Phases: 2, MaxW: 6400})
	if st, _ := c.manager.State("garage"); st.CommandedPhases != 0 {
		t.Fatal("the previous phase count survived a changed car limit")
	}
	c.SetManualHold("garage", ManualHold{PowerW: 6400, PhaseMode: "3p", Persistent: true})
	c.Tick(context.Background(), now)
	cmd, ok := lastSetCurrent(sender.calls)
	if !ok || cmd.phaseMode != "2p" || cmd.power > 6400 || cmd.sitePhases != 3 {
		t.Fatalf("two-phase car on three-phase site: %+v", cmd)
	}
	st, _ := c.manager.State("garage")
	if st.CommandedPhases != 2 || st.CommandedW != 5980 || st.MinChargeW != 2760 || st.MaxChargeW != 6400 {
		t.Fatalf("two-phase state = %+v", st)
	}
	effective := c.manager.Configs()[0]
	if effective.PhaseMode != "2p" || effective.MinChargeW != 2760 || effective.MaxChargeW != 6400 {
		t.Fatalf("car limits = %+v", effective)
	}
	c.manager.SetCarBinding("garage", CarBinding{})
	if got := c.manager.Configs()[0]; got.PhaseMode != "3p" || got.MaxChargeW != cfg.MaxChargeW || got.MinChargeW != cfg.MinChargeW {
		t.Fatalf("the next car inherited the previous car's limits: %+v", got)
	}
}

func TestTwoPhaseCurrentCeilingAndSinglePhaseSite(t *testing.T) {
	cmd := map[string]any{"power_w": 6400.0, "phase_mode": "2p", "voltage": 240.0, "max_amps_per_phase": 10.0}
	applyCurrentCeiling(cmd)
	if got := cmd["power_w"].(float64); math.Abs(got-4800) > 1e-6 {
		t.Fatalf("two-phase ceiling = %v", got)
	}
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 6400, PhaseMode: "2p"}
	got := runPhaseTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 1}, 6000)
	if got.phaseMode != "1p" || got.power > 3680 {
		t.Fatalf("single-phase installation = %+v", got)
	}
}
