//go:build unix

package site

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Unit tests for the R1-R5 judges, on records made up here. They run in
// every go test, without a simulator.

// synth is n one-second samples from t = 0: 10 A per phase, the car idle,
// the battery not forced. set changes sample i.
func synth(n int, marks map[string]float64, meta map[string]any, set func(i int, r *sample)) *runData {
	d := &runData{marks: marks, meta: meta}
	for i := 0; i < n; i++ {
		r := &sample{T: float64(i), PhaseA: [3]float64{10, 10, 10}, CarW: []float64{0}}
		if set != nil {
			set(i, r)
		}
		d.samples = append(d.samples, r)
	}
	return d
}

// order sets the battery's EMS registers to Core's order w (+ charge).
func order(r *sample, w float64) {
	switch {
	case w > 0:
		r.Bat.EMSMode, r.Bat.EMSCmd, r.Bat.EMSSetW = 2, 170, w
	case w < 0:
		r.Bat.EMSMode, r.Bat.EMSCmd, r.Bat.EMSSetW = 2, 187, -w
	default:
		r.Bat.EMSMode, r.Bat.EMSCmd, r.Bat.EMSSetW = 0, 204, 0
	}
}

// profileAt is a SetChargingProfile of limitA per phase on connector 1,
// sent at t.
func profileAt(t, limitA float64) frame {
	b, _ := json.Marshal([]any{2, "id", "SetChargingProfile", map[string]any{"connectorId": 1,
		"csChargingProfiles": map[string]any{"chargingProfilePurpose": "TxDefaultProfile",
			"chargingSchedule": map[string]any{"chargingRateUnit": "A",
				"chargingSchedulePeriod": []any{map[string]any{"limit": limitA}}}}}})
	return frame{T: t, Dir: "cs->cp", Frame: b}
}

func verdictsOf(cs []criterion) map[string]bool {
	out := map[string]bool{}
	for _, c := range cs {
		out[c.Name] = c.OK
	}
	return out
}

func TestBatOrderReadsTheForcedCommand(t *testing.T) {
	for _, w := range []float64{3000, -2500, 0} {
		r := &sample{}
		order(r, w)
		if got := batOrder(r); got != w {
			t.Errorf("order %v read as %v", w, got)
		}
	}
	r := &sample{Bat: plantDevice{EMSMode: 0, EMSCmd: 187, EMSSetW: 3000}}
	if batOrder(r) != 0 || batForced(r) {
		t.Fatal("a discharge command outside forced mode is no order: the inverter runs on its own")
	}
}

func TestOfferRisesCountsOnlyRaisesInTheWindow(t *testing.T) {
	d := &runData{frames: []frame{profileAt(10, 8), profileAt(20, 10), profileAt(30, 6), profileAt(40, 7), profileAt(60, 16)}}
	if n := d.offerRises(15, 50); n != 2 {
		t.Fatalf("rises in [15, 50): %d, want 2 (8→10 A at 20 s, 6→7 A at 40 s)", n)
	}
	if p := d.firstOffer(25, func(p profile) bool { return p.Limit < 7 }); p == nil || *p != 5 {
		t.Fatalf("first offer under 7 A from 25 s: %v, want 5 s", p)
	}
}

// r1Run is R1's record: the meter silent from 100 to 220 s and offline from
// 340 to 430 s. Core discharges 3 kW until the silence and hands the
// battery back and stands the charger down 40 s into it; the meter reads
// degraded while silent; Charge now returns 30 s after each return. orderAt
// is Core's battery order during the silence before the hand-back.
func r1Run(orderAt func(t float64) float64) *runData {
	marks := map[string]float64{"charge_now": 0, "silent": 100, "back": 220, "offline": 340, "online": 430, "end": 550}
	d := synth(550, marks, map[string]any{"fuse": 25.0}, func(i int, r *sample) {
		t := float64(i)
		switch {
		case t <= 100:
			order(r, -3000)
		case t < 140:
			order(r, orderAt(t))
		}
		meter := "ok"
		if (t > 102 && t < 220) || (t > 342 && t < 430) {
			meter = "degraded"
		}
		r.Status = &ftwStatus{Drivers: map[string]ftwDriver{"meter": {Status: meter}}}
		if t < 140 || (t >= 250 && t < 380) || t >= 460 {
			r.CarW[0] = 11040
		}
	})
	d.frames = []frame{profileAt(50, 16), profileAt(140, 0), profileAt(240, 16), profileAt(390, 0), profileAt(450, 16)}
	return d
}

func TestJudgeR1(t *testing.T) {
	_, cs := judgeR1(r1Run(func(t float64) float64 { return -3000 + 50*(t-100) }))
	for name, ok := range verdictsOf(cs) {
		if !ok {
			t.Errorf("an order easing toward 0: %q fails", name)
		}
	}
	_, cs = judgeR1(r1Run(func(t float64) float64 { return -3000 - 100*(t-100) }))
	if v := verdictsOf(cs); v["the battery order moves only toward 0 while the meter is silent"] {
		t.Error("an order winding from -3 kW toward -7 kW passes")
	}
	_, cs = judgeR1(r1Run(func(float64) float64 { return 1000 }))
	if v := verdictsOf(cs); v["the battery order moves only toward 0 while the meter is silent"] {
		t.Error("an order flipping from discharge to charge passes")
	}
	late := r1Run(func(float64) float64 { return -3000 })
	late.frames[1] = profileAt(170, 0)
	_, cs = judgeR1(late)
	if v := verdictsOf(cs); v["a 0 W charger order within 64 s of the silence"] {
		t.Error("a standdown 70 s into the silence passes")
	}
}

func TestJudgeR2(t *testing.T) {
	marks := map[string]float64{"charge_now": 0, "offline": 100, "online": 280, "end": 400}
	run := func(frames []frame) map[string]bool {
		d := synth(400, marks, map[string]any{"fuse": 25.0, "charger_delay_s": 0.0}, func(i int, r *sample) {
			r.BatW = -3000
			q := "measured"
			if i >= 100 && i < 280 {
				q = "estimated"
			}
			w := 5000.0
			r.Status = &ftwStatus{LoadQuality: &q, LoadW: &w}
		})
		d.frames = frames
		_, cs := judgeR2(d)
		return verdictsOf(cs)
	}
	if v := run([]frame{profileAt(50, 8), profileAt(120, 6)}); !v["no EV offer rises while the battery is offline"] ||
		!v["Home use shown as estimated throughout the outage"] || !v["the battery discharged 2 kW or more before the drop"] {
		t.Fatalf("a cut while offline: %v", v)
	}
	if v := run([]frame{profileAt(50, 8), profileAt(120, 10)}); v["no EV offer rises while the battery is offline"] {
		t.Fatal("a raise from 8 to 10 A while the battery is offline passes")
	}
}

func TestJudgeR3(t *testing.T) {
	marks := map[string]float64{"plug": 0, "charge_now": 20, "end": 400}
	run := func(set func(i int, r *sample)) map[string]bool {
		_, cs := judgeR3(synth(400, marks, map[string]any{"fuse": 25.0, "charger_delay_s": 60.0, "planner_floor_pct": 10.0}, set))
		return verdictsOf(cs)
	}
	// SoC falls from 12 % to 8 % over the first 100 s.
	soc := func(i int) float64 { return 12 - 4*float64(min(i, 100))/100 }
	v := run(func(i int, r *sample) { r.Bat.SoCPct = soc(i); order(r, -3000) })
	if v["no discharge ordered below the planner floor"] || !v["the battery reached the planner floor"] {
		t.Fatalf("discharge down to 8 %%: %v", v)
	}
	v = run(func(i int, r *sample) {
		r.Bat.SoCPct = soc(i)
		if r.Bat.SoCPct > 10 {
			order(r, -3000)
		}
	})
	if !v["no discharge ordered below the planner floor"] || !v["the battery order swings at most 100 W over the last 5 min"] {
		t.Fatalf("stopping at the floor: %v", v)
	}
	v = run(func(i int, r *sample) { r.Bat.SoCPct = 10; order(r, -3000*float64(i%2)) })
	if v["the battery order swings at most 100 W over the last 5 min"] {
		t.Fatal("an order hunting between 0 and -3 kW passes")
	}
}

func TestJudgeR4SessionEnergy(t *testing.T) {
	marks := map[string]float64{"charge_now": 0, "kill1": 100, "up1": 130, "kill2": 250, "up2": 400, "end": 600}
	run := func(afterWh float64) map[string]bool {
		d := synth(600, marks, map[string]any{"fuse": 25.0, "charger": "CP1"}, func(i int, r *sample) {
			wh := 2000.0
			if (i >= 130 && i < 250) || i >= 400 {
				wh = afterWh
			}
			r.Status = &ftwStatus{Drivers: map[string]ftwDriver{"CP1": {Status: "ok", EVSessionWh: &wh}}}
			r.CarW[0] = 11040
		})
		_, cs := judgeR4(d)
		return verdictsOf(cs)
	}
	if v := run(0); v["session energy counts again after each restart"] {
		t.Fatal("session energy stuck at 0 after the restarts passes")
	}
	if v := run(150); !v["session energy counts again after each restart"] || !v["Charge now back at its minimum within 60 s of each restart, and held"] {
		t.Fatalf("session energy counting after the restarts: %v", v)
	}
}

func TestJudgeR5bAllowsChargingOnlyOnAFreshPlan(t *testing.T) {
	dir := t.TempDir()
	marks := map[string]float64{"ftw_start": 100, "ftw_up": 110, "end": 200}
	// The rig sees the fresh plan, made at 115 s, at 116 s.
	plan := `{"t": 111, "generated_at_ms": 40000}` + "\n" + `{"t": 116, "generated_at_ms": 115000}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "plan.jsonl"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(frames []frame) map[string]bool {
		d := synth(200, marks, map[string]any{"fuse": 25.0}, nil)
		d.dir, d.frames = dir, frames
		_, cs := judgeR5b(d)
		return verdictsOf(cs)
	}
	if v := run([]frame{profileAt(112, 16)}); v["no positive EV order in the first 30 s without a fresh plan"] {
		t.Fatal("16 A before the fresh plan passes")
	}
	if v := run([]frame{profileAt(112, 0), profileAt(120, 16)}); !v["no positive EV order in the first 30 s without a fresh plan"] {
		t.Fatal("0 A until the fresh plan, then 16 A, fails")
	}
}

func TestHoldsFromResetsOnALaterStop(t *testing.T) {
	d := synth(100, nil, nil, func(i int, r *sample) {
		if i < 20 || i >= 50 {
			r.CarW[0] = 11040
		}
	})
	charging := func(r *sample) bool { return carW(r, 0) >= 0.95*minW }
	if p := d.holdsFrom(10, 100, charging); p == nil || *p != 40 {
		t.Fatalf("charging at 10 s, stopped 20-49 s, back from 50 s: %v, want 40", p)
	}
	if p := d.holdsFrom(10, 40, charging); p != nil {
		t.Fatalf("stopped at the window's end: %v, want nil", *p)
	}
}

// The car still draws at the restart, then Core stops it 20 s later for
// 50 s. A judge that takes the first charging sample calls it back at once.
func TestJudgeR4SeesAStopAfterTheRestart(t *testing.T) {
	marks := map[string]float64{"charge_now": 0, "kill1": 100, "up1": 130, "kill2": 250, "up2": 400, "end": 600}
	wh := 150.0
	d := synth(600, marks, map[string]any{"fuse": 25.0, "charger": "CP1"}, func(i int, r *sample) {
		r.Status = &ftwStatus{Drivers: map[string]ftwDriver{"CP1": {Status: "ok", EVSessionWh: &wh}}}
		if i < 150 || i >= 200 {
			r.CarW[0] = 11040
		}
	})
	m, cs := judgeR4(d)
	if verdictsOf(cs)["Charge now back at its minimum within 60 s of each restart, and held"] {
		t.Fatalf("a stop from 20 to 70 s after the restart passes: %v", m["charge_now_back_s"])
	}
}

// r2Run is R2's record: the battery offline from 100 to 280 s, the site at
// 25.0 A (Core's own line, the fuse) before the drop and after the return, and
// overA per phase for the first overS s of the drop, then 20 A.
func r2Run(overA float64, overS int) *runData {
	marks := map[string]float64{"charge_now": 0, "offline": 100, "online": 280, "end": 400}
	d := synth(400, marks, map[string]any{"fuse": 25.0, "charger_delay_s": 0.0}, func(i int, r *sample) {
		r.BatW = -3000
		q, w := "measured", 5000.0
		if i >= 100 && i < 280 {
			q = "estimated"
		}
		r.Status = &ftwStatus{LoadQuality: &q, LoadW: &w}
		switch {
		case i >= 100 && i < 100+overS:
			r.PhaseA = [3]float64{overA, overA, overA}
		case i >= 100 && i < 280:
			r.PhaseA = [3]float64{20, 20, 20}
		default:
			r.PhaseA = [3]float64{25.0, 25.0, 25.0}
		}
	})
	d.frames = []frame{profileAt(50, 8), profileAt(float64(100+overS-2), 0), profileAt(285, 8)}
	return d
}

func TestJudgeR2Overage(t *testing.T) {
	const instant = "over 1.2 x the fuse no longer than the charger's delay + 7 s after the drop"
	const heat = "the 60 s heat stays under 1.13 x the fuse"
	// A short overage, then Core's own line after the return (the review's
	// edge case), and the heat budget spent at 29.95 A for 18 s, as on
	// master.
	for _, c := range []struct {
		a float64
		s int
	}{{28, 4}, {29.95, 18}} {
		_, cs := judgeR2(r2Run(c.a, c.s))
		if v := verdictsOf(cs); !v[instant] || !v[heat] {
			t.Errorf("%.2f A for %d s, then Core's line: %v", c.a, c.s, v)
		}
	}
	_, cs := judgeR2(r2Run(31, 12))
	if verdictsOf(cs)[instant] {
		t.Error("31 A, over 1.2 x 25 A, for 12 s passes")
	}
	_, cs = judgeR2(r2Run(29.95, 180))
	if verdictsOf(cs)[heat] {
		t.Error("a car never cut at 29.95 A for the whole outage passes")
	}
}

// r5aRun is R5a's record: Core starts at 60 s and answers at 70 s; the site
// runs preA per phase with the car at 16 A until 80 s, then afterA(i) with
// the car at 10 A.
func r5aRun(preA float64, afterA func(i int) float64) *runData {
	marks := map[string]float64{"ftw_start": 60, "ftw_up": 70, "end": 250}
	return synth(250, marks, map[string]any{"fuse": 25.0}, func(i int, r *sample) {
		w := 3000.0
		r.Status = &ftwStatus{LoadW: &w}
		if i < 80 {
			r.PhaseA, r.CarW[0] = [3]float64{preA, preA, preA}, 11040
			return
		}
		a := afterA(i)
		r.PhaseA, r.CarW[0] = [3]float64{a, a, a}, 6900
	})
}

// atFuse is Core's own line on a 25 A fuse: the rating itself.
func atFuse(int) float64 { return 25.0 }

func TestJudgeR5a(t *testing.T) {
	_, cs := judgeR5a(r5aRun(30, atFuse))
	for name, ok := range verdictsOf(cs) {
		if !ok {
			t.Errorf("30 A cut to Core's own 25.0 A line 10 s after it answers: %q fails", name)
		}
	}
	_, cs = judgeR5a(r5aRun(22.75, atFuse))
	if verdictsOf(cs)["the site was over the fuse before Core started"] {
		t.Error("a site at 22.75 A before Core starts passes the setup: the run proves nothing")
	}
}

// R5a judges at the fuse: exactly 25.0 A on a 25 A fuse passes, and 0.1 A
// over it for longer than the 3 s budget fails.
func TestJudgeR5aJudgesAtTheFuse(t *testing.T) {
	const under, over = "under the fuse within 30 s of Core answering", "over the fuse at most 3 s once under it"
	judge := func(afterA func(int) float64) map[string]bool {
		_, cs := judgeR5a(r5aRun(30, afterA))
		return verdictsOf(cs)
	}
	if v := judge(atFuse); !v[under] || !v[over] {
		t.Errorf("held at exactly 25.0 A: %v", v)
	}
	// Under the fuse at 80 s, then 25.1 A from 100 to 109 s.
	v := judge(func(i int) float64 {
		if i >= 100 && i < 110 {
			return 25.1
		}
		return 25.0
	})
	if v[over] {
		t.Error("25.1 A for 9 s once under the fuse passes")
	}
	// The 3 s clock starts at the first sample at or under the fuse, not
	// under fuse - 0.5 A: 24.8 A at 80 s, then 26 A from 85 to 94 s, fails.
	v = judge(func(i int) float64 {
		switch {
		case i < 85:
			return 24.8
		case i < 95:
			return 26
		}
		return 24
	})
	if !v[under] || v[over] {
		t.Errorf("a dip to 24.8 A, then 26 A for 10 s: %v", v)
	}
	if v := judge(func(int) float64 { return 25.1 }); v[under] {
		t.Error("held at 25.1 A after Core answers passes")
	}
}

// R3 allows the charger's delay + 7 s (67 s here) over the fuse: exactly
// 25.0 A throughout passes, and 25.1 A for 80 s fails.
func TestJudgeR3JudgesAtTheFuse(t *testing.T) {
	const over = "over the fuse no longer than the charger's delay + 7 s"
	marks := map[string]float64{"plug": 0, "charge_now": 20, "end": 400}
	judge := func(a func(int) float64) bool {
		d := synth(400, marks, map[string]any{"fuse": 25.0, "charger_delay_s": 60.0, "planner_floor_pct": 10.0}, func(i int, r *sample) {
			v := a(i)
			r.PhaseA = [3]float64{v, v, v}
		})
		_, cs := judgeR3(d)
		return verdictsOf(cs)[over]
	}
	if !judge(atFuse) {
		t.Error("held at exactly 25.0 A fails")
	}
	if judge(func(i int) float64 {
		if i >= 100 && i < 180 {
			return 25.1
		}
		return 25.0
	}) {
		t.Error("25.1 A for 80 s passes")
	}
}
