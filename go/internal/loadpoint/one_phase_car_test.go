package loadpoint

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"testing"
	"time"
)

func TestPhaseVerdict(t *testing.T) {
	base := [3]float64{1.6, 1.7, 2.3}
	plus := func(a, b, c float64) [3]float64 { return [3]float64{base[0] + a, base[1] + b, base[2] + c} }
	for _, c := range []struct {
		name  string
		amps  [3]float64
		drawA float64
		want  int
	}{
		{"one-phase car on L1", plus(7, 0, 0), 7, 1},
		{"one-phase car on L3", plus(0, 0.3, 7), 7, 3},
		{"car limited below the order", plus(5, 0, 0), 5, 1},
		{"three-phase car", plus(7, 7, 7), 21, -1},
		{"two-phase car", plus(7, 7, 0), 14, 0},
		{"kettle while the car draws nothing", plus(9, 0, 0), 0, 0},
		{"car and kettle on one phase", plus(16, 0, 0), 7, 0},
		{"not applied yet", plus(0, 0, 0), 0, 0},
	} {
		if got := phaseVerdict(c.amps, base, 7, c.drawA); got != c.want {
			t.Errorf("%s: verdict %d, want %d", c.name, got, c.want)
		}
	}
}

// The meter marks the session's car: a three-phase order rises by 7 A a
// phase, and 40 s later only L1 rises by 7 A while the charger reads 1.6 kW.
// A three-phase verdict clears it, and the next car starts unknown.
func TestOnePhaseCarMarkedFromTheMeter(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee", MinChargeW: 1600, MaxChargeW: 11100}})
	m.Observe("garage", true, 0, 0, true)
	start := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)
	base := [3]float64{1.6, 1.7, 2.3}
	order := func(w float64) { m.SetCommanded("garage", w, "plan"); m.setCommandedPhases("garage", 3) }
	run := func(from, to time.Duration, amps func(time.Duration) [3]float64, drawW float64) {
		for d := from; d <= to; d += 5 * time.Second {
			at := start.Add(d)
			m.observePhases("garage", SitePhases{A: amps(d), At: at}, drawW/230, 230, time.Minute, at)
		}
	}
	state := func() State { st, _ := m.State("garage"); return st }
	still := func(time.Duration) [3]float64 { return base }

	order(0)
	run(0, 10*time.Second, still, 0)
	order(4830)
	onL1 := func(d time.Duration) [3]float64 {
		if d < 55*time.Second {
			return base
		}
		return [3]float64{base[0] + 7, base[1], base[2]}
	}
	run(15*time.Second, 80*time.Second, onL1, 1610)
	if st := state(); st.CarPhases != 0 {
		t.Fatal("marked before the meter showed the rise for 30 s")
	}
	run(85*time.Second, 90*time.Second, onL1, 1610)
	if st := state(); st.CarPhases != 1 || st.CarPhase != 1 {
		t.Fatalf("after 35 s on L1 alone: car phases %d on L%d", st.CarPhases, st.CarPhase)
	}
	// A rise that shows on all three phases, with the charger's draw rising
	// by their sum, is a three-phase car: here one marked by mistake.
	threePhase := func(a float64) func(time.Duration) [3]float64 {
		return func(time.Duration) [3]float64 { return [3]float64{base[0] + a, base[1] + a, base[2] + a} }
	}
	run(95*time.Second, 120*time.Second, threePhase(7), 3*1610)
	order(11040)
	allThree := func(d time.Duration) [3]float64 {
		if d < 160*time.Second {
			return threePhase(7)(d)
		}
		return threePhase(16)(d)
	}
	run(125*time.Second, 200*time.Second, allThree, 11040)
	if st := state(); st.CarPhases != 0 {
		t.Fatalf("a rise on all three phases left the mark on L%d", st.CarPhase)
	}
	order(4830)
	run(205*time.Second, 210*time.Second, still, 0)
	order(11040)
	run(215*time.Second, 300*time.Second, onL1, 1610)
	m.Observe("garage", false, 0, 0, true)
	m.Observe("garage", true, 0, 0, true)
	if st := state(); st.CarPhases != 0 {
		t.Fatal("the next car would inherit the mark")
	}
}

// Reviewer's P4: a one-phase car on the field table. Without the mark the
// planner got [0, 4800] and the car drew 1.6 kW of a 4.8 kW order. Marked,
// the planner gets the car's one-phase levels up to 3.7 kW.
func TestOnePhaseCarPlansItsOwnLevels(t *testing.T) {
	site := SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3}
	cfg := Config{MinChargeW: 1600, MaxChargeW: 11100, PhaseMode: "auto", AllowedStepsW: easeeFieldSteps}
	st := State{MinChargeW: 1600, MaxChargeW: 11100, CarMaxW: 3700}
	_, maxW, steps := LimitPower(st, OnePhaseCarSteps(cfg, site))
	if maxW < 3680 || maxW > 3700 || steps[len(steps)-1] != 3680 || !slices.Contains(steps, 1380) {
		t.Fatalf("one-phase car plan: max %.0f W, steps %v", maxW, steps)
	}
	if got := OnePhaseCarMaxW(Config{MaxChargeW: 11100}, SiteFuse{MaxAmps: 10, Voltage: 230, PhaseCnt: 3}); got != 2300 {
		t.Fatalf("a 10 A fuse holds a one-phase car to %.0f W, want 2300 W", got)
	}
}

// Marked, a plan slot at the car's 3.7 kW goes out as a three-phase order
// of 16 A a phase, and the command says the car should draw a third of it.
func TestOnePhaseCarOrderNamesItsCurrent(t *testing.T) {
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1600, MaxChargeW: 11100, PhaseMode: "auto", AllowedStepsW: easeeFieldSteps}
	slot := time.Now().Add(-time.Second)
	dir := &Directive{SlotStart: slot, SlotEnd: slot.Add(time.Hour), LoadpointEnergyWh: map[string]float64{cfg.ID: 3680}}
	var payload map[string]any
	send := func(_ context.Context, _ string, p []byte) error { return json.Unmarshal(p, &payload) }
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	c := NewController(NewManager(), PlanFunc(func(time.Time) (Directive, bool) { return *dir, true }),
		TelemetryFunc(func(d string) (EVSample, bool) { s, ok := samples[d]; return s, ok }), send)
	c.manager.Load([]Config{cfg})
	c.SetSiteFuse(SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3})
	c.manager.Observe(cfg.ID, true, 0, 0, true)
	c.manager.SetSchedule(cfg.ID, Schedule{SoC: .8})
	c.manager.byID[cfg.ID].carPhases.phase = 1
	c.Tick(context.Background(), time.Now())
	power, _ := payload["power_w"].(float64)
	expected, _ := payload["expected_w"].(float64)
	if payload["phase_mode"] != "3p" || power != 11100 || math.Abs(expected-3700) > 1 {
		t.Fatalf("sent %v", payload)
	}
}

// phaseRig feeds a session's meter and charger readings every 5 s.
type phaseRig struct {
	m     *Manager
	start time.Time
}

func newPhaseRig() *phaseRig {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040}})
	m.Observe("garage", true, 0, 0, true)
	return &phaseRig{m: m, start: time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)}
}

func (r *phaseRig) order(w float64) {
	r.m.SetCommanded("garage", w, "plan")
	r.m.setCommandedPhases("garage", 3)
}

func (r *phaseRig) run(from, to time.Duration, amps func(time.Duration) [3]float64, drawW func(time.Duration) float64) {
	for d := from; d <= to; d += 5 * time.Second {
		at := r.start.Add(d)
		r.m.observePhases("garage", SitePhases{A: amps(d), At: at}, drawW(d)/230, 230, time.Minute, at)
	}
}

func (r *phaseRig) carPhases() int { st, _ := r.m.State("garage"); return st.CarPhases }

// Review M1: the charger's draw is compared as a change. A first test spoilt
// by 2 A on L2 leaves a car already drawing 6 A on L1; its next 2 A rise,
// on L1 alone with the charger's draw up by 2 A, marks it.
func TestOnePhaseCarMarkedOnALaterRise(t *testing.T) {
	if v := phaseVerdict([3]float64{12, 2, 2}, [3]float64{8, 2, 2}, 4, 4); v != 1 {
		t.Fatalf("a 4 A rise on L1 with the car up by 4 A: verdict %d", v)
	}
	r := newPhaseRig()
	still := func(a [3]float64) func(time.Duration) [3]float64 { return func(time.Duration) [3]float64 { return a } }
	draw := func(w float64) func(time.Duration) float64 { return func(time.Duration) float64 { return w } }
	r.order(0)
	r.run(0, 10*time.Second, still([3]float64{2, 2, 2}), draw(0))
	r.order(4140)
	r.run(15*time.Second, 35*time.Second, still([3]float64{2, 2, 2}), draw(0))
	r.run(40*time.Second, 200*time.Second, still([3]float64{8, 4, 2}), draw(1380))
	if r.carPhases() != 0 {
		t.Fatal("marked through 2 A on L2")
	}
	r.order(8 * 690)
	r.run(205*time.Second, 230*time.Second, still([3]float64{8, 4, 2}), draw(1380))
	r.run(235*time.Second, 300*time.Second, still([3]float64{10, 4, 2}), draw(1840))
	if r.carPhases() != 1 {
		t.Fatal("a 2 A rise on L1 alone, followed by the charger, did not mark the car")
	}
}

// Review M2: a heat pump starting at 2 A a phase during a 2 A rise moves all
// three phases, but the charger's draw rose by 2 A, not 8: the mark stays.
func TestHeatPumpDoesNotClearAOnePhaseCar(t *testing.T) {
	r := newPhaseRig()
	r.m.byID["garage"].carPhases = carPhases{orderA: 6, phase: 1}
	r.order(6 * 690)
	base := [3]float64{8, 2, 2}
	r.run(0, 10*time.Second, func(time.Duration) [3]float64 { return base }, func(time.Duration) float64 { return 1380 })
	r.order(8 * 690)
	r.run(15*time.Second, 200*time.Second, func(d time.Duration) [3]float64 {
		a := base
		if d >= 20*time.Second {
			a = [3]float64{a[0] + 2, a[1] + 2, a[2] + 2}
		}
		if d >= 60*time.Second {
			a[0] += 2
		}
		return a
	}, func(d time.Duration) float64 {
		if d >= 60*time.Second {
			return 8 * 230
		}
		return 6 * 230
	})
	if r.carPhases() != 1 {
		t.Fatal("a heat pump starting during a rise cleared the one-phase car")
	}
}

// Review M3: after a Core restart the order is out and the car draws before
// the first reading. With no baseline, a one-phase car still shows its whole
// draw on one phase and less than a third on the others; a three-phase car
// never does.
func TestOnePhaseCarMarkedAfterARestart(t *testing.T) {
	for _, c := range []struct {
		name  string
		amps  [3]float64
		drawW float64
		want  int
	}{
		{"one-phase car at 16 A on L1, house 3/1/1 A", [3]float64{19, 1, 1}, 3680, 1},
		{"three-phase car at 16 A, house 3/1/1 A", [3]float64{19, 17, 17}, 11040, 0},
		{"two-phase car at 16 A, house 3/1/1 A", [3]float64{19, 17, 1}, 7360, 0},
	} {
		r := newPhaseRig()
		r.order(11040)
		r.run(0, 60*time.Second, func(time.Duration) [3]float64 { return c.amps }, func(time.Duration) float64 { return c.drawW })
		if got := r.carPhases(); got != c.want {
			t.Errorf("%s: car phases %d, want %d", c.name, got, c.want)
		}
	}
}

// Device-simulator rig s14 on 2e847104: a three-phase charger with the
// settings page's minimum, 4,140 W. Once the car was marked one-phase the
// planner got its one-phase levels with that minimum, dropped every level
// below it and planned nothing. The car's levels are the charger's
// three-phase steps divided by three, up to its one-phase maximum.
func TestOnePhaseCarPlanningLimits(t *testing.T) {
	site := SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3}
	planned := func(minW, maxW float64, steps []float64) (n int) {
		for _, w := range steps {
			if w > 0 && w >= minW && w <= maxW {
				n++
			}
		}
		return n
	}
	for _, c := range []struct {
		name              string
		cfg               Config
		carMaxW           float64
		wantMin, wantMax  float64
		wantPlannedLevels int
	}{
		{"settings page's charger", Config{MinChargeW: 4140, MaxChargeW: 11000, PhaseMode: "auto"}, 0, 1380, 3450, 10},
		{"three-phase table", Config{MinChargeW: 4140, MaxChargeW: 11040, PhaseMode: "3p", AllowedStepsW: []float64{0, 4140, 6900, 11040}}, 0, 1380, 3680, 3},
		{"field table", Config{MinChargeW: 1600, MaxChargeW: 11100, PhaseMode: "auto", AllowedStepsW: easeeFieldSteps}, 0, 1600, 10400.0 / 3, 9},
		{"minimum 4,800 W", Config{MinChargeW: 4800, MaxChargeW: 11000, PhaseMode: "auto"}, 0, 1610, 3450, 9},
		{"car's own maximum", Config{MinChargeW: 4140, MaxChargeW: 11000, PhaseMode: "auto"}, 3000, 1380, 3000, 8},
	} {
		st := State{MinChargeW: c.cfg.MinChargeW, MaxChargeW: c.cfg.MaxChargeW, AllowedStepsW: c.cfg.AllowedStepsW, CarMaxW: c.carMaxW, CarPhases: 1}
		minW, maxW, steps := PlanningLimits(st, c.cfg, site)
		if math.Abs(minW-c.wantMin) > .5 || math.Abs(maxW-c.wantMax) > .5 || planned(minW, maxW, steps) != c.wantPlannedLevels {
			t.Errorf("%s: min %.0f max %.0f, %d levels the planner keeps (%v); want %.0f, %.0f, %d",
				c.name, minW, maxW, planned(minW, maxW, steps), steps, c.wantMin, c.wantMax, c.wantPlannedLevels)
		}
	}
	// An unmarked car keeps the charger's limits.
	cfg := Config{MinChargeW: 4140, MaxChargeW: 11000, PhaseMode: "auto"}
	if minW, maxW, _ := PlanningLimits(State{MinChargeW: 4140, MaxChargeW: 11000}, cfg, site); minW != 4140 || maxW != 11000 {
		t.Fatalf("unmarked car: %.0f-%.0f W", minW, maxW)
	}
}

// Review of #1806: every level the planner keeps for a one-phase car is a
// third of an order the charger's table has. A plan slot at that level goes
// out as that order, within the charger and the fuse, and the car draws
// what the plan counts. A third of the charger's minimum was not: on the
// field table 1,380 W became 4,140 W, which the table lacks, and 0 W went
// out; coarse tables drew up to 30 % less than the plan.
func TestOnePhaseCarLevelsAreOrders(t *testing.T) {
	for _, ch := range []struct {
		name       string
		minW, maxW float64
		steps      []float64
	}{
		{"settings page", 4140, 11000, nil},
		{"field table", 1600, 11100, easeeFieldSteps},
		{"minimum 4,800 W", 4800, 11000, nil},
		{"coarse table", 4800, 11000, []float64{0, 4800, 7200, 11000}},
		{"three-phase table", 4140, 11040, []float64{0, 4140, 6900, 11040}},
	} {
		for _, fuseA := range []float64{16, 25} {
			site := SiteFuse{MaxAmps: fuseA, Voltage: 230, PhaseCnt: 3}
			cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: ch.minW, MaxChargeW: ch.maxW, PhaseMode: "auto", AllowedStepsW: ch.steps}
			st := State{MinChargeW: ch.minW, MaxChargeW: ch.maxW, AllowedStepsW: ch.steps, CarPhases: 1}
			minW, maxW, steps := PlanningLimits(st, cfg, site)
			kept := 0
			for _, level := range steps {
				if level <= 0 || level < minW || level > maxW {
					continue // The planner drops it (mpc normalizedSteps).
				}
				kept++
				power, expected := onePhaseCarOrder(cfg, site, level)
				if power <= 0 || power > ch.maxW || power/690 > fuseA+1e-6 || math.Abs(expected-level) > level/100 {
					t.Errorf("%s, %g A: a plan slot at %.0f W went out as %.0f W, the car drawing %.0f W",
						ch.name, fuseA, level, power, expected)
				}
			}
			if kept == 0 {
				t.Errorf("%s, %g A: the planner keeps no level of %v (%.0f-%.0f W)", ch.name, fuseA, steps, minW, maxW)
			}
		}
	}
}

// onePhaseCarOrder is the order a marked one-phase car's plan slot at
// levelW goes out as, with the plan's peak, and the draw it names.
func onePhaseCarOrder(cfg Config, site SiteFuse, levelW float64) (powerW, expectedW float64) {
	slot := time.Now().Add(-time.Second)
	dir := &Directive{SlotStart: slot, SlotEnd: slot.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: levelW / 4}, LoadpointMaxPowerW: map[string]float64{cfg.ID: levelW}}
	var payload map[string]any
	send := func(_ context.Context, _ string, p []byte) error { return json.Unmarshal(p, &payload) }
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	c := NewController(NewManager(), PlanFunc(func(time.Time) (Directive, bool) { return *dir, true }),
		TelemetryFunc(func(d string) (EVSample, bool) { s, ok := samples[d]; return s, ok }), send)
	c.manager.Load([]Config{cfg})
	c.SetSiteFuse(site)
	c.manager.Observe(cfg.ID, true, 0, 0, true)
	c.manager.SetSchedule(cfg.ID, Schedule{SoC: .8})
	c.manager.byID[cfg.ID].carPhases.phase = 1
	c.Tick(context.Background(), time.Now())
	powerW, _ = payload["power_w"].(float64)
	expectedW, _ = payload["expected_w"].(float64)
	return powerW, expectedW
}
