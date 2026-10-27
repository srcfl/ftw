package control

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/sitesim"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// lineRig feeds EVFuseBudget one site reading a second, every configured
// phase at the same current, with one car whose offer the test sets.
type lineRig struct {
	t                   *testing.T
	store               *telemetry.Store
	state               *State
	now                 time.Time
	phases              int
	stepW, fuseMaxW     float64
	offerW              float64
	configs             []loadpoint.Config
	lastRoom            evMeterRoom
	lastGridW, lastCapW float64
}

func newLineRig(t *testing.T, inA float64, phases int, marginA, offerW float64) *lineRig {
	t.Helper()
	r := &lineRig{t: t, store: telemetry.NewStore(), state: NewState(0, 0, "meter"), now: time.Unix(1_800_000_000, 0),
		phases: phases, stepW: evStepA * 230 * float64(phases), fuseMaxW: inA * 230 * float64(phases), offerW: offerW}
	r.state.clock = func() time.Time { return r.now }
	r.state.SiteFuseAmps, r.state.SiteFuseVoltage, r.state.SiteFusePhases, r.state.SiteFuseSafetyA = inA, 230, phases, marginA
	// The charger declares a response time under evSettle, so an offer is
	// shown evSettle after it is sent.
	data, _ := json.Marshal(map[string]any{"power_max_age_s": 1})
	r.store.Update("charger", telemetry.DerEV, 0, nil, data)
	mode := "3p"
	if phases == 1 {
		mode = "1p"
	}
	r.configs = []loadpoint.Config{{ID: "car", DriverName: "charger", PhaseMode: mode,
		MinChargeW: 6 * 230 * float64(phases), MaxChargeW: 32 * 230 * float64(phases)}}
	return r
}

// heat sets the fuse model as if each phase had run at levelA, and holds
// heldA until the next reading.
func (r *lineRig) heat(levelA, heldA float64) {
	r.state.fuseHeat = fuseHeat{set: true, at: r.now}
	for i := range r.phases {
		r.state.fuseHeat.sq[i], r.state.fuseHeat.amps[i] = levelA*levelA, heldA
	}
}

// read delivers one reading a second after the last, with every phase at
// a, and returns the cap.
func (r *lineRig) read(a float64) float64 {
	r.t.Helper()
	r.now = r.now.Add(time.Second)
	data := map[string]any{"control_power_available": true, "control_power_observed_at": r.now.Format(time.RFC3339Nano)}
	for _, key := range []string{"l1_a", "l2_a", "l3_a"}[:r.phases] {
		data[key] = a
	}
	raw, _ := json.Marshal(data)
	r.lastGridW = a * 230 * float64(r.phases)
	r.store.Update("meter", telemetry.DerMeter, r.lastGridW, nil, raw)
	r.store.Get("meter", telemetry.DerMeter).UpdatedAt = r.now
	r.store.RecordDriverSuccess("meter")
	points := []loadpoint.State{{ID: "car", DriverName: "charger", PluggedIn: true, CommandedKnown: true,
		CommandedW: r.offerW, CommandedPhases: r.phases}}
	r.lastCapW, _ = EVFuseBudget(r.store, r.state, points, r.configs, r.fuseMaxW)
	r.lastRoom = r.state.evMeterRoom(r.store.Get("meter", telemetry.DerMeter), r.lastGridW, float64(r.phases), r.fuseMaxW, pendingLoad{})
	return r.lastCapW
}

// The owner's rule of 27 Oct 2026: each phase's working limit is the fuse
// rating less safety_margin_a, with nothing held back below it. Core cuts
// above the line by the heat model and raises only when the next step fits
// under it.
func TestFuseLineProperty(t *testing.T) {
	for _, inA := range []float64{16, 20, 25, 35} {
		for _, phases := range []int{1, 3} {
			for _, marginA := range []float64{0, .5, 1.2} {
				lineA := inA - marginA
				t.Run(fmt.Sprintf("%gA/%dp/margin_%gA", inA, phases, marginA), func(t *testing.T) {
					testFuseLineAtTheLimit(t, inA, phases, marginA, lineA)
					testFuseLineJustOver(t, inA, phases, marginA, lineA)
					testFuseLineExactFit(t, inA, phases, marginA, lineA)
					testFuseLineIdleStart(t, inA, phases, marginA, lineA)
				})
			}
		}
	}
}

// At the line, with its heat at the line, nothing is over and the cap
// holds for ten minutes.
func testFuseLineAtTheLimit(t *testing.T, inA float64, phases int, marginA, lineA float64) {
	t.Helper()
	r := newLineRig(t, inA, phases, marginA, 0)
	r.offerW = 6 * r.stepW
	r.heat(lineA, lineA)
	for i := range 600 {
		if capW := r.read(lineA); capW != r.offerW {
			t.Fatalf("at the line: reading %d cut the cap to %.1f W from %.1f W", i+1, capW, r.offerW)
		}
		if r.lastRoom.fuseOverA != 0 || r.lastRoom.overLimit {
			t.Fatalf("at the line: reading %d over by %.3f A (overLimit %v)", i+1, r.lastRoom.fuseOverA, r.lastRoom.overLimit)
		}
	}
}

// 0.1 A over the line: from a cold fuse the cut waits for the heat budget
// and lands on the first reading after it is spent; from a hot fuse it
// lands on the next reading; above 1.2 × In it lands on the same reading.
// Each cut takes at least the 0.1 A and at most one step.
func testFuseLineJustOver(t *testing.T, inA float64, phases int, marginA, lineA float64) {
	t.Helper()
	overA := lineA + .1
	offerW := 10 * evStepA * 230 * float64(phases)
	checkCut := func(name string, r *lineRig, capW float64) {
		t.Helper()
		if cutW := offerW - capW; cutW < .1*r.stepW-1e-6 || cutW > r.stepW+1e-6 {
			t.Fatalf("%s: cut %.1f W, want 0.1 A to one step (%.0f W)", name, cutW, r.stepW)
		}
	}

	r := newLineRig(t, inA, phases, marginA, offerW)
	coldA := lineA - 2
	r.heat(coldA, overA)
	tStar := -fuseHeatTau.Seconds() * math.Log(1-(lineA*lineA-coldA*coldA)/(overA*overA-coldA*coldA))
	for k := 1; ; k++ {
		capW := r.read(overA)
		if float64(k) < tStar {
			if capW != offerW {
				t.Fatalf("cold: cut to %.1f W at %d s, before the budget is spent at %.1f s", capW, k, tStar)
			}
			continue
		}
		if capW >= offerW {
			t.Fatalf("cold: no cut at %d s, the first reading after the budget (%.1f s)", k, tStar)
		}
		checkCut("cold", r, capW)
		break
	}

	r = newLineRig(t, inA, phases, marginA, offerW)
	r.heat(lineA, overA)
	if capW := r.read(overA); capW >= offerW {
		t.Fatal("hot: no cut on the next reading")
	} else {
		checkCut("hot", r, capW)
	}

	r = newLineRig(t, inA, phases, marginA, offerW)
	r.heat(coldA, coldA)
	if capW := r.read(fuseInstantFactor*inA + .1); capW >= offerW {
		t.Fatalf("above %.1f × In: no cut on the same reading", fuseInstantFactor)
	}
}

// A rise moves one step when level + step is exactly the line, and not
// when it is 0.01 A over. A demand less than a step rises when the room
// equals it, with or without battery charge.
func testFuseLineExactFit(t *testing.T, inA float64, phases int, marginA, lineA float64) {
	t.Helper()
	settled := func(houseA float64) *lineRig {
		t.Helper()
		r := newLineRig(t, inA, phases, marginA, 0)
		r.offerW = 6 * r.stepW
		r.heat(lineA, lineA)
		for range 30 { // The offer shows; no room.
			r.read(lineA)
		}
		r.heat(houseA, houseA) // The house fell, and its heat has cooled.
		return r
	}
	r := settled(lineA - evStepA)
	if capW := r.read(lineA - evStepA); math.Abs(capW-(r.offerW+r.stepW)) > 1e-6 {
		t.Fatalf("level + step = line: cap %.1f W, want one step over the %.0f W offer", capW, r.offerW)
	}
	r = settled(lineA - evStepA + .01)
	if capW := r.read(lineA - evStepA + .01); capW != r.offerW {
		t.Fatalf("level + step = line + 0.01 A: cap %.1f W, want the %.0f W offer", capW, r.offerW)
	}
	for _, charging := range []bool{false, true} {
		r = settled(lineA - evStepA/2)
		wantW := r.offerW + r.stepW/2
		r.state.SetEVFuseDemands([]loadpoint.FuseDemand{{DriverName: "charger", PowerW: wantW, PhaseMode: r.configs[0].PhaseMode}})
		if charging {
			r.state.batteryCommands = map[string]commandLog{"battery": commandLog{}.add(r.now.Add(-time.Minute), 2000)}
		}
		if capW := r.read(lineA - evStepA/2); math.Abs(capW-wantW) > 1e-6 {
			t.Fatalf("half a step to go, room for it, battery charging %v: cap %.1f W, want %.1f W", charging, capW, wantW)
		}
	}
}

// An idle car with room for exactly its minimum starts at the minimum
// once the meter has shown that room for evRestartDwell.
func testFuseLineIdleStart(t *testing.T, inA float64, phases int, marginA, lineA float64) {
	t.Helper()
	r := newLineRig(t, inA, phases, marginA, 0)
	minW := r.configs[0].MinChargeW
	r.heat(lineA, lineA)
	if capW := r.read(lineA); capW != 0 {
		t.Fatalf("idle start with no room: cap %.1f W", capW)
	}
	levelA := lineA - minW/(230*float64(phases))
	for k := 1; k <= 60; k++ {
		capW := r.read(levelA)
		switch at := time.Duration(k-1) * time.Second; {
		case at < evRestartDwell && capW != 0:
			t.Fatalf("idle start: %.1f W after %v of room, before the %v dwell", capW, at, evRestartDwell)
		case at >= evRestartDwell:
			if capW != minW {
				t.Fatalf("idle start: cap %.1f W after %v of room for exactly %.0f W", capW, at, minW)
			}
			return
		}
	}
}

// fitScenario runs a 3 × 16 A site, safety margin 0.5 A, with 3.5 A of
// house on each phase: twelve steps of the car fit exactly under the 15.5 A
// line. The car asks for 16 A.
func fitScenario(noise func() float64) *simSite {
	sc := simScenario{name: "exact fit", run: 30 * time.Minute,
		plant: func(p *sitesim.Plant) {
			p.Fuse.RatingA = 16
			p.HouseW = houseA(3.5, 3.5, 3.5)
		},
		setup: func(s *simSite) { s.meterNoise = noise }}
	return runSimScenario(sc, simStart)
}

// Closed loop: the car climbs to the line and stays there for 30 min.
func TestSiteSimCarEndsAtTheLine(t *testing.T) {
	s := fitScenario(nil)
	if s.failure != "" {
		t.Fatal(s.failure)
	}
	lineA := s.state.fuseLineA()
	if want := (lineA - 3.5) * 690; s.p.EVSE.DrawW != want || s.peakA > lineA+1e-6 {
		t.Fatalf("car %.0f W, peak phase %.3f A; want %.0f W with every phase at most %.1f A", s.p.EVSE.DrawW, s.peakA, want, lineA)
	}
	var last float64
	for _, c := range s.commands {
		if c.device == "evse" {
			if c.w < last {
				t.Fatalf("cut to %v with every phase at or under the line", c)
			}
			last = c.w
		}
	}
}

// Closed loop with 0.1 A of meter noise on each phase: the car steps at
// most once per settle time plus the heat's cooling, no phase passes
// 1.2 × In, and the plant's gG fuse holds.
func TestSiteSimNoisyLineDoesNotHunt(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 0))
	s := fitScenario(func() float64 { return .1 * rng.NormFloat64() })
	if s.failure != "" {
		t.Fatal(s.failure)
	}
	if s.peakA > fuseInstantFactor*s.p.Fuse.RatingA || s.p.Fuse.AnyBlown() {
		t.Fatalf("peak phase %.2f A, fuse %+v", s.peakA, s.p.Fuse)
	}
	var moves []simCommand
	last := 0.0
	for _, c := range s.commands {
		if c.device == "evse" && c.w != last {
			moves, last = append(moves, c), c.w
		}
	}
	// The start, then the climb to the line: one step per reading.
	climb := 0
	for climb+1 < len(moves) && moves[climb+1].w > moves[climb].w {
		climb++
	}
	after := moves[climb+1:]
	perHour := float64(len(after)) / s.p.Now.Sub(moves[climb].at).Hours()
	if limit := time.Hour.Seconds() / (evSettle + fuseHeatTau).Seconds(); perHour > limit {
		t.Fatalf("%d moves after the climb (%.0f an hour, limit %.0f): %v", len(after), perHour, limit, after)
	}
	for i := 1; i < len(after); i++ {
		if after[i].w > after[i-1].w && after[i].at.Sub(after[i-1].at) < evSettle {
			t.Fatalf("rose %v after a cut: %v then %v", after[i].at.Sub(after[i-1].at), after[i-1], after[i])
		}
	}
	t.Logf("car %.0f W, peak phase %.2f A, %d moves after the climb", s.p.EVSE.DrawW, s.peakA, len(after))
}

// With no safety margin, battery charge may fill a phase to exactly the
// rating beside a car that wants nothing more.
func TestEVBatteryChargeFillsPhaseToTheRating(t *testing.T) {
	store, state := evSplitFixture(t, (20+18+18)*230, []float64{20, 18, 18}, 4140, "3p")
	state.SiteFuseSafetyA = 0
	sent(state, "ev", 4140)
	sent(state, "first", 0)
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: 8000}}, store, state, 17250)
	if got, want := targetW(out, "first"), (25.0-20)*690; math.Abs(got-want) > .01 {
		t.Fatalf("battery %.1f W, want %.1f W: L1 from 20 A to exactly 25 A", got, want)
	}
}
