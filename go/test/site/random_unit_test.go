//go:build unix

package site

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// 27.84 A held until the heat passes the fuse, and 25.5 A on a fuse already
// warm at its rating.
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
// every 7 s. Both fail after 3 s.
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
	dips := randomRun(601, 25, 25.5, func(i int, r *sample) {
		if i%7 == 6 {
			r.PhaseA = [3]float64{24.9, 24.9, 24.9}
		}
	})
	for name, d := range map[string]*runData{"export 6 s at 1.31 x In": export, "600 s on a warm fuse, with dips": dips} {
		withCharger(t, d, 5, 0)
		if v := judged(d); v[limitOwn] {
			t.Errorf("%s: %q passed", name, limitOwn)
		}
	}
}

// A window that opens inside FTW's own raise does not turn it into a step:
// FTW raises the car at 45 s and the phases sit at 31 A (1.24 x 25 A) from
// 50 to 60 s; PV rising at 51 s changes nothing. The battery is at its
// floor, so the 40 s charger would be a step's lever.
func TestInvariantsAnEventInsideFTWsRaiseLeavesItOwn(t *testing.T) {
	for _, withEvent := range []bool{false, true} {
		d := randomRun(121, 25, 10, func(i int, r *sample) {
			r.Bat.SoCPct = 6
			if i >= 50 && i < 60 {
				r.PhaseA = [3]float64{31, 31, 31}
			}
		})
		withCharger(t, d, 40, 0)
		d.frames = []frame{profileAt(40, 0), profileAt(45, 16)}
		if withEvent {
			d.events = []houseEventLine{{T: 51, Event: houseEvent{Do: "pv", Device: "inverter"}}}
		}
		if v := judged(d); v[limitOwn] {
			t.Errorf("event %v: FTW's raise held 10 s at 1.24 x In and passed", withEvent)
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

// R8, C1: once the inverter and Core are back, a battery FTW does not
// dispatch leaves its forced mode within 10 s.
func TestInvariantsNoForcedBatteryWithoutDispatch(t *testing.T) {
	const forced = "no forced battery mode FTW does not dispatch"
	for _, c := range []struct {
		name    string
		forcedS int     // the register holds 187 / 3 kW until here
		orderW  float64 // FTW's dispatch; 0: none
		down    bool    // Core, not the inverter, is away from 10 to 40 s
		want    bool
	}{
		{"8 s after the inverter is back", 48, 0, false, true},
		{"30 s after the inverter is back", 70, 0, false, false},
		{"FTW dispatches the discharge", 120, -3000, false, true},
		{"8 s after Core is back", 48, 0, true, true},
		{"30 s after Core is back", 70, 0, true, false},
	} {
		d := randomRun(121, 16, 5, func(i int, r *sample) {
			r.Status = &ftwStatus{}
			if c.orderW != 0 {
				r.Status.Dispatch = []ftwDispatch{{Driver: "sungrow", TargetW: c.orderW}}
			}
			if i < c.forcedS {
				r.Bat.EMSMode, r.Bat.EMSCmd, r.Bat.EMSSetW = 2, 187, 3000
			}
		})
		if c.down {
			d.allMarks = []markLine{{T: 10, Label: "ftw:restart"}, {T: 40, Label: "ftw:up"}}
		} else {
			d.events = []houseEventLine{{T: 10, Event: houseEvent{Do: "offline", Device: "inverter", ForS: 30}}}
		}
		if got := judged(d)[forced]; got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, forced, got, c.want)
		}
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
