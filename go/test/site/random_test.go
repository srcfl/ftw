//go:build unix

package site

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// A random case draws FTW's side from a seed: the fuse, the house, the
// loadpoint's settings and FTW's requests (Charge now, a pause, a plan, a
// Core restart, an import ceiling) at random times. The house draws its own
// timeline from the same seed (device-simulator's random block): load steps
// and spikes, PV clouds, a battery at its floor, a car taking less, slow
// chargers, silent devices, reconnects, a late or glitched meter, signed
// currents. FTW's invariants are checked every second. The house writes
// the timeline it drew, so a failing seed can be replayed and kept.

// randomDurationS is how long a random case runs.
const randomDurationS = 1200.0

// randomTolerated are invariants a seed may break for a finding still open
// on master. A fix deletes its line here.
var randomTolerated = map[string]string{
	"no discharge asked at the battery's floor": "ev-099: the fuse guard drives the battery below its floor; fix unmerged",
}

// ftwAction is one of FTW's requests in a random case.
type ftwAction struct {
	AtS       float64 `json:"at_s"`
	Do        string  `json:"do"` // charge_now, pause, clear, plan, restart, ceiling
	W         float64 `json:"w,omitempty"`
	PhaseMode string  `json:"phase_mode,omitempty"`
	DownS     float64 `json:"down_s,omitempty"`
	CeilingW  float64 `json:"ceiling_w,omitempty"`
	SoCPct    float64 `json:"soc_pct,omitempty"`
	GoalPct   float64 `json:"goal_pct,omitempty"`
	ReadyMin  float64 `json:"ready_min,omitempty"`
}

// randomPlan is everything a random case does, so it can be saved and run
// again: House is set once the house has drawn its timeline.
type randomPlan struct {
	Seed      int64          `json:"seed"`
	FuseA     float64        `json:"fuse_a"`
	HouseA    [3]float64     `json:"house_a"`
	PVW       float64        `json:"pv_w"`
	SoCPct    float64        `json:"soc_pct"`
	LPExtra   map[string]any `json:"lp_extra,omitempty"`
	Actions   []ftwAction    `json:"actions"`
	DurationS float64        `json:"duration_s"`
	House     *houseSpec     `json:"house,omitempty"`
	Found     *foundNote     `json:"found,omitempty"`
}

// foundNote says what a saved case failed and when it was found.
type foundNote struct {
	Invariant string `json:"invariant"`
	On        string `json:"on"` // the date
	Commit    string `json:"commit,omitempty"`
}

func drawPlan(seed int64) randomPlan {
	rng := rand.New(rand.NewSource(seed))
	pick := func(xs ...float64) float64 { return xs[rng.Intn(len(xs))] }
	between := func(lo, hi float64) float64 { return math.Round((lo+rng.Float64()*(hi-lo))*10) / 10 }
	p := randomPlan{Seed: seed, DurationS: randomDurationS,
		FuseA: pick(16, 20, 25), PVW: pick(0, 0, 2000, 5000, 8000), SoCPct: pick(6, 15, 40, 70, 95)}
	for k := range p.HouseA {
		p.HouseA[k] = between(0.5, p.FuseA/2)
	}
	p.LPExtra = map[string]any{}
	switch rng.Intn(3) {
	case 1:
		p.LPExtra["phase_mode"] = "auto"
	case 2:
		p.LPExtra["phase_mode"] = "3p"
	}
	if rng.Intn(2) == 0 {
		p.LPExtra["allowed_steps_w"] = easeeSteps
	}
	for t := 40 + between(0, 60); t < p.DurationS-60; t += between(60, 300) {
		a := ftwAction{AtS: t}
		switch rng.Intn(10) {
		case 0, 1, 2:
			a.Do, a.W, a.PhaseMode = "charge_now", pick(4140, 7360, 11040), "3p"
		case 3:
			a.Do, a.W, a.PhaseMode = "charge_now", pick(1380, 3680), "1p"
		case 4:
			a.Do = "pause"
		case 5:
			a.Do = "clear"
		case 6:
			a.Do, a.SoCPct, a.GoalPct, a.ReadyMin = "plan", between(20, 50), between(60, 90), pick(30, 60, 240)
		case 7:
			a.Do, a.DownS = "restart", between(5, 30)
		case 8:
			a.Do, a.CeilingW = "ceiling", pick(0, 3000, 5000, 8000)
		default:
			continue
		}
		p.Actions = append(p.Actions, a)
	}
	return p
}

// randomScenario runs a plan drawn from seed, or a saved one.
func randomScenario(p randomPlan) scenario {
	name, dir := fmt.Sprintf("r%d", p.Seed), fmt.Sprintf("random-%d", p.Seed)
	sc := scenario{name: name, dir: dir, minutes: p.DurationS/60 + 1,
		run:      func(c *runCtx) (map[string]any, error) { return runPlan(c, p) },
		judge:    judgeInvariants,
		tolerate: randomTolerated}
	if p.Found != nil {
		sc.name, sc.dir = "found-"+fmt.Sprint(p.Seed), "found-"+fmt.Sprint(p.Seed)
		sc.known = map[string]string{p.Found.Invariant: fmt.Sprintf("found by random seed %d on %s; triage it", p.Seed, p.Found.On)}
	}
	return sc
}

func runPlan(c *runCtx, p randomPlan) (map[string]any, error) {
	loads := [3]float64{p.HouseA[0] * volt, p.HouseA[1] * volt, p.HouseA[2] * volt}
	spec := stackSpec{fuseA: p.FuseA, loadsW: loads, pvW: p.PVW, socPct: p.SoCPct, lpExtra: p.LPExtra}
	if p.House != nil {
		spec.house = p.House
	} else {
		spec.random = &houseRandom{Seed: p.Seed, DurationS: p.DurationS, GapS: 45}
	}
	plan, _ := json.MarshalIndent(p, "", "  ")
	err := with(c, spec, func(st *stack) error {
		os.WriteFile(filepath.Join(st.rundir, "plan.json"), plan, 0o644)
		if err := st.plug(0); err != nil {
			return err
		}
		t0 := time.Now()
		st.mark("start")
		for _, a := range p.Actions {
			sleepS(time.Until(t0.Add(time.Duration(a.AtS * float64(time.Second)))).Seconds())
			st.mark("ftw:" + a.Do)
			var err error
			switch a.Do {
			case "charge_now":
				err = st.chargeNow("garage", 0, a.PhaseMode, a.W)
			case "pause":
				err = st.chargeNow("garage", 0, "3p", -1)
			case "clear":
				_, err = call("DELETE", st.ftw+"/api/loadpoints/garage/manual_hold", nil, nil)
			case "plan":
				_, err = st.planTo("garage", a.SoCPct, a.GoalPct, time.Duration(a.ReadyMin*float64(time.Minute)))
			case "restart":
				st.stopFTW(syscall.SIGTERM)
				sleepS(a.DownS)
				err = st.startFTW()
				st.mark("ftw:up")
			case "ceiling":
				err = st.post("/api/peak_import_ceiling", map[string]any{"peak_import_ceiling_w": a.CeilingW}, nil)
			}
			if err != nil {
				st.mark("ftw:error")
			}
		}
		sleepS(time.Until(t0.Add(time.Duration(p.DurationS * float64(time.Second)))).Seconds())
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "random", "seed": p.Seed, "fuse": p.FuseA}, err
}

// ---- invariants, every second ----

// houseTiming is what the house picked for the charger, from the spec it
// ran (timeline.json for a random house).
type houseTiming struct {
	commandS, startupS float64
	floorPct           float64
}

func (d *runData) timing() houseTiming {
	h := houseTiming{floorPct: 5}
	for _, name := range []string{"timeline.json", "../house.json"} {
		b, err := os.ReadFile(filepath.Join(d.dir, "house", name))
		if err != nil {
			continue
		}
		var spec houseSpec
		if json.Unmarshal(b, &spec) != nil {
			continue
		}
		for _, dev := range spec.Devices {
			if dev.ID == "garage" {
				if dev.CommandResponseS != nil {
					h.commandS = *dev.CommandResponseS
				}
				if dev.StartupDelayS != nil {
					h.startupS = *dev.StartupDelayS
				}
			}
			if dev.ID == "inverter" && dev.MinSoCPct != nil {
				h.floorPct = *dev.MinSoCPct
			}
		}
		break
	}
	return h
}

// eventWindow is a stretch of time: a fault, or a Charge now hold.
type eventWindow struct{ from, to float64 }

// within is whether t falls in one of ws, or up to afterS after its end.
func within(ws []eventWindow, t, afterS float64) bool {
	for _, w := range ws {
		if t >= w.from && t <= w.to+afterS {
			return true
		}
	}
	return false
}

// inverterBackFrom gives, for a time t, the latest return before t (at):
// the inverter's from an outage (inverterOut) or Core's from a restart
// (down), and when the R8 count may start after it (from). Core has the inverter back at its first
// good read of it (no consecutive errors), which the Modbus reconnect
// backoff may hold back up to reconnectBackoffMaxS. After an outage, a
// default that failed while the inverter was away waits on the registry's
// retry timer, up to defaultRetryMaxS more, until #1915 (C11) sends it on
// the first good poll. A Core that starts fresh has no such wait.
func (d *runData) inverterBackFrom(inverterOut, down []eventWindow) func(t float64) (at, from float64) {
	type back struct {
		at, from float64
	}
	var backs []back
	add := func(at float64, outage bool) {
		from := at + reconnectBackoffMaxS
		for _, r := range d.samples {
			if r.T >= at && r.T < from && readsInverter(r) {
				from = r.T
				break
			}
		}
		if outage {
			from += defaultRetryMaxS
		}
		backs = append(backs, back{at, from})
	}
	for _, w := range inverterOut {
		add(w.to, true)
	}
	for _, w := range down {
		add(w.to, false)
	}
	return func(t float64) (at, from float64) {
		at, from = math.Inf(-1), math.Inf(-1)
		for _, b := range backs {
			if b.at <= t && b.at >= at {
				at, from = b.at, b.from
			}
		}
		return at, from
	}
}

// readsInverter is whether Core's last poll of the inverter succeeded. A
// status without the driver's error count says nothing, and counts as a
// read.
func readsInverter(r *sample) bool {
	if r.Status == nil {
		return false
	}
	dr, ok := r.Status.Drivers[inverterDriver]
	return !ok || dr.ConsecutiveErrors == nil || *dr.ConsecutiveErrors == 0
}

// stepWindow is a change FTW did not make, and how long FTW may take to
// answer it: until from + extraS + its lever's time + 2 s for the meter and
// the tick + 3 s.
type stepWindow struct {
	from, extraS float64
	leverS       float64 // > 0: the only lever that can answer (the charger, for a car FTW did not start)
	untilT       float64 // > 0: the window ends here, whatever the lever (a Core restart)
	noBattery    bool    // the step took the inverter away
}

// What the random judge takes from the rig and from Core.
const (
	rigBatteryMaxW  = 5000.0 // the simulated Sungrow's largest charge or discharge
	evRestartDwellS = 35.0   // control/ev_fuse_budget.go evRestartDwell
	tickS           = 2.0    // the rig's site.control_interval_s
	plannerFloorPct = 10.0   // Core's planner.soc_min when the config leaves it out (cmd/ftw/main.go)

	// R8: how long a battery FTW does not dispatch may hold a forced mode,
	// and the Core timers that may delay it after the inverter returns.
	inverterDriver       = "sungrow" // the rig's inverter driver (stack_test.go)
	forcedBudgetS        = 10.0
	reconnectBackoffMaxS = 60.0 // modbus/client.go reconnectBackoffMax
	defaultRetryMaxS     = 30.0 // drivers/registry.go defaultRetryMax; 0 once #1915 (C11) lands
)

// dispatchW is Core's battery order on r, + charge; false when Core
// dispatched no battery.
func dispatchW(r *sample) (float64, bool) {
	if r == nil || r.Status == nil || len(r.Status.Dispatch) == 0 {
		return 0, false
	}
	w := 0.0
	for _, x := range r.Status.Dispatch {
		w += x.TargetW
	}
	return w, true
}

// worstSignedA is the phase current furthest from 0, with its direction.
func worstSignedA(r *sample) float64 {
	w := 0.0
	for _, a := range r.PhaseA {
		if math.Abs(a) > math.Abs(w) {
			w = a
		}
	}
	return w
}

// chargeNowChanges says, for each Charge now in the run's plan, whether it
// asked for a power or phase mode other than the Charge now before it.
// Without a plan every Charge now counts as a change.
func (d *runData) chargeNowChanges() []bool {
	b, err := os.ReadFile(filepath.Join(d.dir, "plan.json"))
	if d.dir == "" || err != nil {
		return nil
	}
	var p randomPlan
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	var out []bool
	var last *ftwAction
	for i, a := range p.Actions {
		if a.Do != "charge_now" {
			continue
		}
		out = append(out, last == nil || last.W != a.W || last.PhaseMode != a.PhaseMode)
		last = &p.Actions[i]
	}
	return out
}

func judgeInvariants(d *runData) (metrics, []criterion) {
	fuse := d.metaF("fuse")
	h := d.timing()
	t0, t1 := d.marks["start"], d.marks["end"]
	run := d.window(t0, t1)
	d.warm()
	m := metrics{"charger_command_s": h.commandS, "charger_startup_s": h.startupS, "events": len(d.events)}
	charger := math.Max(2, h.commandS) // the charger answers in its own time, never under 2 s
	orders := d.offers()

	// Windows in which an over-limit second is the house's doing. Only a
	// meter fault adds its length to the time FTW may take: it blinds FTW.
	// An inverter outage takes the battery lever away; a charger outage
	// delays the charger lever (chargerLever).
	var windows []stepWindow
	var faults, chargerFaults, inverterOut, chargerOut []eventWindow
	for _, ev := range d.events {
		w := stepWindow{from: ev.T}
		until := 0.0
		switch ev.Event.Do {
		case "meter_late", "silence", "offline", "reconnect":
			until = ev.T + ev.Event.ForS
		case "meter_glitch":
			until = ev.T + 5
		}
		if until > 0 {
			f := eventWindow{ev.T, until}
			faults = append(faults, f)
			switch ev.Event.Device {
			case "", "meter":
				chargerFaults = append(chargerFaults, f)
				w.extraS = ev.Event.ForS
			case "garage":
				chargerFaults = append(chargerFaults, f)
				chargerOut = append(chargerOut, f)
			case "inverter":
				inverterOut = append(inverterOut, f)
				w.noBattery = true
			}
		}
		windows = append(windows, w)
	}
	// The battery's floor: the higher of the inverter's own floor and the
	// planner's soc_min. FTW may not discharge it there (check 6), so there
	// it is no lever for import either.
	floorPct := h.floorPct
	if p := d.metaF("planner_floor_pct"); p > 0 {
		floorPct = math.Max(floorPct, p)
	} else {
		floorPct = math.Max(floorPct, plannerFloorPct)
	}
	for _, mk := range d.allMarks {
		if mk.Label == "ftw:restart" {
			windows = append(windows, stepWindow{from: mk.T, untilT: mk.T + 60})
		}
	}
	// A car that starts to draw more than FTW's last order lets it (the
	// charger's own default, or a lower order still on its way) is a step
	// FTW did not take, and only the charger can answer it.
	orderW := func(t float64) (float64, bool) {
		w, ok := 0.0, false
		for _, p := range orders {
			if p.T > t {
				break
			}
			w, ok = p.offerW(), true
		}
		return w, ok
	}
	carStarts := 0
	for i := 1; i < len(d.samples); i++ {
		r := d.samples[i]
		if r.T < t0 || r.T > t1 || carW(r, 0)-carW(d.samples[i-1], 0) <= 1000 {
			continue
		}
		if w, ok := orderW(r.T); !ok || carW(r, 0) > w+3*volt {
			windows = append(windows, stepWindow{from: r.T, leverS: charger})
			carStarts++
		}
	}
	m["car_starts_ftw_did_not_order"] = carStarts

	// chargerLever is how long the charger takes to answer from t: its own
	// time, after the rest of any outage it is in.
	chargerLever := func(t float64) float64 {
		left := 0.0
		for _, w := range chargerOut {
			if t >= w.from && t <= w.to {
				left = math.Max(left, w.to-t)
			}
		}
		return charger + left
	}
	// lever is how long FTW's fastest lever takes, from t, to take overA off
	// the worst phase: the battery, in 2 s, while its inverter runs and its
	// spare power covers the overage; otherwise the charger. At its floor
	// the battery may only charge less, never discharge more.
	lever := func(t, overA float64, export bool) float64 {
		r := d.at(t)
		if r == nil || within(inverterOut, t, 0) || (r.Bat.Running != nil && !*r.Bat.Running) {
			return chargerLever(t)
		}
		spare := rigBatteryMaxW + r.BatW // discharge more, or charge less
		switch {
		case export:
			spare = rigBatteryMaxW - r.BatW
		case r.Bat.SoCPct <= floorPct:
			spare = math.Max(0, r.BatW)
		}
		if spare < overA*3*volt {
			return chargerLever(t)
		}
		return 2
	}
	until := func(w stepWindow, at, overA float64, export bool) float64 {
		if w.untilT > 0 {
			return w.untilT
		}
		lv := w.leverS
		switch {
		case lv > 0:
			lv += chargerLever(w.from) - charger
		case w.noBattery:
			lv = chargerLever(w.from)
		default:
			lv = lever(math.Max(w.from, at), overA, export)
		}
		return w.from + w.extraS + lv + 2 + 3
	}

	// 1, 2: the fuse holds; over the limit only as long as an outside step
	// allows, and at most 3 s after FTW's own raise. Between the fuse and
	// 1.2 x the fuse the 60 s heat is the budget (heatOver): the clock
	// starts when the heat passes the fuse, as Core's cut does.
	blownAny := anyBlown(run)
	unexplained, longestOwn := 0.0, 0.0
	var firstBad float64
	bad := func(t float64) {
		if firstBad == 0 {
			firstBad = t
		}
	}
	// judged lists each stretch the heat or 1.2 x the fuse put on the clock:
	// from, end, clock start and allowed until, in seconds from the start,
	// and whether it was FTW's own raise or a step.
	judged := [][]any{}
	judgeStretch := func(stretch []*sample, end float64) {
		s := stretch[0].T
		var clock *sample
		overA, export := 0.0, false
		for _, r := range stretch {
			if clock == nil && heatOver(r, fuse) {
				clock = r
			}
			if w := worstSignedA(r); math.Abs(w)-fuse > overA {
				overA, export = math.Abs(w)-fuse, w < 0
			}
		}
		if clock == nil {
			return // the heat budget covers it
		}
		c := clock.T
		to, explained, own := math.Inf(-1), false, false
		for _, w := range windows {
			if x := until(w, s, overA, export); w.from <= s && s <= x {
				to, explained = math.Max(to, x), true
			}
		}
		if !explained { // no change outside FTW: its own, import or export
			own, to = true, c+3
		}
		if !own && c > s { // Core answers once the heat says so
			to = math.Max(to, c+lever(c, overA, export)+2+3)
		}
		// Every window that opens inside a step's stretch while it is still
		// allowed extends it: a second step, an outage or a Core restart.
		// FTW's own raise stays its own.
		for grown := !own; grown; {
			grown = false
			for _, w := range windows {
				if x := until(w, w.from, overA, export); w.from >= s && w.from <= math.Min(end, to) && x > to {
					to, grown = x, true
				}
			}
		}
		kind := "step"
		if own {
			kind = "own"
		}
		judged = append(judged, []any{round(s-t0, 1), round(end-t0, 1), round(c-t0, 1), round(to-t0, 1), kind})
		switch {
		case end <= to:
		case own:
			longestOwn = math.Max(longestOwn, end-c)
			bad(to)
		default:
			unexplained = math.Max(unexplained, end-to)
			bad(to)
		}
	}
	from := -1
	for i, r := range run {
		over := maxAbsI(r) > fuse
		if over && from < 0 {
			from = i
		}
		if (!over || i == len(run)-1) && from >= 0 {
			hi := i
			if over {
				hi = i + 1
			}
			judgeStretch(run[from:hi], r.T)
			from = -1
		}
	}
	heatPeak := 0.0
	for _, r := range run {
		heatPeak = math.Max(heatPeak, max(r.HeatA[0], r.HeatA[1], r.HeatA[2]))
	}
	m["over_limit_after_steps_s"], m["over_limit_own_s"] = round(unexplained, 1), round(longestOwn, 1)
	m["heat_peak_a"], m["over_limit_judged"] = round(heatPeak, 2), judged

	// 3: with every device reading and no fault, Home use is measured.
	inFault := func(t float64) bool { return within(faults, t, 30) }
	restartAt := []float64{t0}
	for _, mk := range d.allMarks {
		if mk.Label == "ftw:up" {
			restartAt = append(restartAt, mk.T)
		}
	}
	nearRestart := func(t float64) bool {
		for _, r := range restartAt {
			if t >= r && t <= r+60 {
				return true
			}
		}
		return false
	}
	first := map[string]float64{}
	note := func(name string, t float64) {
		if _, ok := first[name]; !ok {
			first[name] = t
		}
	}
	if firstBad > 0 {
		note("limit", firstBad)
	}
	for _, r := range run {
		if blown(r) {
			note("fuse", r.T)
			break
		}
	}
	dishonest, run3 := 0.0, 0.0
	for i, r := range run {
		bad := r.Status != nil && allRead(r) && !inFault(r.T) && !nearRestart(r.T) && str(status(r).LoadQuality) != "measured"
		if bad && i > 0 {
			run3 += r.T - run[i-1].T
			dishonest = math.Max(dishonest, run3)
			if dishonest > 10 {
				note("home use", r.T)
			}
		} else {
			run3 = 0
		}
	}
	m["home_use_estimated_while_reading_s"] = round(dishonest, 1)

	// 4: during Charge now the car charges when there is room for it. With
	// room, Core waits evRestartDwell before it starts a stopped car, then
	// the charger takes its own time, and a tick. The count of idle seconds
	// with room starts again on each Charge now that changes the request.
	// A cut FTW had to send needs no new start: the count runs only while
	// the car is idle with room, so it starts when the cut car stops. A
	// cut with room is FTW's doing and starts nothing. While Core is down
	// the count stops. Only a fault of the meter or of the charger excuses
	// an idle car.
	var holds []eventWindow
	var graceFrom []float64
	var holdFrom float64
	changes, k := d.chargeNowChanges(), 0
	for _, mk := range d.allMarks {
		switch mk.Label {
		case "ftw:charge_now":
			changed := k >= len(changes) || changes[k]
			k++
			if holdFrom == 0 || changed {
				graceFrom = append(graceFrom, mk.T)
			}
			if holdFrom == 0 {
				holdFrom = mk.T
			}
		case "ftw:pause", "ftw:clear", "ftw:plan", "end":
			if holdFrom > 0 {
				holds = append(holds, eventWindow{holdFrom, mk.T})
				holdFrom = 0
			}
		}
	}
	// roomA is the room below the fuse for the car, Core's line; 6 A is
	// the car's minimum on three phases.
	roomA := func(r *sample) float64 { return fuse - (maxAbsI(r) - carW(r, 0)/(3*volt)) }
	// While Core is down nothing can start the car.
	var down []eventWindow
	for i, mk := range d.allMarks {
		if mk.Label != "ftw:restart" {
			continue
		}
		up := t1
		for _, n := range d.allMarks[i:] {
			if n.Label == "ftw:up" {
				up = n.T
				break
			}
		}
		down = append(down, eventWindow{mk.T, up})
	}
	// The car may stand idle with room for Core's dwell, the charger's
	// command and startup time and a tick, plus the 2 s for the meter and
	// tick and 3 s the fuse budgets also give.
	allowedIdle := evRestartDwellS + h.commandS + h.startupS + tickS + 2 + 3
	regrace := func(from, to float64) bool {
		for _, g := range graceFrom {
			if g > from && g <= to {
				return true
			}
		}
		return false
	}
	idle, run4 := 0.0, 0.0
	for _, hw := range holds {
		run4 = 0
		ws := d.window(hw.from, hw.to)
		for i, r := range ws {
			if i > 0 && regrace(ws[i-1].T, r.T) {
				run4 = 0
			}
			bad := carW(r, 0) < 0.5*1380 && roomA(r) >= 6 && !within(chargerFaults, r.T, 30) && !within(down, r.T, 0)
			if bad && i > 0 {
				run4 += r.T - ws[i-1].T
				idle = math.Max(idle, run4)
				if run4 > allowedIdle {
					note("charge now", r.T)
				}
			} else {
				run4 = 0
			}
		}
	}
	m["charge_now_idle_with_room_s"], m["charge_now_idle_allowed_s"] = round(idle, 1), allowedIdle

	// 5: no order above the charger's 16 A per phase.
	over := 0
	for _, p := range connector1(profiles(d.frames, t0, t1)) {
		if p.Limit > 0 && p.perPhaseA() > 16+1e-6 {
			over++
			note("rating", t0+p.T)
		}
	}
	m["offers_over_rating"] = over

	// 6: FTW does not ask the battery to discharge at its floor (floorPct).
	// FTW's orders are judged, not the inverter's register, which can hold
	// an order from before an outage; check 8 judges that.
	floorS := 0.0
	for _, r := range run {
		if w, ok := dispatchW(r); ok && w < -50 && r.Bat.SoCPct <= floorPct {
			floorS++
			if floorS > 10 {
				note("floor", r.T)
			}
		}
	}
	m["discharge_at_floor_s"], m["floor_pct"] = floorS, floorPct

	// 7: a stale meter means no raise.
	raises := 0
	prs := connector1(profiles(d.frames, t0, t1))
	for _, ev := range d.events {
		if ev.Event.Do != "meter_late" {
			continue
		}
		for i := 1; i < len(prs); i++ {
			at := t0 + prs[i].T
			if at > ev.T+2 && at <= ev.T+ev.Event.ForS && prs[i].offerW() > prs[i-1].offerW()+1 {
				raises++
				note("stale meter", at)
			}
		}
	}
	m["raises_on_a_stale_meter"] = raises

	// 8: a battery FTW does not dispatch runs in its own mode (R8, C1). The
	// inverter's register may hold a forced charge or discharge FTW does
	// not dispatch for forcedBudgetS. After an inverter outage or a Core
	// restart the count starts once Core has the inverter back (backFrom).
	// While Core is stopped with SIGTERM its driver cleanup writes the
	// default, so a running inverter gets the same 10 s from the stop. One
	// that comes back while Core is down has its outage's allowance. The
	// triage's line is 10 s from the return itself; the seconds past that
	// line that Core's timers excuse are recorded, not hidden.
	backFrom := d.inverterBackFrom(inverterOut, down)
	forcedS, pastS, timersS, run8, from8 := 0.0, 0.0, 0.0, 0.0, 0.0
	for i, r := range run {
		w, ok := dispatchW(r)
		stray := math.Abs(batOrder(r)) > 50 && !within(inverterOut, r.T, 0)
		if !within(down, r.T, 0) {
			stray = stray && r.Status != nil && (!ok || math.Abs(w) <= 50)
		}
		if !stray || i == 0 {
			run8 = 0
			continue
		}
		if run8 == 0 {
			from8 = run[i-1].T
		}
		run8 += r.T - run[i-1].T
		forcedS = math.Max(forcedS, run8)
		at, from := backFrom(r.T)
		switch {
		case r.T > math.Max(from8, from)+forcedBudgetS:
			pastS += r.T - run[i-1].T
			note("forced battery", r.T)
		case r.T > math.Max(from8, at)+forcedBudgetS:
			timersS += r.T - run[i-1].T
		}
	}
	m["battery_forced_without_dispatch_s"] = round(forcedS, 1)
	m["battery_forced_past_allowance_s"] = round(pastS, 1)
	m["battery_forced_in_core_timers_s"] = round(timersS, 1)
	// The first second an invariant broke, from the start, per invariant:
	// a kept seed is cut a minute after the one it failed on.
	firsts := map[string]float64{}
	for k, t := range first {
		firsts[k] = round(t-t0, 1)
	}
	m["first_violation_s"] = firsts
	return m, []criterion{
		crit("fuse holds", !blownAny),
		crit("over the limit no longer than an outside step allows", unexplained == 0),
		crit("over the limit at most 3 s after FTW's own raise", longestOwn == 0),
		crit("Home use measured while every device reads", dishonest <= 10),
		crit("Charge now charges when there is room", idle <= allowedIdle),
		crit("no offer above the charger's rating", over == 0),
		crit("no discharge asked at the battery's floor", floorS <= 10),
		crit("no raise on a stale meter", raises == 0),
		crit("no forced battery mode FTW does not dispatch", pastS == 0),
	}
}

// ---- seeds, replays and saved cases ----

// randomSet parses FTW_SITE=random:<first>:<count> (or random:<seed>).
func randomSet(which string) ([]scenario, bool) {
	if !strings.HasPrefix(which, "random:") {
		return nil, false
	}
	var first, count int64 = 0, 1
	parts := strings.Split(strings.TrimPrefix(which, "random:"), ":")
	fmt.Sscan(parts[0], &first)
	if len(parts) > 1 {
		fmt.Sscan(parts[1], &count)
	}
	var out []scenario
	for s := first; s < first+count; s++ {
		out = append(out, randomScenario(drawPlan(s)))
	}
	return out, true
}

// foundScenarios are saved failing seeds (testdata/found/*.json): fixed
// cases in the full set, known until someone triages them.
func foundScenarios(root string) []scenario {
	files, _ := filepath.Glob(filepath.Join(root, "go", "test", "site", "testdata", "found", "*.json"))
	sort.Strings(files)
	var out []scenario
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var p randomPlan
		if json.Unmarshal(b, &p) == nil && p.House != nil && p.Found != nil {
			out = append(out, randomScenario(p))
		}
	}
	return out
}

// invariantKey ties each criterion to its first_violation_s entry.
var invariantKey = map[string]string{
	"fuse holds": "fuse",
	"over the limit no longer than an outside step allows": "limit",
	"over the limit at most 3 s after FTW's own raise":     "limit",
	"Home use measured while every device reads":           "home use",
	"Charge now charges when there is room":                "charge now",
	"no offer above the charger's rating":                  "rating",
	"no discharge asked at the battery's floor":            "floor",
	"no raise on a stale meter":                            "stale meter",
	"no forced battery mode FTW does not dispatch":         "forced battery",
}

// keepFailure writes a failing random run as a case to keep: its plan with
// the house's timeline, both cut a minute after the first violation.
func keepFailure(rundir, out string, o outcome) string {
	b, err := os.ReadFile(filepath.Join(rundir, "plan.json"))
	if err != nil {
		return ""
	}
	var p randomPlan
	if json.Unmarshal(b, &p) != nil {
		return ""
	}
	var h houseSpec
	if hb, err := os.ReadFile(filepath.Join(rundir, "house", "timeline.json")); err == nil && json.Unmarshal(hb, &h) == nil {
		p.House = &h
	} else if p.House == nil {
		return ""
	}
	failed := ""
	for _, c := range o.Criteria {
		if _, tolerated := randomTolerated[c.Name]; !c.OK && !tolerated {
			failed = c.Name
			break
		}
	}
	var res struct {
		Metrics map[string]any `json:"metrics"`
	}
	if rb, err := os.ReadFile(filepath.Join(rundir, "result.json")); err == nil && json.Unmarshal(rb, &res) == nil {
		firsts, _ := res.Metrics["first_violation_s"].(map[string]any)
		at, ok := firsts[invariantKey[failed]].(float64)
		if ok && at+60 < p.DurationS {
			p.DurationS = math.Ceil(at + 60)
			var evs []houseEvent
			for _, ev := range p.House.Timeline {
				if ev.AtS <= p.DurationS {
					evs = append(evs, ev)
				}
			}
			p.House.Timeline = evs
			var acts []ftwAction
			for _, a := range p.Actions {
				if a.AtS <= p.DurationS {
					acts = append(acts, a)
				}
			}
			p.Actions = acts
		}
	}
	p.Found = &foundNote{Invariant: failed, On: time.Now().Format("2006-01-02")}
	dst := filepath.Join(out, "found", fmt.Sprintf("%d.json", p.Seed))
	os.MkdirAll(filepath.Dir(dst), 0o755)
	nb, _ := json.MarshalIndent(p, "", "  ")
	if os.WriteFile(dst, nb, 0o644) != nil {
		return ""
	}
	return dst
}
