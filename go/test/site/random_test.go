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
				st.stopFTW()
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

// eventWindow is when the house changed on its own, and how long FTW may
// take to answer it.
type eventWindow struct{ from, to float64 }

func judgeInvariants(d *runData) (metrics, []criterion) {
	fuse := d.metaF("fuse")
	limit := fuse - 1
	h := d.timing()
	t0, t1 := d.marks["start"], d.marks["end"]
	run := d.window(t0, t1)
	m := metrics{"charger_command_s": h.commandS, "charger_startup_s": h.startupS, "events": len(d.events)}

	// Windows in which an over-limit second is the house's doing.
	var windows []eventWindow
	faultUntil := map[float64]float64{} // a fault's start and end
	for _, ev := range d.events {
		batteryUsable := true
		if r := d.at(ev.T); r != nil && r.Bat.SoCPct <= h.floorPct+1 {
			batteryUsable = false
		}
		// The fastest lever: the battery answers in 2 s, the charger in its
		// own time, never under 2 s; then the meter and tick, 2 s, and 3 s.
		lever := math.Max(2, h.commandS)
		if batteryUsable {
			lever = 2
		}
		allow := lever + 2 + 3
		switch ev.Event.Do {
		case "meter_late", "silence", "offline", "reconnect":
			allow += ev.Event.ForS
			faultUntil[ev.T] = ev.T + ev.Event.ForS
		case "meter_glitch":
			faultUntil[ev.T] = ev.T + 5
		}
		windows = append(windows, eventWindow{ev.T, ev.T + allow})
	}
	for _, mk := range d.allMarks {
		if mk.Label == "ftw:restart" {
			windows = append(windows, eventWindow{mk.T, mk.T + 60})
		}
	}
	explained := func(t float64) (float64, bool) {
		best, ok := 0.0, false
		for _, w := range windows {
			if t >= w.from && t <= w.to {
				best, ok = math.Max(best, w.to), true
			}
		}
		return best, ok
	}

	// 1, 2: the fuse holds; over the limit only as long as an outside step
	// allows, and at most 3 s after FTW's own raise.
	blownAny := anyBlown(run)
	unexplained, longestOwn := 0.0, 0.0
	var firstBad float64
	var start *sample
	for i, r := range run {
		over := maxAbsI(r) > limit
		if over && start == nil {
			start = r
		}
		if (!over || i == len(run)-1) && start != nil {
			end := r.T
			if to, ok := explained(start.T); ok {
				if end > to {
					unexplained = math.Max(unexplained, end-to)
					if firstBad == 0 {
						firstBad = to
					}
				}
			} else if end-start.T > 3 {
				longestOwn = math.Max(longestOwn, end-start.T)
				if firstBad == 0 {
					firstBad = start.T + 3
				}
			}
			start = nil
		}
	}
	m["over_limit_after_steps_s"], m["over_limit_own_s"] = round(unexplained, 1), round(longestOwn, 1)

	// 3: with every device reading and no fault, Home use is measured.
	inFault := func(t float64) bool {
		for from, to := range faultUntil {
			if t >= from && t <= to+30 {
				return true
			}
		}
		return false
	}
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

	// 4: during Charge now the car charges when there is room for it.
	var holds []eventWindow
	var holdFrom float64
	for _, mk := range d.allMarks {
		switch mk.Label {
		case "ftw:charge_now":
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
	grace := 60 + h.commandS + h.startupS + 10
	idle, run4 := 0.0, 0.0
	for _, hw := range holds {
		run4 = 0
		for i, r := range d.window(hw.from+grace, hw.to) {
			roomA := limit - (maxAbsI(r) - carW(r, 0)/(3*volt))
			bad := carW(r, 0) < 0.5*1380 && roomA >= 7 && !inFault(r.T) && !nearRestart(r.T)
			if bad && i > 0 {
				run4++
				idle = math.Max(idle, run4)
				if idle > 30 {
					note("charge now", r.T)
				}
			} else {
				run4 = 0
			}
		}
	}
	m["charge_now_idle_with_room_s"] = idle

	// 5: no order above the charger's 16 A per phase.
	over := 0
	for _, p := range connector1(profiles(d.frames, t0, t1)) {
		if p.Limit > 0 && p.perPhaseA() > 16+1e-6 {
			over++
			note("rating", t0+p.T)
		}
	}
	m["offers_over_rating"] = over

	// 6: FTW does not ask the battery to discharge at its floor.
	floorS := 0.0
	for _, r := range run {
		if r.Bat.EMSCmd == 187 && r.Bat.EMSSetW > 50 && r.Bat.SoCPct <= h.floorPct+0.5 {
			floorS++
			if floorS > 10 {
				note("floor", r.T)
			}
		}
	}
	m["discharge_at_floor_s"] = floorS

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
		crit("Charge now charges when there is room", idle <= 30),
		crit("no offer above the charger's rating", over == 0),
		crit("no discharge asked at the battery's floor", floorS <= 10),
		crit("no raise on a stale meter", raises == 0),
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
