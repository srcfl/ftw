//go:build unix

package site

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Unit tests for the random judge (judgeInvariants), each on the shape of
// a nightly seed that the judge called wrong (triage of 27 Oct 2026, rig
// items R2-R7). They run in every go test, without a simulator.

const (
	limitStep = "over the limit no longer than an outside step allows"
	limitOwn  = "over the limit at most 3 s after FTW's own raise"
	chargeNow = "Charge now charges when there is room"
	atFloor   = "no discharge asked at the battery's floor"
)

// randomRun is n one-second samples on a fuse, from start at 0 to end at
// n-1: house current base per phase, the car idle, the battery at 50 %
// with its inverter running. set changes sample i.
func randomRun(n int, fuse, base float64, set func(i int, r *sample)) *runData {
	d := &runData{marks: map[string]float64{"start": 0, "end": float64(n - 1)}, meta: map[string]any{"fuse": fuse}}
	running := true
	for i := 0; i < n; i++ {
		r := &sample{T: float64(i), PhaseA: [3]float64{base, base, base}, CarW: []float64{0},
			Bat: plantDevice{SoCPct: 50, Running: &running}}
		if set != nil {
			set(i, r)
		}
		d.samples = append(d.samples, r)
	}
	return d
}

// withCharger records the charger's command and startup times as the house
// wrote them, so the judge reads them as from a real run.
func withCharger(t *testing.T, d *runData, commandS, startupS float64) {
	t.Helper()
	dir := t.TempDir()
	spec := houseSpec{Devices: []houseDevice{{ID: "garage", CommandResponseS: &commandS, StartupDelayS: &startupS}}}
	b, _ := json.Marshal(spec)
	if err := os.MkdirAll(filepath.Join(dir, "house"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "house", "timeline.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	d.dir = dir
}

func judged(d *runData) map[string]bool {
	_, cs := judgeInvariants(d)
	return verdictsOf(cs)
}

// R2, seed 207539's shape: a step the battery answers opens the stretch,
// and a Core restart 1 s into it extends what the stretch may take. A
// restart that comes after the step's time ran out excuses nothing.
func TestInvariantsExtendAStretchByEveryWindowInsideIt(t *testing.T) {
	for _, c := range []struct {
		name      string
		restartAt float64 // 0: no restart
		want      bool
	}{
		{"restart 1 s into the stretch", 120, true},
		{"no restart", 0, false},
		{"restart after the step's time", 130, false},
		{"restart 0.2 s after the step's time", 121.9, false},
	} {
		// 20 A fuse; 26 A (1.3 x) from 119 to 143 s, after a load step
		// at 114.7 s. The battery's 5 kW (7.2 A) covers the 6 A overage,
		// so the step alone allows 2 + 2 + 3 s.
		d := randomRun(301, 20, 10, func(i int, r *sample) {
			if i >= 119 && i < 144 {
				r.PhaseA = [3]float64{26, 13, 12}
			}
		})
		withCharger(t, d, 40, 0)
		d.events = []houseEventLine{{T: 114.7, Event: houseEvent{Do: "loads"}}}
		if c.restartAt > 0 {
			d.allMarks = []markLine{{T: c.restartAt, Label: "ftw:restart"}, {T: c.restartAt + 9, Label: "ftw:up"}}
		}
		if got := judged(d)[limitStep]; got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, limitStep, got, c.want)
		}
	}
}

// R3, seeds 207534, 207530 and 207539: the battery answers in 2 s only
// while its inverter runs and its spare power covers the overage; otherwise
// the charger is the lever, in its own time (5 s here).
func TestInvariantsBatteryIsTheLeverOnlyWhenItCanAnswer(t *testing.T) {
	for _, c := range []struct {
		name    string
		batW    float64
		running bool
		want    bool
	}{
		{"battery idle, 7.2 A spare for a 4 A overage", 0, true, false},
		{"battery already discharging 3266 W, 2.5 A spare", -3266, true, true},
		{"inverter offline", 0, false, true},
	} {
		// 16 A fuse; 20 A (1.25 x) from 50 to 59 s after a step at 49.5 s:
		// 9.5 s, past the battery's 7 s and inside the charger's 10 s.
		d := randomRun(101, 16, 8, func(i int, r *sample) {
			r.BatW, r.Bat.Running = c.batW, &c.running
			if i >= 50 && i < 59 {
				r.PhaseA = [3]float64{20, 12, 12}
			}
		})
		withCharger(t, d, 5, 0)
		d.events = []houseEventLine{{T: 49.5, Event: houseEvent{Do: "loads"}}}
		if got := judged(d)[limitStep]; got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, limitStep, got, c.want)
		}
	}
}

// R4, seeds 207532 and 207537: a car that starts on the charger's own
// 16 A while FTW's order is 0 A is a step FTW did not take, answered by the
// 40 s charger. The same start right after FTW raised the order to 16 A is
// FTW's own.
func TestInvariantsCarStartingOnItsOwnIsAStep(t *testing.T) {
	for _, c := range []struct {
		name     string
		frames   []frame
		wantStep bool
		wantOwn  bool
	}{
		{"FTW ordered 0 A", []frame{profileAt(0, 0), profileAt(20, 0)}, true, true},
		{"FTW raised to 16 A", []frame{profileAt(0, 0), profileAt(25, 16)}, true, false},
	} {
		// 16 A fuse; the car draws 11 040 W from 30 s; the meter shows
		// 20 A (1.25 x) from 31 to 39 s.
		d := randomRun(121, 16, 4, func(i int, r *sample) {
			if i >= 30 && i < 40 {
				r.CarW[0] = 11040
			}
			if i >= 31 && i < 39 {
				r.PhaseA = [3]float64{20, 18, 19}
			}
		})
		withCharger(t, d, 40, 30)
		d.frames = c.frames
		v := judged(d)
		if v[limitStep] != c.wantStep || v[limitOwn] != c.wantOwn {
			t.Errorf("%s: step %v, own %v; want %v, %v", c.name, v[limitStep], v[limitOwn], c.wantStep, c.wantOwn)
		}
	}
}

// R5, seed 207536: between the fuse and 1.2 x the fuse the 60 s heat is the
// budget (owner rule, 27 Oct 2026). 27.84 A on 25 A for 15 s from 15 A
// passes; above 1.2 x it fails once the step's time is up; so does the same
// 27.84 A held past the lever's time after the heat passes the fuse, and
// 25.5 A on a fuse already warm at its rating.
func TestInvariantsJudgeBetweenFuseAndInstantByHeat(t *testing.T) {
	for _, c := range []struct {
		name     string
		base, a  float64
		from, to int
		want     bool
	}{
		{"27.84 A for 15 s", 15, 27.84, 50, 65, true},
		{"30.1 A for 15 s", 15, 30.1, 50, 65, false},
		{"27.84 A for 200 s", 15, 27.84, 50, 250, false},
		{"25.5 A for 15 s on a warm fuse", 25, 25.5, 50, 65, false},
		// The heat passes the fuse at 127 s; the battery then has 2 + 5 s.
		{"27.84 A until 7 s after the heat passes the fuse", 15, 27.84, 50, 134, true},
		{"27.84 A until 8 s after the heat passes the fuse", 15, 27.84, 50, 135, false},
	} {
		d := randomRun(301, 25, c.base, func(i int, r *sample) {
			if i >= c.from && i < c.to {
				r.PhaseA[0] = c.a
			}
		})
		d.events = []houseEventLine{{T: 49.5, Event: houseEvent{Do: "loads"}}}
		if got := judged(d)[limitStep]; got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, limitStep, got, c.want)
		}
	}
}

// R6, seeds 207533, 207536 and 207539: during Charge now an idle car with
// room is allowed Core's 35 s dwell, the charger's command and startup time
// and a tick, plus 5 s (72 s here, with a 30 s startup). The grace starts
// again on each Charge now that changes the request and on each cut FTW
// had to send, over the fuse or with no room; a cut with room does not.
// While Core is down the count stops. A fault of the meter or the charger
// excuses the car; an inverter outage does not.
func TestInvariantsChargeNowGrace(t *testing.T) {
	for _, c := range []struct {
		name   string
		startS int    // the car draws from here; 0: never
		overS  [2]int // the car draws 16 A, 21 A on the 16 A fuse, in [from, to)
		marks  []markLine
		frames []frame
		events []houseEventLine
		want   bool
	}{
		{"starts after 70 s", 71, [2]int{}, nil, nil, nil, true},
		{"starts after 80 s", 81, [2]int{}, nil, nil, nil, false},
		{"a changed Charge now at 60 s", 121, [2]int{}, []markLine{{T: 60, Label: "ftw:charge_now"}}, nil, nil, true},
		{"FTW cuts the car over the fuse at 60 s", 121, [2]int{10, 61}, nil,
			[]frame{profileAt(0, 16), profileAt(60, 0)}, nil, true},
		{"FTW cuts an idle car with room at 60 s", 121, [2]int{}, nil,
			[]frame{profileAt(0, 16), profileAt(60, 0)}, nil, false},
		{"FTW hunts 6 A, 0 A, 6 A with room", 0, [2]int{}, nil,
			[]frame{profileAt(0, 6), profileAt(15, 0), profileAt(30, 6), profileAt(45, 0),
				profileAt(60, 6), profileAt(75, 0), profileAt(90, 6), profileAt(105, 0),
				profileAt(120, 6), profileAt(135, 0), profileAt(150, 6), profileAt(165, 0),
				profileAt(180, 6), profileAt(195, 0)}, nil, false},
		{"Core restarts at 50 s and answers at 60 s", 121, [2]int{},
			[]markLine{{T: 50, Label: "ftw:restart"}, {T: 60, Label: "ftw:up"}}, nil, nil, true},
		{"the meter late for 40 s from 10 s", 140, [2]int{}, nil, nil,
			[]houseEventLine{{T: 10, Event: houseEvent{Do: "meter_late", ForS: 40}}}, true},
		{"the inverter offline for 40 s from 10 s", 140, [2]int{}, nil, nil,
			[]houseEventLine{{T: 10, Event: houseEvent{Do: "offline", Device: "inverter", ForS: 40}}}, false},
	} {
		// 16 A fuse, a 5 A house: 11 A of room. Charge now at 1 s.
		d := randomRun(201, 16, 5, func(i int, r *sample) {
			if c.startS > 0 && i >= c.startS {
				r.CarW[0] = 6900
				r.PhaseA = [3]float64{15, 15, 15}
			}
			if i >= c.overS[0] && i < c.overS[1] {
				r.CarW[0] = 11040
				r.PhaseA = [3]float64{21, 21, 21}
			}
		})
		withCharger(t, d, 0, 30)
		d.allMarks = append([]markLine{{T: 1, Label: "ftw:charge_now"}}, c.marks...)
		d.allMarks = append(d.allMarks, markLine{T: 200, Label: "end"})
		d.frames, d.events = c.frames, c.events
		if got := judged(d)[chargeNow]; got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, chargeNow, got, c.want)
		}
	}
}

// R7, seeds 207534 and 207536: the floor judges FTW's own discharge orders
// (status.dispatch) against the higher of the inverter's 5 % and the
// planner's 10 %, with no extra 0.5 %. A forced discharge the inverter
// still holds while FTW dispatches nothing is not FTW's order.
func TestInvariantsFloorJudgesFTWsOrders(t *testing.T) {
	for _, c := range []struct {
		name    string
		socPct  float64
		orderW  float64 // FTW's dispatch; 0: none
		forcedW float64 // the inverter's register
		want    bool
	}{
		{"FTW orders 3 kW at 5.0 %", 5, -3000, 3000, false},
		{"FTW orders 3 kW at 9 %, under the planner's 10 %", 9, -3000, 3000, false},
		{"FTW orders 3 kW at 10.5 %", 10.5, -3000, 3000, true},
		{"the register holds 3 kW at 5.0 %, FTW orders nothing", 5, 0, 3000, true},
	} {
		d := randomRun(61, 16, 5, func(i int, r *sample) {
			r.Bat.SoCPct = c.socPct
			r.Bat.EMSMode, r.Bat.EMSCmd, r.Bat.EMSSetW = 2, 187, c.forcedW
			r.Status = &ftwStatus{}
			if c.orderW != 0 {
				r.Status.Dispatch = []ftwDispatch{{Driver: "sungrow", TargetW: c.orderW}}
			}
		})
		if got := judged(d)[atFloor]; got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, atFloor, got, c.want)
		}
	}
}

// An over-limit stretch no outside change explains is FTW's own, whichever
// way the current flows and however it is cut up: FTW's discharge pushing
// export over the fuse, and a warm fuse held over its rating with a 1 s dip
// every 7 s, or every 4 s. A dip does not end a stretch while the heat stays
// over the fuse. All fail after 3 s.
func TestInvariantsAStretchWithNoStepIsFTWsOwn(t *testing.T) {
	export := randomRun(121, 16, -10, func(i int, r *sample) {
		w := 0.0
		if i >= 47 {
			w, r.BatW = -5000, -5000
		}
		r.Status = &ftwStatus{Dispatch: []ftwDispatch{{Driver: "sungrow", TargetW: w}}}
		if i >= 50 && i < 56 {
			r.PhaseA = [3]float64{-21, -21, -21}
		}
	})
	dips := func(every int) *runData {
		return randomRun(601, 25, 25.5, func(i int, r *sample) {
			if i%every == every-1 {
				r.PhaseA = [3]float64{24.9, 24.9, 24.9}
			}
		})
	}
	for name, d := range map[string]*runData{"export 6 s at 1.31 x In": export,
		"600 s on a warm fuse, a dip every 7 s": dips(7), "600 s on a warm fuse, 3 s over and 1 s under": dips(4)} {
		withCharger(t, d, 5, 0)
		if v := judged(d); v[limitOwn] {
			t.Errorf("%s: %q passed", name, limitOwn)
		}
	}
}

// A window that opens inside FTW's own raise does not turn it into a step:
// FTW raises the car at 45 s and the phases sit at 31 A (1.24 x 25 A) from
// 50 to 60 s; PV moving at 51 s changes nothing, and a load step at 51 s
// that adds 1 A comes after FTW's raise broke the line. The battery is at
// its floor, so the 40 s charger would be a step's lever.
func TestInvariantsAnEventInsideFTWsRaiseLeavesItOwn(t *testing.T) {
	for _, c := range []struct {
		name  string
		event string // at 51 s; "": none
		addA  float64
	}{
		{"no event", "", 0},
		{"PV moves", "pv", 0},
		{"a load step adds 1 A", "loads", 1},
	} {
		d := randomRun(121, 25, 10, func(i int, r *sample) {
			r.Bat.SoCPct = 6
			if i >= 50 && i < 60 {
				r.PhaseA = [3]float64{31, 31, 31}
			}
			if i >= 51 && i < 60 {
				r.PhaseA[0] += c.addA
			}
		})
		withCharger(t, d, 40, 0)
		d.frames = []frame{profileAt(40, 0), profileAt(45, 16)}
		if c.event != "" {
			d.events = []houseEventLine{{T: 51, Event: houseEvent{Do: c.event}}}
		}
		if v := judged(d); v[limitOwn] {
			t.Errorf("%s: FTW's raise held 10 s at 1.24 x In and passed", c.name)
		}
	}
}

// The battery is the 2 s lever for import only above the floor the floor
// check uses (the planner's 10 % here), and never while its inverter is
// out; a charger outage delays the charger. Only a meter fault adds its
// length to a step's time. 16 A fuse, a 5 s charger, 20 A on L1 (1.25 x).
func TestInvariantsLeverAndFaults(t *testing.T) {
	for _, c := range []struct {
		name     string
		socPct   float64
		from, to int // 20 A on L1 in [from, to)
		events   []houseEventLine
		want     bool
	}{
		{"a step at 8 %: the charger answers", 8, 50, 59, nil, true},
		{"a step at 11 %: the battery answers", 11, 50, 59, nil, false},
		{"a step while the inverter is offline", 50, 50, 59,
			[]houseEventLine{{T: 30, Event: houseEvent{Do: "offline", Device: "inverter", ForS: 40}}}, true},
		{"the inverter offline 40 s, 15 s over", 50, 50, 65,
			[]houseEventLine{{T: 45, Event: houseEvent{Do: "offline", Device: "inverter", ForS: 40}}}, false},
		{"the meter late 40 s, 15 s over", 50, 50, 65,
			[]houseEventLine{{T: 45, Event: houseEvent{Do: "meter_late", ForS: 40}}}, true},
		{"the charger away 20 s, 18 s over", 8, 50, 68,
			[]houseEventLine{{T: 45, Event: houseEvent{Do: "reconnect", Device: "garage", ForS: 20}}}, true},
	} {
		d := randomRun(121, 16, 8, func(i int, r *sample) {
			r.Bat.SoCPct = c.socPct
			if i >= c.from && i < c.to {
				r.PhaseA = [3]float64{20, 12, 12}
			}
		})
		withCharger(t, d, 5, 0)
		d.events = append([]houseEventLine{{T: 49.5, Event: houseEvent{Do: "loads"}}}, c.events...)
		if got := judged(d)[limitStep]; got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, limitStep, got, c.want)
		}
	}
}

// forcedCase is an R8 run: the inverter's register holds a forced command
// from the start until forcedTo, and FTW dispatches the battery from
// dispatchFrom (0: never). An outage of the inverter (offline: it stops; a
// silence: it runs but does not answer) or a Core restart may come first.
type forcedCase struct {
	name         string
	outage       string  // "offline", "silence" or ""
	outFrom      float64 // the outage, from outFrom
	outFor       float64 // for outFor
	readAt       float64 // Core's polls of the inverter fail from outFrom until here
	stop, up     float64 // a Core restart (SIGTERM at stop), if up > 0
	cmd          float64 // 187 discharge, 170 charge
	setW         float64
	forcedTo     float64
	dispatchFrom float64
	want         bool
}

func (c forcedCase) run() *runData {
	// FTW dispatched the battery until the outage or the stop.
	lead := c.outFrom
	if c.up > 0 && (c.outage == "" || c.stop < lead) {
		lead = c.stop
	}
	d := randomRun(1201, 16, 5, func(i int, r *sample) {
		t := float64(i)
		if t < c.forcedTo {
			r.Bat.EMSMode, r.Bat.EMSCmd, r.Bat.EMSSetW = 2, c.cmd, c.setW
		} else {
			r.Bat.EMSCmd = 204
		}
		if c.outage == "offline" && t >= c.outFrom && t < c.outFrom+c.outFor {
			off := false
			r.Bat.Running = &off
		}
		if c.up > 0 && t >= c.stop && t <= c.up {
			return // Core is down: no reading
		}
		errs := 0
		if c.outage != "" && t >= c.outFrom && t < c.readAt {
			errs = 1 + int(t-c.outFrom)/2
		}
		r.Status = &ftwStatus{Drivers: map[string]ftwDriver{inverterDriver: {Status: "ok", ConsecutiveErrors: &errs}}}
		if t < lead || (c.dispatchFrom > 0 && t >= c.dispatchFrom) {
			r.Status.Dispatch = []ftwDispatch{{Driver: inverterDriver, TargetW: -c.setW}}
		}
	})
	if c.outage != "" {
		d.events = []houseEventLine{{T: c.outFrom, Event: houseEvent{Do: c.outage, Device: "inverter", ForS: c.outFor}}}
	}
	if c.up > 0 {
		d.allMarks = []markLine{{T: c.stop, Label: "ftw:restart"}, {T: c.up, Label: "ftw:up"}}
	}
	return d
}

// R8, C1, on the shapes of the nightly seeds on master 83c2b24c. Once Core
// has the inverter back, a battery FTW does not dispatch leaves its forced
// mode within 10 s, plus the registry's 30 s default retry until #1915
// (C11) lands. The Modbus reconnect backoff between the inverter's return
// and Core's first good read is Core's to take.
func TestInvariantsNoForcedBatteryWithoutDispatch(t *testing.T) {
	const forced = "no forced battery mode FTW does not dispatch"
	read207539 := 182 + reconnectBackoffMaxS - 4
	for _, c := range []forcedCase{
		// 207533: offline 576.6-644.7 s, Core reads 9 s later, 187 /
		// 2668 W until FTW dispatches again 236 s after the return (C1).
		{name: "207533: held until Core's 5 min retry", outage: "offline", outFrom: 100, outFor: 68, readAt: 177,
			cmd: 187, setW: 2668, forcedTo: 1201, dispatchFrom: 404, want: false},
		// 207535: the same with a forced charge, 170 / 2632 W, for 235 s.
		{name: "207535: a forced charge held", outage: "offline", outFrom: 100, outFor: 69, readAt: 177,
			cmd: 170, setW: 2632, forcedTo: 404, want: false},
		// 207537: the inverter runs but does not answer for 46.4 s; Core
		// reads 19.8 s after it answers again, the discharge holds 264 s.
		{name: "207537: held after a silence", outage: "silence", outFrom: 100, outFor: 46, readAt: 166,
			cmd: 187, setW: 3854, forcedTo: 1201, dispatchFrom: 410, want: false},
		// 207538: Core reads 2 s after the return; held to the run's end.
		{name: "207538: held to the end", outage: "offline", outFrom: 100, outFor: 39, readAt: 141,
			cmd: 187, setW: 4674, forcedTo: 1201, want: false},
		// The same outages once C1 is fixed: the default goes out within
		// 10 s of Core's first good read.
		{name: "default 8 s after Core reads again", outage: "offline", outFrom: 100, outFor: 68, readAt: 177,
			cmd: 187, setW: 2668, forcedTo: 185, want: true},
		// 207539: offline 508.5-590.0 s, the Modbus backoff (60 s then)
		// holds Core's first good read back 55.7 s, and FTW dispatches 20 s
		// after it. Here the read comes 4 s before the backoff's cap ends.
		// Inside Core's timers; it fails once #1915 (C11) lands.
		{name: "207539: Core's reconnect backoff and default retry", outage: "offline", outFrom: 100, outFor: 82, readAt: read207539,
			cmd: 187, setW: 814, forcedTo: 1201, dispatchFrom: read207539 + 20, want: true},
		{name: "past the registry's 30 s retry", outage: "offline", outFrom: 100, outFor: 82, readAt: read207539,
			cmd: 187, setW: 814, forcedTo: 1201, dispatchFrom: read207539 + 47, want: false},
		// Core never reads the inverter again: the count starts at the
		// backoff's cap after the return all the same.
		{name: "Core never reads it again", outage: "offline", outFrom: 100, outFor: 82, readAt: 1201,
			cmd: 187, setW: 814, forcedTo: 1201, dispatchFrom: 290, want: false},
		{name: "FTW dispatches the discharge", cmd: 187, setW: 3000, forcedTo: 1201, dispatchFrom: 1, want: true},
		{name: "forced with no dispatch and no outage", cmd: 187, setW: 3000, forcedTo: 1201, dispatchFrom: 500, want: false},
		// Item 10, 207538's restart at 625.5 s: SIGTERM, the driver's
		// cleanup writes 204 within a second, Core is back 14.6 s later
		// and dispatches 6 s after that.
		{name: "207538: SIGTERM restart writes the default", stop: 100, up: 115, cmd: 187, setW: 868,
			forcedTo: 101, dispatchFrom: 121, want: true},
		// The same stop as SIGKILL: no cleanup, the discharge runs on
		// while Core is down.
		{name: "restart without cleanup", stop: 100, up: 115, cmd: 187, setW: 868,
			forcedTo: 121, dispatchFrom: 121, want: false},
		// 207530: Core stops while the inverter is offline (888.7-916.4
		// s), so no cleanup can land. The inverter is back 2.8 s before
		// Core (here 13 s), and the default lands as Core starts.
		{name: "207530: restart while the inverter is offline", outage: "offline", outFrom: 90, outFor: 12, readAt: 116,
			stop: 100, up: 115, cmd: 170, setW: 2983, forcedTo: 116, dispatchFrom: 125, want: true},
		// A Core that starts fresh has no default retry to wait for: 10 s
		// from its first good read, not the 40 s an outage gets.
		{name: "forced 8 s after Core starts", stop: 100, up: 101, cmd: 187, setW: 868,
			forcedTo: 1201, dispatchFrom: 110, want: true},
		{name: "forced 23 s after Core starts", stop: 100, up: 101, cmd: 187, setW: 868,
			forcedTo: 1201, dispatchFrom: 125, want: false},
		// The inverter is back at 102 while Core, stopped at 100 during the
		// outage, stays down until 150. Core's first read is at 151 and the
		// default lands 7 s later. While Core is down nothing reads the
		// inverter, so the count cannot start before 151.
		{name: "inverter back while Core is down for 48 s", outage: "offline", outFrom: 90, outFor: 12, readAt: 151,
			stop: 100, up: 150, cmd: 187, setW: 3000, forcedTo: 158, dispatchFrom: 170, want: true},
	} {
		if got := judged(c.run())[forced]; got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, forced, got, c.want)
		}
	}
}

// R8 with two inverter outages: the count after the second starts at its
// own return and Core's first read after it, not the first outage's.
func TestInvariantsForcedBatteryCountsFromTheLatestReturn(t *testing.T) {
	const forced = "no forced battery mode FTW does not dispatch"
	// Outage 1 100-120, Core reads at 125 and dispatches 130-300. Outage 2
	// 300-340, Core reads at 380 and the default lands at 388.
	run := func(forcedTo float64) *runData {
		d := randomRun(601, 16, 5, func(i int, r *sample) {
			tt := float64(i)
			if tt < forcedTo {
				r.Bat.EMSMode, r.Bat.EMSCmd, r.Bat.EMSSetW = 2, 187, 3000
			} else {
				r.Bat.EMSCmd = 204
			}
			if (tt >= 100 && tt < 120) || (tt >= 300 && tt < 340) {
				off := false
				r.Bat.Running = &off
			}
			errs := 0
			if (tt >= 100 && tt < 125) || (tt >= 300 && tt < 380) {
				errs = 3
			}
			r.Status = &ftwStatus{Drivers: map[string]ftwDriver{inverterDriver: {Status: "ok", ConsecutiveErrors: &errs}}}
			if tt < 100 || (tt >= 130 && tt < 300) {
				r.Status.Dispatch = []ftwDispatch{{Driver: inverterDriver, TargetW: -3000}}
			}
		})
		d.events = []houseEventLine{
			{T: 100, Event: houseEvent{Do: "offline", Device: "inverter", ForS: 20}},
			{T: 300, Event: houseEvent{Do: "offline", Device: "inverter", ForS: 40}},
		}
		return d
	}
	if !judged(run(388))[forced] {
		t.Errorf("default 8 s after Core reads again after the second outage: got fail, want pass")
	}
	if judged(run(1201))[forced] {
		t.Errorf("forced to the end after the second outage: got pass, want fail")
	}
}

// R8 forgives the seconds Core's reconnect backoff and default retry hold
// its first write after a return (reconnectBackoffMaxS, defaultRetryMaxS).
// The triage line is 10 s from the return, so the judge records what it
// forgave beyond that line instead of hiding it.
func TestInvariantsRecordForcedSecondsInsideCoreTimers(t *testing.T) {
	// 207539's shape: back at 182, Core reads 4 s before the backoff's cap
	// ends (238 with 60 s) and dispatches 7 s later, inside the backoff
	// alone. Forced with no dispatch until then; past 182+10 that is from
	// 193 (52 s with 60 s).
	read := 182 + reconnectBackoffMaxS - 4
	c := forcedCase{outage: "offline", outFrom: 100, outFor: 82, readAt: read,
		cmd: 187, setW: 814, forcedTo: 1201, dispatchFrom: read + 7}
	m, _ := judgeInvariants(c.run())
	if in, past, want := m["battery_forced_in_core_timers_s"], m["battery_forced_past_allowance_s"], read+6-192; in != want || past != 0.0 {
		t.Errorf("in Core's timers %v s, past the allowance %v s; want %v and 0", in, past, want)
	}
	// Forced to 299: from 193 to the allowance's end inside the timers,
	// the rest past them.
	c.dispatchFrom = 300
	m, _ = judgeInvariants(c.run())
	end := read + defaultRetryMaxS + forcedBudgetS
	if in, past := m["battery_forced_in_core_timers_s"], m["battery_forced_past_allowance_s"]; in != end-192 || past != 299-end {
		t.Errorf("in Core's timers %v s, past the allowance %v s; want %v and %v", in, past, end-192, 299-end)
	}
}

// R8's allowances are Core's own timers. Read them from Core's source so
// they cannot drift. Once the registry retries a failed default on the
// first good poll after an outage (#1915, C11), defaultRetryMaxS must be
// 0: from then on the 30 s the judge gives today is no longer Core's.
func TestForcedAllowancesMatchCore(t *testing.T) {
	src := func(p string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "internal", p))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	seconds := func(code, name string) float64 {
		m := regexp.MustCompile(name + `\s*=\s*(\d+)\s*\*\s*time\.Second`).FindStringSubmatch(code)
		if m == nil {
			t.Fatalf("no %s = N * time.Second in Core's source", name)
		}
		n, _ := strconv.Atoi(m[1])
		return float64(n)
	}
	// reconnectBackoffMaxS is read from Core's source; the 207539 shapes
	// below need a backoff that can hold Core's first read back past 4 s.
	if reconnectBackoffMaxS < 5 {
		t.Errorf("Core's reconnectBackoffMax is %v s; rework the 207539 shapes", reconnectBackoffMaxS)
	}
	registry := src("drivers/registry.go")
	// Today the registry sends the default on a failure and on its retry
	// timer: two calls. #1915 adds a third, on the first good poll.
	calls := strings.Count(registry, "attemptDefault()")
	switch {
	case calls == 2 && defaultRetryMaxS != seconds(registry, "defaultRetryMax"):
		t.Errorf("defaultRetryMaxS = %v, Core's defaultRetryMax is %v s", defaultRetryMaxS, seconds(registry, "defaultRetryMax"))
	case calls > 2 && defaultRetryMaxS != 0:
		t.Errorf("Core's registry calls attemptDefault() in %d places, not 2. If it now retries "+
			"the default on the first good poll after an outage (#1915, C11), set defaultRetryMaxS "+
			"to 0 and turn the R8 cases that lean on it to fail. Otherwise update this check.", calls)
	case calls < 2:
		t.Errorf("Core's registry calls attemptDefault() in %d places; update this check", calls)
	}
}

// TestJudge also judges the random seeds a results directory holds, from
// the plan each run wrote.
func TestRecordedRandomFindsSeedRuns(t *testing.T) {
	dir := t.TempDir()
	for _, seed := range []int64{7, 9} {
		b, _ := json.Marshal(drawPlan(seed))
		os.MkdirAll(filepath.Join(dir, fmt.Sprintf("random-%d", seed)), 0o755)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("random-%d", seed), "plan.json"), b, 0o644)
	}
	var got []string
	for _, sc := range recordedRandom(dir) {
		got = append(got, sc.name+"@"+sc.dir)
	}
	if len(got) != 2 || got[0] != "r7@random-7" || got[1] != "r9@random-9" {
		t.Fatalf("recordedRandom = %v", got)
	}
}

// R2 after the second review of #1920: a house event that is no fault
// extends a step's stretch only if the worst phase rose 0.5 A or more
// within 3 s of it. 16 A fuse, the battery at its floor and a 40 s charger,
// so a step allows 45 s; 20 A on L1 (1.25 x) from 12 s, after a load step
// at 10 s.
func TestInvariantsOnlyARiseExtendsAStretch(t *testing.T) {
	for _, c := range []struct {
		name   string
		to     int     // 20 A on L1 until here
		riseAt int     // L1 rises to 21 A from here; 0: never
		every  float64 // a load event that changes nothing, this often; 0: none
		want   bool
	}{
		{"a load event at 50 s raises L1", 80, 50, 0, true},
		{"load events every 40 s change nothing", 290, 0, 40, false},
	} {
		d := randomRun(301, 16, 8, func(i int, r *sample) {
			r.Bat.SoCPct = 6
			if i >= 12 && i < c.to {
				r.PhaseA = [3]float64{20, 12, 12}
				if c.riseAt > 0 && i >= c.riseAt {
					r.PhaseA[0] = 21
				}
			}
		})
		withCharger(t, d, 40, 0)
		d.events = []houseEventLine{{T: 10, Event: houseEvent{Do: "loads"}}}
		if c.riseAt > 0 {
			d.events = append(d.events, houseEventLine{T: float64(c.riseAt), Event: houseEvent{Do: "loads"}})
		}
		for t0 := 50.0; c.every > 0 && t0 < 290; t0 += c.every {
			d.events = append(d.events, houseEventLine{T: t0, Event: houseEvent{Do: "loads"}})
		}
		if got := judged(d)[limitStep]; got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, limitStep, got, c.want)
		}
	}
}

// The inverter is back at the end of its outage: its return is no step
// unless the current rises, and then the battery answers in 2 s, not the
// charger. 16 A fuse, a 40 s charger, the inverter out from 10 to 40 s;
// the outage allows until 55 s.
func TestInvariantsTheInverterIsBackAtItsReturn(t *testing.T) {
	for _, c := range []struct {
		name     string
		from, to int // 20 A on L1 in [from, to)
	}{
		{"over from 12 to 80 s, the return changes nothing", 12, 80},
		{"the current rises at the return, over until 60 s", 40, 60},
	} {
		d := randomRun(121, 16, 8, func(i int, r *sample) {
			run := !(i >= 10 && i < 40)
			r.Bat.Running = &run
			if i >= c.from && i < c.to {
				r.PhaseA = [3]float64{20, 12, 12}
			}
		})
		withCharger(t, d, 40, 0)
		d.events = []houseEventLine{
			{T: 10, Event: houseEvent{Do: "offline", Device: "inverter", ForS: 30}},
			{T: 40, Event: houseEvent{Do: "online", Device: "inverter"}},
		}
		if judged(d)[limitStep] {
			t.Errorf("%s: %q passed", c.name, limitStep)
		}
	}
}

// R4 after the second review of #1920: a car raise FTW sends after a
// step's meter delay, or after a Core restart, is FTW's own; one sent
// before the meter shows the step is part of the step. 25 A fuse, the
// battery at its floor and a 40 s charger; the phases sit at 31 A
// (1.24 x) from 50 to 80 s.
func TestInvariantsARaiseAfterAStepIsFTWsOwn(t *testing.T) {
	for _, c := range []struct {
		name    string
		raiseAt float64
		event   bool // PV moves at 40 s
		restart bool // Core restarts at 30 s and is back at 35 s
		want    bool // both limit criteria hold
	}{
		{"FTW raises 5 s after PV moved", 45, true, false, false},
		{"FTW raises 1 s after PV moved", 41, true, false, true},
		{"FTW raises 15 s after Core is back", 50, false, true, false},
	} {
		d := randomRun(121, 25, 10, func(i int, r *sample) {
			r.Bat.SoCPct = 6
			if i >= 50 && i < 80 {
				r.PhaseA = [3]float64{31, 31, 31}
				r.CarW[0] = 11040
			}
		})
		withCharger(t, d, 40, 0)
		d.frames = []frame{profileAt(20, 0), profileAt(c.raiseAt, 16)}
		if c.event {
			d.events = []houseEventLine{{T: 40, Event: houseEvent{Do: "pv", Device: "inverter"}}}
		}
		if c.restart {
			d.allMarks = []markLine{{T: 30, Label: "ftw:restart"}, {T: 35, Label: "ftw:up"}}
		}
		v := judged(d)
		if got := v[limitStep] && v[limitOwn]; got != c.want {
			t.Errorf("%s: step %v, own %v; want both %v", c.name, v[limitStep], v[limitOwn], c.want)
		}
	}
}

// A Core restart explains a stretch for 60 s from the restart. 16 A fuse,
// Core restarts at 50 s and is back at 55 s; 20 A on L1 (1.25 x).
func TestInvariantsARestartExplainsAMinute(t *testing.T) {
	for _, c := range []struct {
		name     string
		from, to int
		want     bool
	}{
		{"over from 100 to 108 s", 100, 108, true},
		{"over from 115 to 125 s", 115, 125, false},
	} {
		d := randomRun(201, 16, 8, func(i int, r *sample) {
			if i >= c.from && i < c.to {
				r.PhaseA = [3]float64{20, 12, 12}
			}
		})
		withCharger(t, d, 5, 0)
		d.allMarks = []markLine{{T: 50, Label: "ftw:restart"}, {T: 55, Label: "ftw:up"}}
		v := judged(d)
		if got := v[limitStep] && v[limitOwn]; got != c.want {
			t.Errorf("%s: step %v, own %v; want both %v", c.name, v[limitStep], v[limitOwn], c.want)
		}
	}
}

// For export the battery's spare is what it can charge more: a battery
// charging 4 kW has 1 kW, too little for a 4 A overage, so the 5 s charger
// answers; one discharging 4 kW has 9 kW and answers in 2 s, even at its
// floor. 16 A fuse; -20 A on L1 from 50 to 59 s after a step at 49.5 s.
func TestInvariantsBatteryLeverForExport(t *testing.T) {
	for _, c := range []struct {
		name   string
		batW   float64
		socPct float64
		want   bool
	}{
		{"charging 4 kW", 4000, 50, true},
		{"discharging 4 kW", -4000, 50, false},
		{"idle at its floor", 0, 6, false},
	} {
		d := randomRun(101, 16, -8, func(i int, r *sample) {
			r.BatW, r.Bat.SoCPct = c.batW, c.socPct
			if i >= 50 && i < 59 {
				r.PhaseA = [3]float64{-20, -12, -12}
			}
		})
		withCharger(t, d, 5, 0)
		d.events = []houseEventLine{{T: 49.5, Event: houseEvent{Do: "pv", Device: "inverter"}}}
		if got := judged(d)[limitStep]; got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, limitStep, got, c.want)
		}
	}
}

// A dip under the fuse keeps the stretch open while the heat stays over
// it, but a new step after the dip still gets its own time (2 + 5 s for
// the battery), after a step or after FTW's own raise that was cut in
// time. A step held past its time fails as a step. 16 A fuse at 15.95 A,
// so the heat stays over the fuse for a minute after a few seconds at 20 A
// on L1.
func TestInvariantsAStepAfterADipGetsItsTime(t *testing.T) {
	for _, c := range []struct {
		name              string
		first             []houseEventLine // the change before 50 s; none: FTW's own
		stepTo            int              // the step at 80 s holds 20 A until here
		wantStep, wantOwn bool
	}{
		{"after a step", []houseEventLine{{T: 49.5, Event: houseEvent{Do: "loads"}}}, 84, true, true},
		{"after FTW's own raise", nil, 84, true, true},
		{"held 8 s after FTW's own raise", nil, 88, false, true},
	} {
		d := randomRun(201, 16, 15.95, func(i int, r *sample) {
			if (i >= 50 && i < 53) || (i >= 80 && i < c.stepTo) {
				r.PhaseA[0] = 20
			}
		})
		withCharger(t, d, 5, 0)
		d.events = append(c.first, houseEventLine{T: 80, Event: houseEvent{Do: "loads"}})
		d.warm()
		if h := d.at(79).HeatA[0]; h <= 16 {
			t.Fatalf("%s: heat %.2f A at 79 s, want over the fuse", c.name, h)
		}
		if v := judged(d); v[limitStep] != c.wantStep || v[limitOwn] != c.wantOwn {
			t.Errorf("%s: step %v, own %v; want %v, %v", c.name, v[limitStep], v[limitOwn], c.wantStep, c.wantOwn)
		}
	}
}

// R4 after the third review of #1920: a car or battery raise FTW sends
// while an import stretch is over the fuse is FTW's own, after a step's
// meter delay too, and a house event that changes nothing does not take
// the rise from it. One sent in a dip under the fuse, with the heat still
// over it, is allowed: the step after it gets its time. Raising the car
// into export is the right answer. 40 s charger, the battery at its
// floor.
func TestInvariantsARaiseOverTheFuseIsFTWsOwn(t *testing.T) {
	for _, c := range []struct {
		name   string
		fuse   float64
		set    func(i int, r *sample)
		frames []frame
		events []houseEventLine
		want   bool // both limit criteria hold
	}{
		{"0 -> 16 A at 1.24 x In after PV moved", 25, func(i int, r *sample) {
			if i >= 41 && i < 84 {
				r.PhaseA = [3]float64{31, 31, 31}
			}
			if i >= 52 && i < 84 {
				r.PhaseA = [3]float64{42, 42, 42}
			}
		}, []frame{profileAt(20, 0), profileAt(50, 16)},
			[]houseEventLine{{T: 40, Event: houseEvent{Do: "pv", Device: "inverter"}}}, false},
		{"6 -> 10 A at 1.25 x In, a load event that changes nothing as it lands", 16, func(i int, r *sample) {
			if i >= 11 && i < 75 {
				r.PhaseA = [3]float64{20, 12, 12}
			}
			if i >= 32 && i < 75 {
				r.PhaseA[0] = 22
			}
		}, []frame{profileAt(1, 6), profileAt(30, 10)},
			[]houseEventLine{{T: 10, Event: houseEvent{Do: "loads"}}, {T: 31, Event: houseEvent{Do: "loads"}}}, false},
		{"battery charge 0 -> 3 kW at 1.25 x In after a load step", 16, func(i int, r *sample) {
			r.Status = &ftwStatus{Dispatch: []ftwDispatch{{Driver: "sungrow", TargetW: 0}}}
			if i >= 30 {
				r.Status.Dispatch[0].TargetW, r.BatW = 3000, 3000
			}
			if i >= 11 && i < 55 {
				r.PhaseA = [3]float64{20, 12, 12}
			}
			if i >= 32 && i < 55 {
				r.PhaseA = [3]float64{22, 16, 16}
			}
		}, nil, []houseEventLine{{T: 10, Event: houseEvent{Do: "loads"}}}, false},
		{"car and battery raised in a dip under the fuse, a step after it", 16, func(i int, r *sample) {
			r.Status = &ftwStatus{Dispatch: []ftwDispatch{{Driver: "sungrow", TargetW: 0}}}
			if i >= 56 {
				r.Status.Dispatch[0].TargetW = 2000
			}
			switch {
			case i >= 11 && i < 45, i >= 71 && i < 76:
				r.PhaseA = [3]float64{22, 12, 12}
			case i >= 45 && i < 71:
				r.PhaseA = [3]float64{15, 12, 12}
			}
		}, []frame{profileAt(1, 6), profileAt(55, 8)},
			[]houseEventLine{{T: 10, Event: houseEvent{Do: "loads"}}, {T: 70, Event: houseEvent{Do: "loads"}}}, true},
		{"0 -> 16 A into export over the fuse after PV moved", 16, func(i int, r *sample) {
			r.PhaseA, r.BatW = [3]float64{-8, -8, -8}, 4000
			if i >= 11 && i < 45 {
				r.PhaseA = [3]float64{-24, -12, -12}
			}
		}, []frame{profileAt(1, 0), profileAt(20, 16)},
			[]houseEventLine{{T: 10, Event: houseEvent{Do: "pv", Device: "inverter"}}}, true},
	} {
		d := randomRun(121, c.fuse, 12, func(i int, r *sample) {
			r.Bat.SoCPct = 6
			c.set(i, r)
		})
		withCharger(t, d, 40, 0)
		d.frames, d.events = c.frames, c.events
		v := judged(d)
		if got := v[limitStep] && v[limitOwn]; got != c.want {
			t.Errorf("%s: step %v, own %v; want both %v", c.name, v[limitStep], v[limitOwn], c.want)
		}
	}
}

// R4 for the battery: a charge raise FTW sends after a Core restart is
// its own, as a car raise is (triage C2). An order moved while the
// inverter is off reaches no battery. 16 A fuse, Core restarts at 50 s
// and is back at 55 s; the order goes 0 -> 4 kW at 60 s; 21 A on L1
// (1.31 x) from 61 to 105 s.
func TestInvariantsABatteryRaiseAfterARestartIsFTWsOwn(t *testing.T) {
	for _, c := range []struct {
		name    string
		running bool // the inverter runs from 55 to 70 s
		want    bool
	}{
		{"the inverter runs", true, false},
		{"the inverter is off", false, true},
	} {
		d := randomRun(201, 16, 8, func(i int, r *sample) {
			w := 0.0
			if i >= 60 {
				w = 4000
			}
			r.Status = &ftwStatus{Dispatch: []ftwDispatch{{Driver: "sungrow", TargetW: w}}}
			if i >= 55 && i < 70 {
				run := c.running
				r.Bat.Running = &run
			}
			if i >= 61 && c.running {
				r.BatW = w
			}
			if i >= 61 && i < 105 {
				r.PhaseA = [3]float64{21, 13, 13}
			}
		})
		withCharger(t, d, 5, 0)
		d.allMarks = []markLine{{T: 50, Label: "ftw:restart"}, {T: 55, Label: "ftw:up"}}
		v := judged(d)
		if got := v[limitStep] && v[limitOwn]; got != c.want {
			t.Errorf("%s: step %v, own %v; want both %v", c.name, v[limitStep], v[limitOwn], c.want)
		}
	}
}

// Fourth review of #1920: what FTW's battery order did, read correctly.
// While Core is down, or before its first tick, status has no dispatch,
// but the inverter keeps the last order: a missing order is no move. A
// move counts from 0.5 A a phase (345 W), as raisedAt does, so a one-
// sample 158 W wobble of Core's closed loop (seed 207539 at 151.7 s) is
// no raise. 16 A fuse, a load step at 40 s puts 20 A on L1.
func TestInvariantsABatteryOrderMovesOnlyWhenFTWMovesIt(t *testing.T) {
	order := func(r *sample, w float64) {
		r.Status = &ftwStatus{Dispatch: []ftwDispatch{{Driver: "sungrow", TargetW: w}}}
	}
	for _, c := range []struct {
		name string
		set  func(i int, r *sample)
		down bool // Core restarts at 44 s and is back at 49 s
		want bool // both limit criteria hold
	}{
		// At 6 % SoC the battery charging 1 kW cannot take 4 A a phase
		// off, so the charger is the lever; Core's restart extends the
		// stretch to 104 s.
		{"1 kW order kept while Core is down", func(i int, r *sample) {
			r.BatW, r.Bat.SoCPct = 1000, 6
			order(r, 1000)
			if i >= 41 && i < 80 {
				r.PhaseA = [3]float64{20, 8, 8}
			}
		}, true, true},
		{"1 kW order missing while Core is down, then restored", func(i int, r *sample) {
			r.BatW, r.Bat.SoCPct = 1000, 6
			if i < 44 || i >= 50 {
				order(r, 1000)
			}
			if i >= 41 && i < 80 {
				r.PhaseA = [3]float64{20, 8, 8}
			}
		}, true, true},
		{"0 W order missing while Core is down, then 1 kW", func(i int, r *sample) {
			r.Bat.SoCPct = 6
			switch {
			case i < 44:
				order(r, 0)
			case i >= 50:
				order(r, 1000)
				r.BatW = 1000
			}
			if i >= 41 && i < 80 {
				r.PhaseA = [3]float64{20, 8, 8}
			}
		}, true, false},
		// The battery discharges 4 999 W and answers in 2 s: allowed
		// until 47 s.
		{"steady discharge order", func(i int, r *sample) {
			r.BatW = -4999
			order(r, -4999)
			if i >= 41 && i < 46 {
				r.PhaseA = [3]float64{20, 8, 8}
			}
		}, false, true},
		{"one 158 W wobble toward import", func(i int, r *sample) {
			r.BatW = -4999
			order(r, -4999)
			if i == 43 {
				order(r, -4841)
			}
			if i >= 41 && i < 46 {
				r.PhaseA = [3]float64{20, 8, 8}
			}
		}, false, true},
		{"one 360 W move toward import", func(i int, r *sample) {
			r.BatW = -4999
			order(r, -4999)
			if i == 43 {
				order(r, -4639)
			}
			if i >= 41 && i < 46 {
				r.PhaseA = [3]float64{20, 8, 8}
			}
		}, false, false},
	} {
		d := randomRun(121, 16, 8, c.set)
		withCharger(t, d, 40, 0)
		d.events = []houseEventLine{{T: 40, Event: houseEvent{Do: "loads"}}}
		if c.down {
			d.allMarks = []markLine{{T: 44, Label: "ftw:restart"}, {T: 49, Label: "ftw:up"}}
		}
		m, _ := judgeInvariants(d)
		v := judged(d)
		if got := v[limitStep] && v[limitOwn]; got != c.want {
			t.Errorf("%s: step %v, own %v; want both %v; judged %v", c.name, v[limitStep], v[limitOwn], c.want, m["over_limit_judged"])
		}
	}
}

// Export is judged by what pushes export. A car raise before an export
// stretch soaks it up, so the stretch keeps its step's time. A battery
// pushed toward export after a Core restart is FTW's own push: the
// restart's minute does not excuse it.
func TestInvariantsExportIsFTWsOwnOnlyWhenFTWPushedExport(t *testing.T) {
	t.Run("car raise before an export stretch", func(t *testing.T) {
		off := false
		d := randomRun(121, 16, 0, func(i int, r *sample) {
			r.Bat.Running = &off
			if i >= 45 && i < 80 {
				r.PhaseA = [3]float64{-20, -20, -20}
			}
		})
		withCharger(t, d, 40, 0)
		d.frames = []frame{profileAt(20, 0), profileAt(43, 16)}
		d.events = []houseEventLine{{T: 40, Event: houseEvent{Do: "pv", Device: "inverter"}}}
		m, _ := judgeInvariants(d)
		if v := judged(d); !(v[limitStep] && v[limitOwn]) {
			t.Errorf("step %v, own %v; judged %v", v[limitStep], v[limitOwn], m["over_limit_judged"])
		}
	})
	for _, c := range []struct {
		name    string
		running bool // the inverter runs from 55 s
		want    bool
	}{
		{"battery pushed 0 -> -5 kW after a restart", true, false},
		{"the same order with the inverter off", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := randomRun(201, 16, 0, func(i int, r *sample) {
				w := 0.0
				if i >= 57 {
					w = -5000
				}
				r.Status = &ftwStatus{Dispatch: []ftwDispatch{{Driver: "sungrow", TargetW: w}}}
				if i >= 55 && !c.running {
					run := false
					r.Bat.Running = &run
				}
				if i >= 58 && c.running {
					r.BatW = w
				}
				if i >= 58 && i < 100 {
					r.PhaseA = [3]float64{-21, -10, -10}
				}
			})
			withCharger(t, d, 5, 0)
			d.allMarks = []markLine{{T: 50, Label: "ftw:restart"}, {T: 55, Label: "ftw:up"}}
			m, _ := judgeInvariants(d)
			v := judged(d)
			if got := v[limitStep] && v[limitOwn]; got != c.want {
				t.Errorf("step %v, own %v; want both %v; judged %v", v[limitStep], v[limitOwn], c.want, m["over_limit_judged"])
			}
		})
	}
	// Inside an export stretch after a PV step. The battery charges at
	// its maximum, so the 40 s charger is the lever. Moving its order
	// toward export while the meter is over the fuse is FTW's own; the
	// same move in a dip under the fuse, with a step after it, is not.
	for _, c := range []struct {
		name   string
		moveAt int // the order goes 5 -> 4 kW; 0: no move
		dip    bool
		want   bool
	}{
		{"no move", 0, false, true},
		{"battery moved toward export over the fuse", 20, false, false},
		{"the same move in a dip, a step after it", 56, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := randomRun(121, 16, -12, func(i int, r *sample) {
				r.BatW = rigBatteryMaxW
				w := rigBatteryMaxW
				if c.moveAt > 0 && i >= c.moveAt {
					w = 4000
				}
				r.Status = &ftwStatus{Dispatch: []ftwDispatch{{Driver: "sungrow", TargetW: w}}}
				switch {
				case !c.dip && i >= 11 && i < 45:
					r.PhaseA = [3]float64{-22, -12, -12}
				case c.dip && (i >= 11 && i < 45 || i >= 71 && i < 76):
					r.PhaseA = [3]float64{-22, -12, -12}
				case c.dip && i >= 45 && i < 71:
					r.PhaseA = [3]float64{-15, -12, -12}
				}
			})
			withCharger(t, d, 40, 0)
			d.events = []houseEventLine{{T: 10, Event: houseEvent{Do: "pv", Device: "inverter"}}}
			if c.dip {
				d.events = append(d.events, houseEventLine{T: 70, Event: houseEvent{Do: "pv", Device: "inverter"}})
			}
			m, _ := judgeInvariants(d)
			v := judged(d)
			if got := v[limitStep] && v[limitOwn]; got != c.want {
				t.Errorf("step %v, own %v; want both %v; judged %v", v[limitStep], v[limitOwn], c.want, m["over_limit_judged"])
			}
		})
	}
}

// The line is In, not 1.2 x In: a car raise sent while the meter shows
// 1.1 x In (under the instant cut, the heat still cool) is sent over the
// line, and the stretch is FTW's own.
func TestInvariantsARaiseBetweenFuseAndInstantIsOwn(t *testing.T) {
	d := randomRun(121, 25, 12, func(i int, r *sample) {
		r.Bat.SoCPct = 6
		if i >= 41 && i < 84 {
			r.PhaseA = [3]float64{27.5, 27.5, 27.5}
		}
		if i >= 52 && i < 84 {
			r.PhaseA = [3]float64{42, 42, 42}
		}
	})
	withCharger(t, d, 40, 0)
	d.frames = []frame{profileAt(20, 0), profileAt(50, 16)}
	d.events = []houseEventLine{{T: 40, Event: houseEvent{Do: "pv", Device: "inverter"}}}
	m, _ := judgeInvariants(d)
	if v := judged(d); v[limitStep] && v[limitOwn] {
		t.Errorf("a raise sent at 1.1 x In passed; judged %v", m["over_limit_judged"])
	}
}
