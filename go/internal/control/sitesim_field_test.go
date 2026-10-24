package control

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/sitesim"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The field matrix runs Core's EV charging and fuse protection in the closed
// loop against the settings sites run: each field report's fuse, phase mode,
// step table, charger, car, house and battery, and the axes around them. A
// beta must pass it, not only our defaults (fixed 3p, no step table, a
// charger that answers at once or exactly when it says).
//
// Each case runs in a synctest bubble, so the telemetry store's wall clock is
// the plant's clock: command evidence, the API's verdict and live Home use
// are timed as on a box.

// fieldTiming is a charger's response: how long an order takes to reach the
// car and the power_max_age_s its driver declares, zero for none. Easee Cloud
// applies an order in 30-60 s and declares 180 s.
type fieldTiming struct {
	name     string
	delay    time.Duration
	declared time.Duration
	cloud    bool // easee_cloud.lua: splits auto at the fuse, holds a phase count 90 s, reports 10 s late
}

var (
	timingFast       = fieldTiming{"fast2s", 2 * time.Second, 0, false}
	timingEasee      = fieldTiming{"easee40s", 40 * time.Second, 3 * time.Minute, true}
	timingSlowCloud  = fieldTiming{"easee120s", 2 * time.Minute, 3 * time.Minute, true}
	timingUndeclared = fieldTiming{"cloud60s", time.Minute, 0, true}
	fieldTimings     = []fieldTiming{timingFast, timingEasee, timingSlowCloud, timingUndeclared}
)

// fieldUse is what the household asked for.
type fieldUse string

const (
	usePlanned fieldUse = "planned"   // a ready-by goal; the planner's directive per slot
	useNow     fieldUse = "chargenow" // Charge now from the app: 3p, or 1p on a 1p charger
	useSurplus fieldUse = "surplus"   // surplus_only while export steps up and down
	usePause   fieldUse = "pause"     // Charge now, Pause for 25 s, then Start
	useRestart fieldUse = "restart"   // a planned charge across a 20 s Core restart
	usePhaseUp fieldUse = "1p-to-3p"  // a held 1p order changed to 3p after 5 min
)

// fieldCase is one site and one request. The name is stable: the known rows
// refer to it.
type fieldCase struct {
	report    string  // the configuration it comes from
	fuseA     float64 // per phase, three phases at 230 V
	mode      string  // loadpoint phase_mode
	steps     string  // step table: none, mixed (1p and 3p levels), 3p, 1p, or a report's own
	timing    fieldTiming
	carPhases int
	use       fieldUse
	house     [3]float64 // A per phase
	houseName string
	battery   bool          // the plan charges the home battery meanwhile
	every     time.Duration // control_interval_s; zero: 2 s
	lp        func(*loadpoint.Config)
	car       func(*sitesim.Car)

	charger   string        // the charger itself when not what the phase mode says: "1p32A"
	planW     float64       // > 0: the plan asks this many W each slot, as Wh only
	needW     float64       // > 0: the goal asks this many W on average
	goal      bool          // surplus-only with a ready-by goal
	exportW   []float64     // surplus: export each quarter hour
	houseStep time.Duration // > 0: L1 +8 A for 10 min from this time into the run
	warm      bool          // the fuse starts warm: the house has carried its load all evening
}

func (c fieldCase) key() string {
	mode := c.mode
	if mode == "" {
		mode = "empty"
	}
	name := fmt.Sprintf("%gA/%s/%s/%s/car%dp/%s/%s", c.fuseA, mode, c.steps, c.timing.name, c.carPhases, c.houseName, c.use)
	export := ""
	if len(c.exportW) > 0 {
		export = fmt.Sprintf("export%gW", c.exportW[0])
	}
	for _, v := range []struct {
		on   bool
		text string
	}{
		{c.battery, "battery"}, {c.charger != "", "charger" + c.charger}, {c.planW > 0, fmt.Sprintf("plan%gW", c.planW)}, {c.needW > 0, fmt.Sprintf("need%gW", c.needW)},
		{c.goal, "goal"}, {export != "", export}, {c.houseStep > 0, "L1step"},
	} {
		if v.on {
			name += "+" + v.text
		}
	}
	return name
}

func (c fieldCase) name() string { return c.report + "/" + c.key() }

// Step tables as sites have them. mixed is Easee's whole amps on one phase,
// then on three (srcfl/ftw#1774's tests, the rig's s5s).
func stepTable(name string) (steps []float64, minW, maxW float64) {
	amps := func(phases float64, from, to float64) []float64 {
		var out []float64
		for a := from; a <= to; a++ {
			out = append(out, a*230*phases)
		}
		return out
	}
	switch name {
	case "mixed":
		return slices.Concat([]float64{0}, amps(1, 6, 16), amps(3, 6, 16)), 1380, 11040
	case "3p":
		return slices.Concat([]float64{0}, amps(3, 6, 16)), 4140, 11040
	case "1p":
		return slices.Concat([]float64{0}, amps(1, 6, 16)), 1380, 3680
	case "1p-3": // three one-phase levels and a maximum above them
		return []float64{0, 1380, 2300, 3680}, 1380, 5520
	}
	return nil, 4140, 11000 // What the settings page writes for a new charger.
}

func fieldLoadpoint(c fieldCase) loadpoint.Config {
	steps, minW, maxW := stepTable(c.steps)
	if c.mode == "1p" && c.steps == "none" {
		minW, maxW = 1380, 3680
	}
	cfg := loadpoint.Config{ID: "garage", DriverName: "evse", PhaseMode: c.mode, AllowedStepsW: steps,
		MinChargeW: minW, MaxChargeW: maxW, VehicleCapacityWh: 60000, SurplusOnly: c.use == useSurplus}
	if c.lp != nil {
		c.lp(&cfg)
	}
	return cfg
}

var (
	houseBalanced = [3]float64{3, 3, 3}
	houseL1       = [3]float64{7, 1, 1} // most of the house on one phase, the phase a 1p car uses
)

// Field reports. Settings only: no names, hosts or credentials.

// helpReport is the help report of 22 Oct 2026, v0.155.0-beta.1: 3 × 20 A,
// Easee Cloud, phase_mode auto, a step table with one-phase levels
// 1,600-3,800 W and three-phase 4,800-11,100 W, a deadline goal,
// control_interval_s 5. Pixii is the site meter; its battery sat at its floor
// all night, so it is left out. The house drew about 2 A a phase; the car
// took 3.1-3.4 kW on one phase and was cut to 0 W for the fuse every few
// minutes.
func helpReport(use fieldUse, tm fieldTiming) fieldCase {
	return fieldCase{report: "help1022", fuseA: 20, mode: "auto", steps: "own", timing: tm, carPhases: 3, use: use,
		house: [3]float64{2, 2, 2}, houseName: "2A", every: 5 * time.Second,
		lp: func(c *loadpoint.Config) {
			c.AllowedStepsW = []float64{0, 1600, 1900, 2100, 2400, 2600, 2800, 3100, 3300, 3500, 3800,
				4800, 5500, 6200, 6900, 7600, 8300, 9000, 9700, 10400, 11100}
			c.MinChargeW, c.MaxChargeW, c.VehicleCapacityWh = 1600, 11100, 84000
		}}
}

// pr1763Site is the site in srcfl/ftw#1763's comments: 3 × 25 A, Easee Cloud
// and Pixii, the battery charging from the plan while the car charges,
// offers to 15 A of which the car takes about 12 A, pause and restart.
func pr1763Site(use fieldUse, tm fieldTiming) fieldCase {
	return fieldCase{report: "pr1763", fuseA: 25, mode: "3p", steps: "3p-15A", timing: tm, carPhases: 3, use: use,
		house: [3]float64{6, 2, 2}, houseName: "6-2-2", battery: true,
		lp: func(c *loadpoint.Config) {
			steps, minW, _ := stepTable("3p")
			c.AllowedStepsW, c.MinChargeW, c.MaxChargeW = steps[:len(steps)-1], minW, 10350
		},
		car: func(car *sitesim.Car) { car.MaxW = 8300 }}
}

// homeBox is the home box: 3 × 16 A, Easee Cloud, phase_mode unset, no step
// table, 4,140-11,000 W. Its own goal is 80 % at 07:00; like every planned
// case here it plans the matrix's need instead.
func homeBox(use fieldUse, tm fieldTiming) fieldCase {
	return fieldCase{report: "homebox", fuseA: 16, mode: "", steps: "none", timing: tm, carPhases: 3, use: use,
		house: houseBalanced, houseName: "balanced",
		lp: func(c *loadpoint.Config) { c.VehicleCapacityWh = 76000 }}
}

// fieldCases crosses the axes. The field reports run with every charger
// timing. The sweep crosses fuse, phase mode, step table, car and house for
// planned charging and Charge now with an Easee Cloud charger; the timing
// sweep runs every request with every timing on fewer sites. Pruned: a 1p
// phase mode is a one-phase charger, so the car's phases and a 3p table do
// not apply; 20 A runs as the help report's site, between 16 and 25 A.
func fieldCases() []fieldCase {
	var out []fieldCase
	seen := map[string]bool{}
	add := func(c fieldCase) {
		if !seen[c.key()] {
			seen[c.key()] = true
			out = append(out, c)
		}
	}
	for _, use := range []fieldUse{usePlanned, useNow, usePause, useRestart} {
		for _, tm := range fieldTimings {
			add(helpReport(use, tm))
			add(pr1763Site(use, tm))
			add(homeBox(use, tm))
		}
	}
	type modeSteps struct{ mode, steps string }
	all := []modeSteps{{"", "none"}, {"", "mixed"}, {"", "3p"}, {"auto", "none"}, {"auto", "mixed"}, {"auto", "3p"},
		{"3p", "none"}, {"3p", "mixed"}, {"3p", "3p"}, {"1p", "none"}, {"1p", "1p"}}
	houses := []struct {
		name string
		a    [3]float64
	}{{"balanced", houseBalanced}, {"L1", houseL1}}
	for _, use := range []fieldUse{usePlanned, useNow} {
		for _, fuse := range []float64{16, 25} {
			for _, h := range houses {
				for _, ms := range all {
					for _, car := range []int{3, 1} {
						if ms.mode == "1p" && car == 1 {
							continue
						}
						add(fieldCase{report: "sweep", fuseA: fuse, mode: ms.mode, steps: ms.steps, timing: timingEasee,
							carPhases: car, use: use, house: h.a, houseName: h.name})
					}
				}
			}
		}
	}
	for _, use := range []fieldUse{usePlanned, useNow, useSurplus, usePause, useRestart} {
		for _, fuse := range []float64{16, 25} {
			for _, ms := range []modeSteps{{"", "none"}, {"auto", "mixed"}, {"3p", "3p"}} {
				for _, tm := range fieldTimings {
					add(fieldCase{report: "timing", fuseA: fuse, mode: ms.mode, steps: ms.steps, timing: tm,
						carPhases: 3, use: use, house: houseL1, houseName: "L1"})
				}
			}
		}
	}
	for _, c := range probeCases() {
		add(c)
	}
	return out
}

// probeCases are the edges srcfl/ftw#1799's review named, and the one-phase
// car that fix D is for.
func probeCases() []fieldCase {
	var out []fieldCase
	two := []fieldTiming{timingFast, timingEasee}
	for _, tm := range two {
		for _, use := range []fieldUse{usePlanned, useNow} {
			// A table of one-phase levels with a maximum above it, on auto.
			out = append(out, fieldCase{report: "probe", fuseA: 25, mode: "auto", steps: "1p-3", timing: tm, carPhases: 3, use: use,
				house: houseBalanced, houseName: "balanced", charger: "1p32A"})
		}
		for _, fuse := range []float64{16, 25} {
			// Phase mode unset, a goal, and a plan slot of 2,300 W.
			out = append(out, fieldCase{report: "probe", fuseA: fuse, mode: "", steps: "none", timing: tm, carPhases: 3, use: usePlanned,
				house: houseBalanced, houseName: "balanced", planW: 2300})
		}
		// Surplus-only with a goal and 2.5 kW of export: one phase is allowed.
		out = append(out, fieldCase{report: "probe", fuseA: 25, mode: "auto", steps: "mixed", timing: tm, carPhases: 3, use: useSurplus,
			house: houseBalanced, houseName: "balanced", goal: true, exportW: []float64{2500, 2500, 3000, 2500}})
		// A house step on one phase under a three-phase car is not a one-phase car.
		out = append(out, fieldCase{report: "probe", fuseA: 25, mode: "3p", steps: "3p", timing: tm, carPhases: 3, use: usePlanned,
			house: houseBalanced, houseName: "balanced", houseStep: 30 * time.Minute})
	}
	for _, tm := range []fieldTiming{timingFast, timingEasee, timingSlowCloud} {
		// A held 1p order raised to 3p beside a phase at 20 A.
		out = append(out, fieldCase{report: "probe", fuseA: 25, mode: "auto", steps: "mixed", timing: tm, carPhases: 3, use: usePhaseUp,
			house: [3]float64{2, 20, 2}, houseName: "2-20-2", warm: true})
	}
	for _, tm := range fieldTimings {
		for _, use := range []fieldUse{usePlanned, useNow} {
			// The help report's site with a one-phase car.
			c := helpReport(use, tm)
			c.carPhases = 1
			out = append(out, c)
		}
	}
	for _, tm := range []fieldTiming{timingFast, timingEasee} {
		// A small goal for the one-phase car: the plan picks its lowest
		// level, which must be a third of an order the table has.
		c := helpReport(usePlanned, tm)
		c.carPhases, c.needW = 1, 1000
		out = append(out, c)
	}
	c := helpReport(usePlanned, timingEasee)
	c.houseStep = 30 * time.Minute
	return append(out, c)
}

// Failure kinds: the invariants. The sitesim adapter's own checks give the
// first five.
const (
	failFuse     = "fuse"      // the main fuse blew
	failLimit    = "limit"     // over Core's limit after an outside change, longer than the fastest lever
	failOwnRaise = "own-raise" // a raise we sent kept a phase over the limit more than 3 s
	failIdle     = "idle"      // the car idle with room for it and a request
	failStale    = "stale"     // a raise on a stale meter
	failCut      = "cut"       // the car's order cut for the fuse with every phase under the limit
	failOverCut  = "over-cut"  // cut again for an overage that a cut the car has not applied yet covers
	failEnergy   = "energy"    // a planned charge under 90 % of its energy by the deadline the fuse allowed
	failOnePhase = "one-phase" // planned charging or Charge now put a three-phase charger on one phase
	failVerdict  = "verdict"   // a charger drawing against its order not reported as not following
	failHomeUse  = "home-use"  // Home use not measured while every device reads
)

func failureKind(msg string) string {
	for _, k := range []struct{ text, kind string }{
		{"the main fuse blew", failFuse}, {"a raise we sent kept", failOwnRaise},
		{"over Core's limit", failLimit}, {"the car is idle", failIdle}, {"on a meter", failStale},
	} {
		if strings.Contains(msg, k.text) {
			return k.kind
		}
	}
	return "other"
}

// fieldPlan is the planner's side of a deadline goal, the way the directive
// reaches the charger: at each 15 min slot the remaining energy at the
// smallest planning step that reaches the goal by the deadline, as the slot's
// Wh and its on-power. It replans from what the car has taken, as a real
// replan does from Core's session energy.
type fieldPlan struct {
	id             string
	deadline       time.Time
	needWh         float64
	planning       func() []float64 // the steps main.go gives the planner
	fixedW         float64          // > 0: every slot asks this, as Wh only
	slotStart      time.Time
	stepW, slotWh  float64
	slotDelivered0 float64
	run            *fieldRun
}

const fieldSlot = 15 * time.Minute

func (p *fieldPlan) slotEnd() time.Time {
	end := p.slotStart.Add(fieldSlot)
	if end.After(p.deadline) {
		return p.deadline
	}
	return end
}

func (p *fieldPlan) directive(now time.Time) (loadpoint.Directive, bool) {
	if !now.Before(p.deadline) {
		return loadpoint.Directive{}, false
	}
	if start := now.Truncate(fieldSlot); !start.Equal(p.slotStart) {
		p.slotStart, p.slotDelivered0 = start, p.run.deliveredWh
		p.stepW, p.slotWh = 0, 0
		left, hours := p.needWh-p.run.deliveredWh, p.deadline.Sub(start).Hours()
		if p.fixedW > 0 {
			p.stepW, p.slotWh = p.fixedW, p.fixedW*p.slotEnd().Sub(start).Hours()
		} else if steps := p.planning(); left > 0 && hours > 0 && len(steps) > 0 {
			p.stepW = slices.Max(steps)
			for _, w := range steps {
				if w > 0 && w >= left/hours {
					p.stepW = w
					break
				}
			}
			p.slotWh = math.Min(left, p.stepW*p.slotEnd().Sub(start).Hours())
		}
	}
	d := loadpoint.Directive{SlotStart: p.slotStart, SlotEnd: p.slotEnd(), LoadpointEnergyWh: map[string]float64{p.id: p.slotWh}}
	if p.fixedW == 0 {
		d.LoadpointMaxPowerW = map[string]float64{p.id: p.stepW}
	}
	return d, true
}

// fieldPlanningSteps is what the planner can choose for the session: the
// steps main.go gives it (planningLimitsForLoadpoint), within the minimum
// and maximum it gets with them, as mpc's normalizedSteps keeps them.
func fieldPlanningSteps(s *simSite) []float64 {
	st, _ := s.mgr.State(s.lp.ID)
	cfg := loadpoint.Config{MinChargeW: st.MinChargeW, MaxChargeW: st.MaxChargeW, AllowedStepsW: st.AllowedStepsW, PhaseMode: s.lp.PhaseMode}
	minW, maxW, steps := loadpoint.PlanningLimits(st, cfg, loadpoint.SiteFuse{MaxAmps: s.p.Fuse.RatingA, Voltage: s.p.Voltage, PhaseCnt: 3})
	return slices.DeleteFunc(slices.Clone(steps), func(w float64) bool { return w > 0 && (w < minW || maxW > 0 && w > maxW) })
}

// asks reports whether the plan wants the car charging now: its slot has an
// on-power and more energy left than the charger's response time takes.
func (p *fieldPlan) asks(now time.Time, response time.Duration) bool {
	if p.stepW <= 0 || !now.Before(p.deadline) {
		return false
	}
	return p.slotWh-(p.run.deliveredWh-p.slotDelivered0) > p.stepW*response.Hours()
}

// fieldRun holds a case's request and the checks the adapter does not do.
type fieldRun struct {
	c         fieldCase
	holdPhase string
	plan      *fieldPlan
	verdict   func() (status, reason string)
	paused    bool
	failures  map[string]string // the first failure of each kind

	deliveredWh   float64
	orderW        float64 // the order last sent, and since when
	orderAt       time.Time
	expectW       float64   // the draw it asks for: expected_w for a one-phase car, else the order
	mismatchSince time.Time // the car drawing against an order older than its response time
	drawW         float64   // the car's draw, and when it last changed
	drawChangedAt time.Time
	heatRoomSince time.Time // the phases' heat has left room to start since
	nextLook      time.Time
	trace         func(format string, args ...any) // FTW_FIELD_TRACE names the case
	tracedReason  string
	exportPeak    func(window time.Duration) (float64, bool) // surplus: the forecast from now
}

// FieldVerdict returns a reader of the EV verdict the API shows for a store
// and loadpoints: status and reason. The api package imports control, so
// sitesim_field_api_test.go in package control_test sets it.
var FieldVerdict func(tel *telemetry.Store, lps *loadpoint.Manager, ctrl *loadpoint.Controller) func() (status, reason string)

func (s *simSite) coreEvery() time.Duration {
	if s.every > 0 {
		return s.every
	}
	return simCoreEvery
}

func (s *simSite) declaredMaxAge() time.Duration {
	if s.field == nil {
		return 0
	}
	return s.field.c.timing.declared
}

// evData is what a charger driver adds to its reading.
func (s *simSite) evData() map[string]any {
	if s.field == nil {
		return nil
	}
	d := map[string]any{"connected": s.ev.Plugged, "control_power_confirmed": true}
	if age := s.field.c.timing.declared; age > 0 {
		d["power_max_age_s"] = age.Seconds()
	}
	return d
}

func (f *fieldRun) fail(s *simSite, kind, format string, args ...any) {
	s.fail(format, args...)
	f.harvest(s, kind)
}

// harvest files the adapter's failure under its kind and clears it, so a
// case runs to its end and reports every kind it breaks.
func (f *fieldRun) harvest(s *simSite, kind string) {
	if s.failure == "" {
		return
	}
	if kind == "" {
		kind = failureKind(s.failure)
	}
	if _, ok := f.failures[kind]; !ok {
		f.failures[kind] = s.failure
	}
	if kind != failFuse {
		s.failure = ""
	}
}

func (f *fieldRun) limitA(s *simSite) float64 { return s.p.Fuse.RatingA - evPhaseMarginA }

// send records the command as the driver registry does and checks a cut.
func (f *fieldRun) send(ctx context.Context, s *simSite, driver string, payload []byte) error {
	var cmd struct {
		Action    string   `json:"action"`
		PowerW    float64  `json:"power_w"`
		ExpectedW *float64 `json:"expected_w"`
	}
	ours := json.Unmarshal(payload, &cmd) == nil && cmd.Action == "ev_set_current" && driver == s.lp.DriverName
	if ours && cmd.PowerW < s.offerW-1 && !s.p.Offline("evse") {
		// Surplus follows the export, an outside step; its cuts are not judged.
		if st, _ := s.mgr.State(s.lp.ID); st.CommandedReason == "fuse_limit" && !s.surplus {
			f.checkCut(s, cmd.PowerW)
		}
	}
	if st, _ := s.mgr.State(s.lp.ID); f.trace != nil && cmd.Action == "ev_set_current" && (cmd.PowerW != s.offerW || st.CommandedReason != f.tracedReason) {
		f.tracedReason = st.CommandedReason
		e := s.p.EVSE
		f.trace("%s order %.0f W (%s); phases %.1f A, offer %.0f A on %d, car %.0f W on %d",
			s.p.Now.Format("15:04:05"), cmd.PowerW, st.CommandedReason, s.p.A, e.OfferA, e.OfferPhases, e.DrawW, e.DrawPhases)
	}
	evidence := s.store.BeginCommand(driver, payload, s.p.Now)
	err := s.send(ctx, driver, payload)
	result := "accepted"
	if err != nil {
		result = "failed"
	}
	s.store.CompleteCommand(evidence, result)
	expectW := f.expectW
	if ours && err == nil {
		expectW = s.offerW
		if cmd.ExpectedW != nil {
			expectW = *cmd.ExpectedW
		}
	}
	if s.offerW != f.orderW || expectW != f.expectW {
		f.orderW, f.expectW, f.orderAt = s.offerW, expectW, s.p.Now
	}
	return err
}

// checkCut judges a cut for the fuse against the meter Core read. With
// every phase under the limit nothing measured asks for it: only orders
// that cannot have shown yet, which may hold back a raise and never cut.
// Over the limit, a cut the car has not applied yet that covers the
// overage already answers it; a second one takes the car down twice.
func (f *fieldRun) checkCut(s *simSite, w float64) {
	limit, e, over := f.limitA(s), s.p.EVSE, false
	for i, a := range s.meter.PhaseA {
		over = over || math.Abs(a) >= limit || s.heat[i] >= limit*limit
	}
	if !over {
		f.fail(s, failCut, "cut the car %.0f -> %.0f W for the fuse with every phase under %.0f A (meter %.1f A)",
			s.offerW, w, limit, s.meter.PhaseA)
		return
	}
	queuedA, queuedPhases, queued := e.Queued()
	if !queued || e.DrawW <= 0 {
		return
	}
	drawA := e.DrawW / s.p.Voltage / float64(e.DrawPhases)
	carA := s.p.Car.MaxW / s.p.Voltage / float64(min(max(queuedPhases, 1), e.Phases, s.p.Car.Phases))
	covered := true
	for i, a := range s.meter.PhaseA {
		after := 0.0
		if i < min(max(queuedPhases, 1), e.Phases, s.p.Car.Phases) && queuedA >= e.MinA {
			after = math.Min(queuedA, carA)
		}
		before := 0.0
		if i < e.DrawPhases {
			before = drawA
		}
		if overA := math.Abs(a) - limit; overA > 0 && before-after < overA {
			covered = false
		}
	}
	if covered {
		f.fail(s, failOverCut, "cut the car %.0f -> %.0f W while a cut to %.0f A it has not applied covers the overage (meter %.1f A, limit %.0f A)",
			s.offerW, w, queuedA, s.meter.PhaseA, limit)
	}
}

// wantsCharge adds the request to the adapter's room test: a planned charge
// asks only while its slot has energy left, and a pause asks for nothing.
// After an overage Core waits for the importing phases' heat (I² over 60 s,
// the fuse's own memory) to leave room for the car's minimum and a step, for
// evRestartDwell, as its start rule does.
func (f *fieldRun) wantsCharge(s *simSite, baseW float64) bool {
	p, limit := s.p, f.limitA(s)
	room := true
	for i := range min(p.EVSE.Phases, p.Car.Phases) {
		level := p.A[i] // A phase that exports has room for import, whatever its heat.
		if level > 0 {
			level = math.Max(level, math.Sqrt(s.heat[i]))
		}
		room = room && level+p.EVSE.MinA+evPhaseMarginA <= limit
	}
	switch {
	case !room:
		f.heatRoomSince = time.Time{}
	case f.heatRoomSince.IsZero():
		f.heatRoomSince = p.Now
	}
	switch {
	case f.paused, !room, p.Now.Sub(f.heatRoomSince) < evRestartDwell && p.EVSE.DrawW <= 0:
		return false
	case f.plan != nil:
		return f.plan.asks(p.Now, p.EVSE.Delay+time.Minute)
	case s.surplus:
		return -baseW >= f.surplusMinW(s)
	}
	return true
}

// surplusMinW is the least export surplus charging can use: one phase at
// 6 A where the loadpoint may run on one phase and the next 30 min of
// forecast stay below three phases at 6 A, as pickSurplusSteps decides;
// else the car's phases.
func (f *fieldRun) surplusMinW(s *simSite) float64 {
	e, phases := s.p.EVSE, min(s.p.EVSE.Phases, s.p.Car.Phases)
	min3 := 3 * s.p.Voltage * e.MinA
	onePhase := s.lp.PhaseMode != "3p" && slices.ContainsFunc(s.lp.AllowedStepsW, func(w float64) bool { return w > 0 && w < min3 })
	near, ok := 0.0, false
	if f.exportPeak != nil {
		near, ok = f.exportPeak(30 * time.Minute)
	}
	if onePhase && (!ok || near < min3) || s.lp.PhaseMode == "1p" {
		phases = 1
	}
	return e.MinA * s.p.Voltage * float64(phases)
}

// response is how long Core may wait for the charger's power to show an
// order: what its driver declares, or Easee's 180 s when it declares nothing.
func (f *fieldRun) response() time.Duration {
	if d := f.c.timing.declared; d > 0 {
		return max(d, f.c.timing.delay)
	}
	return max(evUnknownSettle, f.c.timing.delay)
}

func (f *fieldRun) check(s *simSite) {
	p, now := s.p, s.p.Now
	f.harvest(s, "")
	f.deliveredWh += p.EVSE.DrawW * sitesim.Step.Hours()
	if p.EVSE.DrawW != f.drawW {
		f.drawW, f.drawChangedAt = p.EVSE.DrawW, now
	}
	if f.c.use != useSurplus && f.c.use != usePhaseUp && p.EVSE.Phases == 3 && p.EVSE.OfferPhases == 1 && p.EVSE.OfferA > 0 {
		f.fail(s, failOnePhase, "%s put the three-phase charger on one phase at %.0f A", f.c.use, p.EVSE.OfferA)
	}
	up := s.downUntil.IsZero()
	if up && p.Car.Plugged && now.Sub(f.orderAt) >= f.response() && math.Abs(p.EVSE.DrawW-f.expectW) > telemetry.ControlToleranceW(f.expectW) {
		if f.mismatchSince.IsZero() {
			f.mismatchSince = now
		}
	} else {
		f.mismatchSince = time.Time{}
	}
	if !up || now.Before(f.nextLook) {
		return
	}
	f.nextLook = now.Add(10 * time.Second)
	// A charger that reports late leaves the balance negative until its
	// reading catches up with the meter; that is not a failed read.
	// The plant's PV has no driver of its own, so Home use cannot be
	// measured while it produces.
	if s.meterAt.Equal(now) && p.PVW == 0 && now.Sub(f.drawChangedAt) > p.EVSE.ReadbackLag+2*sitesim.Step {
		if live := s.store.LiveMeasurementNow("meter", telemetry.ForecastOptions{}); live.LoadQuality != telemetry.LoadMeasured {
			f.fail(s, failHomeUse, "Home use is %s (%s) while every device reads", live.LoadQuality, live.LoadReason)
		}
	}
	// Core waits its response time for an order to take hold, then two
	// minutes of measured disagreement (srcfl/ftw#1795). A car marked
	// one-phase is judged against a third of a three-phase order, the
	// expected_w Core sends with it (srcfl/ftw#1801).
	if f.verdict != nil && !f.mismatchSince.IsZero() && now.Sub(f.mismatchSince) >= 2*time.Minute+30*time.Second {
		if status, reason := f.verdict(); status != "not_following" && reason != "one_phase" {
			f.fail(s, failVerdict, "the car draws %.0f W against an expected %.0f W (%v), the verdict says %s/%s",
				p.EVSE.DrawW, f.expectW, now.Sub(f.orderAt).Round(time.Second), status, reason)
		}
	}
}

// fieldRoomW is the most the car can draw under Core's limit with the
// house's steady load: what a deadline goal can be held to.
func fieldRoomW(c fieldCase, e *sitesim.EVSE, car *sitesim.Car) float64 {
	phases := min(e.Phases, car.Phases)
	worst := 0.0
	for i := range phases {
		worst = math.Max(worst, c.house[i])
	}
	roomA := math.Min(c.fuseA-evPhaseMarginA-worst, e.MaxA)
	if roomA < e.MinA {
		return 0
	}
	return math.Min(car.MaxW, roomA*230*float64(phases))
}

type fieldAction struct {
	at time.Duration
	do func(*simSite)
}

// runFieldCase builds the site and runs the request; the run holds the
// failures by kind. It runs inside a synctest bubble.
func runFieldCase(c fieldCase, trace func(string, ...any)) (*simSite, *fieldRun) {
	start := time.Date(2026, 10, 21, 22, 0, 0, 0, time.Local)
	time.Sleep(start.Sub(time.Now())) // The bubble's clock becomes the plant's.
	p := sitesim.New(start, c.fuseA)
	phases := 3
	if c.mode == "1p" {
		phases = 1
	}
	p.EVSE = &sitesim.EVSE{Phases: phases, MinA: 6, MaxA: 16, Delay: c.timing.delay}
	if c.charger == "1p32A" {
		p.EVSE.Phases, p.EVSE.MaxA = 1, 32
	}
	if c.timing.cloud {
		p.EVSE.ReadbackLag, p.EVSE.SplitAtA, p.EVSE.PhaseHold = 10*time.Second, c.fuseA, 90*time.Second
	}
	p.Car = &sitesim.Car{Phases: c.carPhases, MaxW: 11040, Plugged: true}
	if c.carPhases == 1 {
		p.Car.MaxW = 3680
	}
	if c.car != nil {
		c.car(p.Car)
	}
	p.HouseW = houseA(c.house[0], c.house[1], c.house[2])
	if c.warm {
		if err := p.Fuse.Preheat(c.house); err != nil {
			panic(err)
		}
	}
	if c.battery {
		p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 10000, MaxDischargeW: 10000, CapacityWh: 20000, SoC: .3, Latency: 3 * time.Second}
	}
	lp := fieldLoadpoint(c)
	f := &fieldRun{c: c, failures: map[string]string{}, holdPhase: "3p", trace: trace}
	if c.mode == "1p" {
		f.holdPhase = "1p" // The app sends the loadpoint's phases (web/app.js).
	}
	s := &simSite{p: p, lp: lp, surplus: c.use == useSurplus, every: c.every, field: f}

	run, kettle := 20*time.Minute, []time.Duration{8 * time.Minute}
	outside := func(do func(*sitesim.Plant)) func(*sitesim.Plant) {
		return func(p *sitesim.Plant) { do(p); s.outsideAt = p.Now }
	}
	var actions []fieldAction
	var exportPlan []float64 // surplus: each quarter hour's export without the car
	switch c.use {
	case usePlanned, useRestart:
		run, kettle = 3*time.Hour, []time.Duration{50 * time.Minute, 140 * time.Minute}
		if c.use == useRestart {
			run, kettle = 90*time.Minute, []time.Duration{50 * time.Minute}
			p.Events = append(p.Events, sitesim.Event{At: 30 * time.Minute, Kind: sitesim.CoreRestart, For: 20 * time.Second})
		}
		roomW, askW := fieldRoomW(c, p.EVSE, p.Car), 3300.0
		if c.planW > 0 {
			askW = c.planW
		}
		if c.needW > 0 {
			askW = c.needW
		}
		f.plan = &fieldPlan{id: lp.ID, deadline: start.Add(run), run: f, planning: func() []float64 { return fieldPlanningSteps(s) },
			fixedW: c.planW, needWh: math.Min(askW*run.Hours(), .8*roomW*run.Hours())}
		s.plan = f.plan.directive
	case usePause:
		run, kettle = 15*time.Minute, []time.Duration{11 * time.Minute}
		actions = []fieldAction{
			{5 * time.Minute, func(s *simSite) {
				f.paused = true
				s.ctrl.SetManualHold(lp.ID, loadpoint.ManualHold{PowerW: 0, PhaseMode: f.holdPhase, Persistent: true, StartedAt: s.p.Now, UpdatedAt: s.p.Now})
			}},
			{5*time.Minute + 25*time.Second, func(s *simSite) {
				f.paused = false
				s.ctrl.SetManualHold(lp.ID, loadpoint.ManualHold{PowerW: lp.MaxChargeW, PhaseMode: f.holdPhase, Persistent: true, StartedAt: s.p.Now, UpdatedAt: s.p.Now})
			}},
		}
	case usePhaseUp:
		run, kettle = 15*time.Minute, nil
		actions = []fieldAction{{5 * time.Minute, func(s *simSite) {
			s.ctrl.SetManualHold(lp.ID, loadpoint.ManualHold{PowerW: lp.MaxChargeW, PhaseMode: "3p", Persistent: true, StartedAt: s.p.Now, UpdatedAt: s.p.Now})
		}}}
	case useSurplus:
		run, kettle = time.Hour, nil
		// Export steps up and down, never past the fuse with the car off.
		houseW, maxPVW := p.HouseW[0]+p.HouseW[1]+p.HouseW[2], math.Inf(1)
		for _, a := range c.house {
			maxPVW = math.Min(maxPVW, 3*230*(c.fuseA-2*evPhaseMarginA+a))
		}
		exports := []float64{2500, 6000, 9000, 3000}
		if len(c.exportW) > 0 {
			exports = c.exportW
		}
		for i, exportW := range exports {
			pvW := -math.Min(houseW+exportW, maxPVW)
			exportPlan = append(exportPlan, -pvW-houseW)
			p.Events = append(p.Events, sitesim.Event{At: time.Duration(i) * 15 * time.Minute, Do: outside(func(p *sitesim.Plant) { p.PVW = pvW })})
		}
	}
	// A kettle on L1 for three minutes: an outside step Core must make room for.
	for _, at := range kettle {
		p.Events = append(p.Events,
			sitesim.Event{At: at, Do: outside(func(p *sitesim.Plant) { p.HouseW[0] += 6 * 230 })},
			sitesim.Event{At: at + 3*time.Minute, Do: outside(func(p *sitesim.Plant) { p.HouseW[0] -= 6 * 230 })})
	}
	if c.houseStep > 0 {
		p.Events = append(p.Events,
			sitesim.Event{At: c.houseStep, Do: outside(func(p *sitesim.Plant) { p.HouseW[0] += 8 * 230 })},
			sitesim.Event{At: c.houseStep + 10*time.Minute, Do: outside(func(p *sitesim.Plant) { p.HouseW[0] -= 8 * 230 })})
	}
	slices.SortStableFunc(p.Events, func(a, b sitesim.Event) int { return cmp.Compare(a.At, b.At) })

	s.setup = func(s *simSite) {
		// The periodic order goes through the outcome sender, as main.go
		// wires it to the driver registry, which records command evidence.
		s.ctrl.SetOutcomeSender(func(ctx context.Context, driver string, payload []byte, outcome func(error)) error {
			err := f.send(ctx, s, driver, payload)
			if outcome != nil {
				outcome(err)
			}
			return err
		})
		switch c.use {
		case useNow, usePause:
			// Charge now from the app, which names the loadpoint's phases (web/app.js).
			s.ctrl.SetManualHold(lp.ID, loadpoint.ManualHold{PowerW: lp.MaxChargeW, PhaseMode: f.holdPhase, Persistent: true, StartedAt: s.p.Now, UpdatedAt: s.p.Now})
		case usePhaseUp:
			s.ctrl.SetManualHold(lp.ID, loadpoint.ManualHold{PowerW: 3680, PhaseMode: "1p", Persistent: true, StartedAt: s.p.Now, UpdatedAt: s.p.Now})
		}
		if f.plan != nil || c.goal {
			deadline := start.Add(run).Local()
			s.mgr.SetSchedule(lp.ID, loadpoint.NewSchedule(1, deadline.Hour()*60+deadline.Minute(), false, 0, false))
		}
		if c.battery {
			fieldBatteryPlan(s, 6000)
		}
		if exportPlan != nil {
			// The planner's PV forecast, which decides when surplus may use
			// one phase: here it knows the export to come.
			peak := func(from, to time.Duration) (float64, bool) {
				w, ok := 0.0, false
				for i, e := range exportPlan {
					at := time.Duration(i) * 15 * time.Minute
					if at+15*time.Minute > from && at <= to {
						w, ok = math.Max(w, e), true
					}
				}
				return w, ok
			}
			f.exportPeak = func(window time.Duration) (float64, bool) {
				return peak(s.p.Now.Sub(start), s.p.Now.Sub(start)+window)
			}
			s.ctrl.SetNearTermPeakSurplusW(f.exportPeak)
			s.ctrl.SetPeakRemainingSurplusW(func() (float64, bool) { return peak(s.p.Now.Sub(start), run) })
		}
		if FieldVerdict != nil {
			f.verdict = FieldVerdict(s.store, s.mgr, s.ctrl)
		}
	}
	s.boot()
	for end := start.Add(run); s.p.Now.Before(end) && f.failures[failFuse] == ""; {
		for len(actions) > 0 && s.p.Now.Sub(start) >= actions[0].at && s.downUntil.IsZero() {
			actions[0].do(s)
			actions = actions[1:]
		}
		time.Sleep(sitesim.Step)
		s.step()
	}
	if f.plan != nil && f.plan.needWh > 0 && f.deliveredWh < .9*f.plan.needWh {
		f.fail(s, failEnergy, "the planned charge delivered %.1f of %.1f kWh by the deadline", f.deliveredWh/1000, f.plan.needWh/1000)
	}
	return s, f
}

// fieldBatteryPlan has the plan charge the home battery at w in every slot,
// with the car's slot in the same directive as the planner sends it.
func fieldBatteryPlan(s *simSite, w float64) {
	s.state.Mode, s.state.UseEnergyDispatch = ModePlannerArbitrage, true
	s.state.SlotDirective = func(now time.Time) (SlotDirective, bool) {
		start := now.Truncate(fieldSlot)
		d := SlotDirective{SlotStart: start, SlotEnd: start.Add(fieldSlot), BatteryEnergyWh: w * fieldSlot.Hours(), Strategy: "arbitrage"}
		if s.plan != nil {
			if ev, ok := s.plan(now); ok {
				d.LoadpointEnergyWh, d.LoadpointMaxPowerW = maps.Clone(ev.LoadpointEnergyWh), maps.Clone(ev.LoadpointMaxPowerW)
			}
		}
		return d, true
	}
}
