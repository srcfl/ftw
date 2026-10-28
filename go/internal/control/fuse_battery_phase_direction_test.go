package control

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The battery fuse paths read each phase with its direction and count the
// batteries' own move. 16 A fuse, 230 V, 3 phases; the working limit is
// the fuse less safety_margin_a (fuseLineA).

func phaseDirectionStore(t *testing.T, gridW, batW float64, data map[string]any) *telemetry.Store {
	t.Helper()
	s := telemetry.NewStore()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	s.Update("meter", telemetry.DerMeter, gridW, nil, raw)
	s.DriverHealthMut("meter").RecordSuccess()
	soc := 0.6
	s.Update("bat", telemetry.DerBattery, batW, &soc, nil)
	s.DriverHealthMut("bat").RecordSuccess()
	return s
}

func phaseDirectionState(safetyA float64) *State {
	st := NewState(0, 50, "meter")
	st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases, st.SiteFuseSafetyA = 16, 230, 3, safetyA
	st.DriverLimits = map[string]PowerLimits{"bat": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	return st
}

// L1 exports 20 A, L2/L3 import 12 A, the total imports 920 W. No phase
// imports over the limit, and a discharge adds export on L1.
var exportingL1 = map[string]any{"l1_a": 20.0, "l2_a": 12.0, "l3_a": 12.0,
	"l1_w": -4600.0, "l2_w": 2760.0, "l3_w": 2760.0}

func TestFuseSaverDoesNotDischargeIntoExportingPhase(t *testing.T) {
	store := phaseDirectionStore(t, 920, 0, exportingL1)
	out := fuseSaverFromLivePower(store, phaseDirectionState(0.5), map[string]float64{"bat": 15200}, 16*230*3)
	for _, o := range out {
		if o.TargetW < 0 {
			t.Fatalf("fuse saver ordered %.0f W discharge; L1 export 20.0 A -> %.1f A on a 16 A fuse", o.TargetW, 20+(-o.TargetW)/690)
		}
	}
}

func TestIdleDispatchDoesNotDischargeIntoExportingPhase(t *testing.T) {
	store := phaseDirectionStore(t, 920, 0, exportingL1)
	st := phaseDirectionState(0.5)
	st.Mode = ModeIdle
	out := ComputeDispatch(store, st, map[string]float64{"bat": 15200}, 16*230*3)
	for _, o := range out {
		if o.TargetW < 0 {
			t.Fatalf("Idle ComputeDispatch ordered %.0f W discharge; L1 export 20.0 A -> %.1f A", o.TargetW, 20+(-o.TargetW)/690)
		}
	}
}

// L1 imports 22 A, L2/L3 export 15 A, the battery discharges 3 kW and the
// total exports. Cutting the discharge raises L1's import.
func TestFuseGuardKeepsDischargeThatRelievesImportingPhase(t *testing.T) {
	store := phaseDirectionStore(t, (22-30)*230, -3000, map[string]any{
		"l1_a": 22.0, "l2_a": 15.0, "l3_a": 15.0,
		"l1_w": 5060.0, "l2_w": -3450.0, "l3_w": -3450.0})
	st := phaseDirectionState(0.5)
	out := applyFuseGuard([]DispatchTarget{{Driver: "bat", TargetW: -3000}}, store, st, 16*230*3)
	if out[0].TargetW > -3000+1 {
		t.Fatalf("fuse guard cut discharge -3000 -> %.0f W; L1 import 22.0 -> %.1f A; hold max discharge %.0f W until +%v",
			out[0].TargetW, 22+(out[0].TargetW+3000)/690, st.FuseHoldMaxDischargeW, st.FuseHoldUntil.Sub(st.now()))
	}
}

// L1 exports 18 A (one-phase PV), L2/L3 import 14 A, the total imports
// 2300 W. Charging relieves L1 and loads L2/L3.
var exportingL1Charge = map[string]any{"l1_a": 18.0, "l2_a": 14.0, "l3_a": 14.0,
	"l1_w": -4140.0, "l2_w": 3220.0, "l3_w": 3220.0}

func TestFuseSaverIgnoresExportingPhaseUnderImportTotal(t *testing.T) {
	store := phaseDirectionStore(t, 2300, 0, exportingL1Charge)
	if out := fuseSaverFromLivePower(store, phaseDirectionState(0), map[string]float64{"bat": 15200}, 11040); out != nil {
		t.Errorf("fuseSaverFromLivePower = %.0f W; discharging pushes exporting L1 further past the fuse", out[0].TargetW)
	}
}

// No charge fits both sides: 3000 W takes L2/L3 to 18.3 A, none leaves L1
// at 18 A of export. The guard leaves the charge where the worst phase is
// least over: 1380 W, with every phase at 16 A.
func TestFuseGuardChargeBalancesExportingAndImportingPhases(t *testing.T) {
	store := phaseDirectionStore(t, 2300, 0, exportingL1Charge)
	got := applyFuseGuard([]DispatchTarget{{Driver: "bat", TargetW: 3000}}, store, phaseDirectionState(0), 11040)
	l1, l2 := -18+got[0].TargetW/690, 14+got[0].TargetW/690
	if worst := math.Max(math.Abs(l1), math.Abs(l2)); worst > 16.01 {
		t.Errorf("applyFuseGuard charge 3000 -> %.0f W leaves L1 %.2f A, L2/L3 %.2f A; want every phase at or below 16 A", got[0].TargetW, l1, l2)
	}
}

// The relief counts the battery's own share of the phase it reads. A
// battery that follows its order keeps L1 at the working limit, 15.5 A on
// a 0.5 A margin, on every tick instead of returning to 0 W when the
// overage stops showing.
func TestPerPhaseFuseReliefHoldsWhileOverloadPersists(t *testing.T) {
	st := phaseDirectionState(0.5)
	st.Mode = ModeIdle
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st.clock = func() time.Time { return now }

	store := telemetry.NewStore()
	house := [3]float64{20, 5, 5} // A per phase, import
	batW := 0.0                   // battery follows its last order within a tick
	caps := map[string]float64{"bat": 15200}
	var targets, l1 []float64
	for tick := 0; tick < 10; tick++ {
		a := house
		for i := range a {
			a[i] += batW / (230 * 3) // a three-phase battery spreads evenly
		}
		raw, _ := json.Marshal(map[string]any{"l1_a": a[0], "l2_a": a[1], "l3_a": a[2]})
		store.Update("meter", telemetry.DerMeter, (a[0]+a[1]+a[2])*230, nil, raw)
		store.DriverHealthMut("meter").RecordSuccess()
		soc := 0.6
		store.Update("bat", telemetry.DerBattery, batW, &soc, nil)
		store.DriverHealthMut("bat").RecordSuccess()

		out := ComputeDispatch(store, st, caps, 16*230*3)
		w := 0.0
		for _, o := range out {
			if o.Driver == "bat" {
				w = o.TargetW
			}
		}
		targets = append(targets, math.Round(w))
		l1 = append(l1, math.Round(a[0]*10)/10)
		batW = w
		now = now.Add(5 * time.Second)
	}
	line := st.fuseLineA()
	for i := 1; i < len(l1); i++ {
		if l1[i] > line || l1[i] < line-0.05 {
			t.Fatalf("tick %d: L1 %.1f A, want the %.1f A working limit while the overload persists; targets %v, L1 %v", i, l1[i], line, targets, l1)
		}
	}
}

// chargeLoop repeats a 4000 W charge request against house loads of
// 13/7/5 A on a 0 A margin, with a three-phase battery that follows each
// order. dispatch returns the battery's next target.
func chargeLoop(t *testing.T, ticks int, dispatch func(*telemetry.Store, *State) float64) (targets, l1 []float64) {
	t.Helper()
	st := phaseDirectionState(0)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st.clock = func() time.Time { return now }
	store := telemetry.NewStore()
	batW := 0.0
	for tick := 0; tick < ticks; tick++ {
		a := [3]float64{13, 7, 5}
		for i := range a {
			a[i] += batW / 690
		}
		raw, _ := json.Marshal(map[string]any{"l1_a": a[0], "l2_a": a[1], "l3_a": a[2],
			"l1_w": a[0] * 230, "l2_w": a[1] * 230, "l3_w": a[2] * 230})
		store.Update("meter", telemetry.DerMeter, (a[0]+a[1]+a[2])*230, nil, raw)
		store.DriverHealthMut("meter").RecordSuccess()
		soc := 0.5
		store.Update("bat", telemetry.DerBattery, batW, &soc, nil)
		store.DriverHealthMut("bat").RecordSuccess()
		batW = dispatch(store, st)
		targets = append(targets, math.Round(batW))
		l1 = append(l1, math.Round(a[0]*100)/100)
		now = now.Add(5 * time.Second)
	}
	return targets, l1
}

// The fuse hold caps the request before the guard reads the phases, so the
// hold's cut is not taken off again. On a 0 A margin the charge holds
// 2070 W with L1 at the 16 A rating on every repeat, instead of switching
// between 2070 W and 0 W.
func TestFuseGuardHeldChargeHoldsSteady(t *testing.T) {
	assertSteady := func(name string, targets, l1 []float64) {
		t.Helper()
		for i := range targets {
			if math.Abs(targets[i]-2070) > 1 || (i > 0 && math.Abs(l1[i]-16) > 0.01) {
				t.Fatalf("%s: tick %d charge %v W, L1 %v A; want 2070 W and 16 A; targets %v, L1 %v", name, i, targets[i], l1[i], targets, l1)
			}
		}
	}
	targets, l1 := chargeLoop(t, 10, func(store *telemetry.Store, st *State) float64 {
		return applyFuseGuard([]DispatchTarget{{Driver: "bat", TargetW: 4000}}, store, st, 16*230*3)[0].TargetW
	})
	assertSteady("applyFuseGuard", targets, l1)

	targets, l1 = chargeLoop(t, 12, func(store *telemetry.Store, st *State) float64 {
		st.Mode = ModeCharge
		st.DriverLimits = map[string]PowerLimits{"bat": {MaxChargeW: 4000, MaxDischargeW: 4000}}
		for _, o := range ComputeDispatch(store, st, map[string]float64{"bat": 15200}, 16*230*3) {
			if o.Driver == "bat" {
				return o.TargetW
			}
		}
		return 0
	})
	assertSteady("Charge mode", targets, l1)
}

// A watchdog-offline meter's cached phases are not the site: they drive no
// phase relief.
func TestPhaseReliefSkipsOfflineMeter(t *testing.T) {
	store := phaseDirectionStore(t, 20*230, 0, map[string]any{"l1_a": 20.0, "l2_a": 0.0, "l3_a": 0.0,
		"l1_w": 4600.0, "l2_w": 0.0, "l3_w": 0.0})
	meter := store.Get("meter", telemetry.DerMeter)
	if imp, _ := phaseFuseReliefW(livePhaseSample(store, "meter", meter), phaseDirectionState(0), 0); imp <= 0 {
		t.Fatalf("online meter: import relief %.0f W, want > 0 for L1 at 20 A", imp)
	}
	store.DriverHealthMut("meter").SetOffline()
	if imp, exp := phaseFuseReliefW(livePhaseSample(store, "meter", meter), phaseDirectionState(0), 0); imp != 0 || exp != 0 {
		t.Fatalf("offline meter: relief %.0f/%.0f W, want none", imp, exp)
	}
}

// The batteries' move counts only power read live from the store, in the
// same reading the rest of the decision used. A battery that went offline,
// moved on to a newer reading or has no reading leaves its power in the
// phases unknown: no phase relief, as #1876's stop relief did. Before,
// the cached reading's RawW drove the relief.
func TestPhaseReliefNeedsTheBatteryReadingItUses(t *testing.T) {
	data := map[string]any{"l1_a": 15.0, "l2_a": 6.0, "l3_a": 5.1, "l1_w": 3450.0, "l2_w": 1380.0, "l3_w": 1173.0}
	st := phaseDirectionState(0)
	stop := []DispatchTarget{{Driver: "bat", TargetW: 0}}
	seed := func() (*telemetry.Store, map[string]*telemetry.DerReading, phaseSample) {
		store := phaseDirectionStore(t, 6003, -3000, data)
		meter := store.Get("meter", telemetry.DerMeter)
		return store, map[string]*telemetry.DerReading{"bat": store.Get("bat", telemetry.DerBattery)}, livePhaseSample(store, "meter", meter)
	}

	_, readings, phases := seed()
	// Stopping 3000 W puts L1 at 15 + 4.35 A; 3.35 A over 16 A is 2310 W.
	if imp, _ := phaseReliefForTargets(phases, st, stop, readings); math.Abs(imp-2310) > 1 {
		t.Fatalf("live readings: relief %.0f W, want 2310 W", imp)
	}

	store, readings, phases := seed()
	store.DriverHealthMut("bat").SetOffline()
	if imp, exp := phaseReliefForTargets(phases, st, stop, readings); imp != 0 || exp != 0 {
		t.Errorf("offline battery: relief %.0f/%.0f W, want none", imp, exp)
	}

	store, readings, phases = seed()
	time.Sleep(time.Millisecond)
	soc := 0.6
	store.Update("bat", telemetry.DerBattery, -3000, &soc, nil)
	if imp, exp := phaseReliefForTargets(phases, st, stop, readings); imp != 0 || exp != 0 {
		t.Errorf("newer battery reading: relief %.0f/%.0f W, want none", imp, exp)
	}

	// Counted from 0 W, a 2000 W charge would read as L1 at 17.9 A.
	_, _, phases = seed()
	charge := []DispatchTarget{{Driver: "bat", TargetW: 2000}}
	if imp, exp := phaseReliefForTargets(phases, st, charge, map[string]*telemetry.DerReading{}); imp != 0 || exp != 0 {
		t.Errorf("no battery reading: relief %.0f/%.0f W, want none", imp, exp)
	}
}

// The battery's working limit on each phase is L = In − safety_margin_a,
// in both directions, with no floor under the margin. A phase at L needs no
// relief, 0.1 A past it needs exactly 0.1 A, and a target that lands a
// phase exactly at L passes the guard unchanged.
func TestBatteryFuseLineProperty(t *testing.T) {
	const v = 230.0
	for _, in := range []float64{16, 20, 25, 35} {
		for _, phases := range []int{1, 3} {
			for _, margin := range []float64{0, 0.5, 1.2} {
				for _, dir := range []float64{1, -1} { // 1 import, -1 export
					name := fmt.Sprintf("%gA/%dp/margin%g/dir%+g", in, phases, margin, dir)
					st := NewState(0, 50, "meter")
					st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases, st.SiteFuseSafetyA = in, v, phases, margin
					st.DriverLimits = map[string]PowerLimits{"bat": {MaxChargeW: 50000, MaxDischargeW: 50000}}
					line := in - margin
					if got := st.fuseLineA(); got != line {
						t.Fatalf("%s: fuseLineA %.2f A, want %.2f A", name, got, line)
					}
					perA := v * float64(phases)
					fuseMaxW := in * perA
					// store reads the worst phase at worstA and 2 A on the
					// others, all flowing in dir, with phase power.
					store := func(worstA float64) *telemetry.Store {
						amps := []float64{worstA, 2, 2}[:phases]
						data := map[string]any{}
						var totalW float64
						for i, a := range amps {
							w := dir * a * v
							data[fmt.Sprintf("l%d_a", i+1)] = a
							data[fmt.Sprintf("l%d_w", i+1)] = w
							totalW += w
						}
						return phaseDirectionStore(t, totalW, 0, data)
					}
					relief := func(worstA float64) (float64, float64) {
						s := store(worstA)
						meter := s.Get("meter", telemetry.DerMeter)
						return phaseFuseReliefW(livePhaseSample(s, "meter", meter), st, 0)
					}

					if imp, exp := relief(line); imp != 0 || exp != 0 {
						t.Errorf("%s: phase at the %.1f A line: relief %.1f/%.1f W, want none", name, line, imp, exp)
					}

					imp, exp := relief(line + 0.1)
					want := 0.1 * perA
					gotW, otherW := imp, exp
					if dir < 0 {
						gotW, otherW = exp, imp
					}
					if math.Abs(gotW-want) > 1 || otherW != 0 {
						t.Errorf("%s: 0.1 A past the line: relief %.1f/%.1f W, want %.1f W on the flowing side", name, imp, exp, want)
					}

					// The battery moves the phase from line − 2 A to the line.
					targetW := dir * 2 * perA
					out := applyFuseGuard([]DispatchTarget{{Driver: "bat", TargetW: targetW}}, store(line-2), st, fuseMaxW)
					if out[0].Clamped || math.Abs(out[0].TargetW-targetW) > 1e-6 || !st.FuseHoldUntil.IsZero() {
						t.Errorf("%s: target %.0f W that lands at the %.1f A line: guard gave %+v, hold until %v", name, targetW, line, out[0], st.FuseHoldUntil)
					}
				}
			}
		}
	}
}

// C3, rig seed 207533, on a 16 A fuse with no margin: the battery holds a
// 3 kW discharge with L1 at 14.4 A, then PV drops 1280 W and L1 reads
// 16.3 A. The relief brings L1 to the line. On the next ticks the reading
// sits at the line with the battery at its relief power, and the relief
// stays: L1 crosses the rating once, on the PV step, and never again while
// PV is low. Each tick the battery reaches the order it was sent the tick
// before, and the meter reads it there.
func TestFuseReliefStaysAtTheLineAfterPVDrop(t *testing.T) {
	targets, l1, line := pvDropLoop(0, 0)
	assertHeldAtTheLine(t, targets, l1, line)
}

// pvDropLoop runs the 207533 shape: a -3005 W hold, L1 at 14.45 A, and PV
// down 1280 W from tick 5 to tick 29, with 5 s ticks. The meter sees the
// battery's actual power. The battery's own reading reports the power of
// readLag ticks earlier. maxDischargeW > 0 caps what the battery delivers
// without Core knowing it.
func pvDropLoop(readLag int, maxDischargeW float64) (targets, l1 []float64, line float64) {
	r := pvDropRun(pvDropCase{readLag: readLag, maxDischargeW: maxDischargeW})
	return r.targets, r.l1, r.line
}

// pvDropCase varies pvDropLoop. freezeFrom > 0 makes the meter repeat its
// reading from that tick on. onePhase puts the battery on that phase only
// (1-3), which Core reads as balanced. load replaces the house and car
// current per phase. ticks > 0 replaces the 40 ticks, pvW the PV step, and
// extra adds import current per phase at a tick.
type pvDropCase struct {
	readLag       int
	maxDischargeW float64
	freezeFrom    int
	onePhase      int
	load          [3]float64
	ticks         int
	pvW           func(tick int) float64
	extra         func(tick int) [3]float64
}

type pvDropResult struct {
	targets, l1 []float64
	// batPhase is the true current on the battery's phase when onePhase > 0.
	batPhase []float64
	line     float64
}

func pvDropRun(c pvDropCase) (r pvDropResult) {
	st := phaseDirectionState(0)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st.clock = func() time.Time { return now }
	st.SetBatteryManualHold(BatteryManualHold{PowerW: -3005, ExpiresAt: now.Add(time.Hour)})

	store := telemetry.NewStore()
	load := [3]float64{23.44, 11.04, 11.54} // house and car, A import per phase
	if c.load != ([3]float64{}) {
		load = c.load
	}
	pvW := -3200.0
	actual := []float64{-3005}
	r.line = st.fuseLineA()
	var frozen json.RawMessage
	var frozenW float64
	ticks := 40
	if c.ticks > 0 {
		ticks = c.ticks
	}
	for tick := 0; tick < ticks; tick++ {
		switch tick {
		case 5:
			pvW = -1920
		case 30:
			pvW = -3200
		}
		if c.pvW != nil {
			pvW = c.pvW(tick)
		}
		var extra [3]float64
		if c.extra != nil {
			extra = c.extra(tick)
		}
		batW := actual[len(actual)-1]
		var a [3]float64
		data := map[string]any{}
		var totalW float64
		for i := range a {
			a[i] = load[i] + extra[i] + (pvW+batW)/690
			if c.onePhase > 0 {
				a[i] = load[i] + extra[i] + pvW/690
				if i == c.onePhase-1 {
					a[i] += batW / 230
				}
			}
			data[fmt.Sprintf("l%d_a", i+1)] = math.Abs(a[i])
			data[fmt.Sprintf("l%d_w", i+1)] = a[i] * 230
			totalW += a[i] * 230
		}
		raw, _ := json.Marshal(data)
		if c.freezeFrom > 0 && tick >= c.freezeFrom {
			if frozen == nil {
				frozen, frozenW = raw, totalW
			}
			raw, totalW = frozen, frozenW
		}
		store.Update("meter", telemetry.DerMeter, totalW, nil, raw)
		store.DriverHealthMut("meter").RecordSuccess()
		soc := 0.6
		store.Update("bat", telemetry.DerBattery, actual[max(0, len(actual)-1-c.readLag)], &soc, nil)
		store.DriverHealthMut("bat").RecordSuccess()

		order := batW
		for _, o := range ComputeDispatch(store, st, map[string]float64{"bat": 15200}, 16*230*3) {
			if o.Driver == "bat" {
				order = o.TargetW
			}
		}
		batW = order
		if c.maxDischargeW > 0 {
			batW = math.Max(order, -c.maxDischargeW)
		}
		actual = append(actual, batW)
		r.targets = append(r.targets, math.Round(order))
		r.l1 = append(r.l1, math.Round(a[0]*100)/100)
		if c.onePhase > 0 {
			r.batPhase = append(r.batPhase, math.Round(a[c.onePhase-1]*100)/100)
		}
		now = now.Add(5 * time.Second)
	}
	return r
}

// assertHeldAtTheLine wants L1 at the line while PV is low, and one crossing
// of it, on the PV step.
func assertHeldAtTheLine(t *testing.T, targets, l1 []float64, line float64) {
	t.Helper()
	crossings := 0
	for i := range l1 {
		if l1[i] > line+0.01 && (i == 0 || l1[i-1] <= line+0.01) {
			crossings++
		}
		if i > 5 && i < 30 && (l1[i] > line+0.01 || l1[i] < line-0.05) {
			t.Errorf("tick %d: L1 %.2f A, want the %.1f A line while PV is low", i, l1[i], line)
		}
	}
	if crossings != 1 {
		t.Errorf("L1 crossed the %.1f A line %d times, want once (the PV step)", line, crossings)
	}
	if t.Failed() {
		t.Logf("targets %v", targets)
		t.Logf("L1 %v", l1)
	}
}

// The battery's reading lags the meter by one or two ticks: the meter
// already shows the battery at its relief while the reading still holds
// the power before it. Counted from that reading, the relief looked like a
// move still to make, so every other tick took it back, and the order ran
// out further each time (-3214, -3005, -3422, -3005, ... -5717 W) with L1
// back at 16.3 A on every other tick. The last command bounds the move, so
// the relief holds -3214 W with L1 at the line.
func TestFuseReliefHoldsWhenTheBatteryReadingLags(t *testing.T) {
	for _, lag := range []int{1, 2} {
		t.Run(fmt.Sprintf("lag %d", lag), func(t *testing.T) {
			targets, l1, line := pvDropLoop(lag, 0)
			assertHeldAtTheLine(t, targets, l1, line)
			for i := 6; i < 30; i++ {
				if targets[i] != targets[5] {
					t.Errorf("tick %d: order %.0f W, want the %.0f W relief held", i, targets[i], targets[5])
				}
			}
		})
	}
}

// A battery that cannot reach its order, with no limit Core knows, must
// not wind the relief up through the last-command bound: the order settles,
// and it returns to the hold once PV comes back.
func TestFuseReliefSettlesWhenTheBatteryFallsShort(t *testing.T) {
	targets, l1, _ := pvDropLoop(0, 3100)
	for i := 8; i < 30; i++ {
		if targets[i] != targets[i-1] || targets[i] < -3300 {
			t.Errorf("tick %d: order %.0f W after %.0f W, want it settled near -3214 W", i, targets[i], targets[i-1])
		}
	}
	if targets[31] != -3005 {
		t.Errorf("tick 31: order %.0f W, want the -3005 W hold back once PV returns", targets[31])
	}
	if t.Failed() {
		t.Logf("targets %v", targets)
		t.Logf("L1 %v", l1)
	}
}

// Mixed directions, three-phase battery holding -2000 W: L1 imports at the
// 16 A line, and a one-phase PV inverter on L3 steps from -15 A to -18 A
// of export. No move fits both phases. The least-over move cuts the
// discharge by 690 W and leaves L1 at 17 A and L3 at -17 A. Before, the
// last-command bound read L1 at the import line as a reason to keep
// -2000 W, put back every cut the export side made, and left L3 at -18 A.
func TestFuseReliefLeavesMixedPhasesLeastOver(t *testing.T) {
	st := phaseDirectionState(0)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st.clock = func() time.Time { return now }
	st.SetBatteryManualHold(BatteryManualHold{PowerW: -2000, ExpiresAt: now.Add(time.Hour)})
	store := telemetry.NewStore()
	base := [3]float64{18.9, 8, -12.1} // A without the battery, + import
	batW := -2000.0
	var orders, worst []float64
	for tick := 0; tick < 20; tick++ {
		if tick == 3 {
			base[2] = -15.1
		}
		data := map[string]any{}
		var totalW, w float64
		for i := range base {
			a := base[i] + batW/690
			data[fmt.Sprintf("l%d_a", i+1)] = math.Abs(a)
			data[fmt.Sprintf("l%d_w", i+1)] = a * 230
			totalW += a * 230
			w = math.Max(w, math.Abs(a))
		}
		raw, _ := json.Marshal(data)
		store.Update("meter", telemetry.DerMeter, totalW, nil, raw)
		store.DriverHealthMut("meter").RecordSuccess()
		soc := 0.6
		store.Update("bat", telemetry.DerBattery, batW, &soc, nil)
		store.DriverHealthMut("bat").RecordSuccess()
		for _, o := range ComputeDispatch(store, st, map[string]float64{"bat": 15200}, 16*230*3) {
			if o.Driver == "bat" {
				batW = o.TargetW
			}
		}
		orders = append(orders, math.Round(batW))
		worst = append(worst, math.Round(w*100)/100)
		now = now.Add(5 * time.Second)
	}
	for i := 8; i < 20; i++ {
		if worst[i] > 17.05 {
			t.Errorf("tick %d: worst phase %.2f A, want at most 17 A (L1 17, L3 -17)", i, worst[i])
		}
	}
	if t.Failed() {
		t.Logf("orders %v", orders)
		t.Logf("worst phase |A| %v", worst)
	}
}

// A one-phase battery on L2, which Core reads as balanced, with L1 at 18 A
// while PV is low. Once PV returns, L1 still sits at 16.14 A, which the
// battery cannot reach. Before, the last-command bound held the -4882 W
// discharge for L1 and left L2 exporting 19.86 A, past 1.2 x In, on every
// tick. Now L2 comes back inside the line and the order holds.
func TestFuseReliefLetsAOnePhaseBatteryOffItsOwnPhase(t *testing.T) {
	r := pvDropRun(pvDropCase{onePhase: 2, load: [3]float64{20.78, 6, 11.54}})
	for i := 31; i < 40; i++ {
		if math.Abs(r.batPhase[i]) > r.line+0.01 {
			t.Errorf("tick %d: L2 %.2f A, want inside the %.1f A line", i, r.batPhase[i], r.line)
		}
		if r.targets[i] != r.targets[31] {
			t.Errorf("tick %d: order %.0f W, want the %.0f W order held", i, r.targets[i], r.targets[31])
		}
	}
	if t.Failed() {
		t.Logf("targets %v", r.targets)
		t.Logf("L1 %v", r.l1)
		t.Logf("L2 %v", r.batPhase)
	}
}

// The meter repeats its 16.3 A reading from the PV step on (frozen@5)
// while the battery follows its orders. Before, every tick found the same
// 0.3 A overage and added it to a battery already moved for it: -3214,
// -3422, ... -10000 W, with the true L1 down at 4.3 A. Now the relief stops
// once the battery has moved one ampere per phase (690 W) and L1 has not
// moved: at most that ampere and one step past the first relief.
func TestFuseReliefStopsWhenTheMeterIsFrozen(t *testing.T) {
	r := pvDropRun(pvDropCase{freezeFrom: 5})
	assertReliefStops(t, r)
}

// The meter freezes on a reading that is not the worst phase's reference:
// L1 reads 16.3 A on the PV step, the relief brings it to 16.0 A, and a
// 0.2 A load on L1 starts at tick 8, the reading the meter then repeats.
// L1 moved only 0.1 A from 16.3 A, so the stop counts the battery's move
// from the reading the meter repeats, not from that reference: the order
// stops once the battery has moved one ampere per phase since the freeze.
func TestFuseReliefStopsWhenTheMeterFreezesOffTheReference(t *testing.T) {
	r := pvDropRun(pvDropCase{
		freezeFrom: 8,
		extra: func(tick int) [3]float64 {
			if tick >= 8 {
				return [3]float64{0.2, 0, 0}
			}
			return [3]float64{}
		},
	})
	const floorW = -3214 - 0.2*690 - phaseResponseA*690 - 0.2*690
	for i := 8; i < 30; i++ {
		if r.targets[i] < floorW {
			t.Errorf("tick %d: order %.0f W, past %.0f W", i, r.targets[i], floorW)
		}
		if i >= 15 && r.targets[i] != r.targets[15] {
			t.Errorf("tick %d: order %.0f W, want the %.0f W order held", i, r.targets[i], r.targets[15])
		}
	}
	if t.Failed() {
		t.Logf("targets %v", r.targets)
		t.Logf("L1 %v", r.l1)
	}
}

// A one-phase battery on L2 and L1 at 16.3 A while PV is low. The battery
// cannot reach L1, so L1 never shows its move. Before, the relief grew by
// 0.3 A of overage a tick to -4512 W and drove L2 to -16.4 A of export,
// then -18.25 A on the PV return tick. Now it stops the same way as for a
// frozen meter, and L2 stays inside the line.
func TestFuseReliefStopsWhenTheBatteryCannotReachTheWorstPhase(t *testing.T) {
	r := pvDropRun(pvDropCase{onePhase: 2, load: [3]float64{19.08, 6, 11.54}})
	assertReliefStops(t, r)
	for i, a := range r.batPhase {
		if math.Abs(a) > r.line+0.01 {
			t.Errorf("tick %d: L2 %.2f A, want inside the %.1f A line", i, a, r.line)
		}
	}
}

// assertReliefStops wants the order no further out than one ampere per
// phase and one 0.3 A step past the first relief (-3214 W), and held from
// tick 9 until PV returns.
func assertReliefStops(t *testing.T, r pvDropResult) {
	t.Helper()
	const floorW = -3214 - phaseResponseA*690 - 0.3*690
	for i := 5; i < 30; i++ {
		if r.targets[i] < floorW {
			t.Errorf("tick %d: order %.0f W, past %.0f W", i, r.targets[i], floorW)
		}
		if i >= 9 && r.targets[i] != r.targets[9] {
			t.Errorf("tick %d: order %.0f W, want the %.0f W order held", i, r.targets[i], r.targets[9])
		}
	}
	if t.Failed() {
		t.Logf("targets %v", r.targets)
		t.Logf("L1 %v", r.l1)
		if r.batPhase != nil {
			t.Logf("battery phase %v", r.batPhase)
		}
	}
}

// The meter fails to show the batteries only once they moved one ampere
// per phase (phaseResponseA, 690 W here) and either the meter repeated its
// reading all the while, or the worst phase moved less than half an ampere
// while another phase carried the move. A phase that moves becomes the new
// reference.
func TestPhaseUnresponsiveNeedsOneAmpereOfBatteryMove(t *testing.T) {
	st := phaseDirectionState(0)
	for _, c := range []struct {
		name  string
		amps  []float64
		batW  float64
		stuck bool
	}{
		{"first reading: a reference", []float64{16.3, 9, 9.5}, -3000, false},
		{"689 W moved, same reading", []float64{16.3, 9, 9.5}, -3689, false},
		{"690 W moved, same reading", []float64{16.3, 9, 9.5}, -3690, true},
		{"L1 0.49 A, L2 and L3 still: a load cancels the move", []float64{16.79, 9, 9.5}, -3690, false},
		{"L1 0.49 A, L2 and L3 one share each: a load on L1", []float64{16.79, 8, 8.5}, -3690, false},
		{"L1 0.49 A, L2 1.99 shares", []float64{16.79, 7.01, 9.5}, -3690, false},
		{"L1 0.49 A, L2 two shares: a battery off L1", []float64{16.79, 7, 9.5}, -3690, true},
		{"L1 moved 0.5 A: a new reference", []float64{15.8, 9, 9.5}, -3690, false},
		{"same reading again", []float64{15.8, 9, 9.5}, -3690, false},
		{"690 W back, same reading", []float64{15.8, 9, 9.5}, -3000, true},
		{"L1 0.1 A, L2 and L3 still", []float64{15.9, 9, 9.5}, -3000, false},
		{"690 W back again, the meter repeats L1 0.1 A off the reference", []float64{15.9, 9, 9.5}, -2310, true},
	} {
		if _, got := st.phaseUnresponsive(c.amps, map[string]float64{"bat": c.batW}); got != c.stuck {
			t.Errorf("%s: stuck %v, want %v", c.name, got, c.stuck)
		}
	}
}

// pvLowAfter5 drops PV to -440 W from tick 5 on, which puts L1 at 18.45 A
// under the -3005 W hold.
func pvLowAfter5(tick int) float64 {
	if tick >= 5 {
		return -440
	}
	return -3200
}

// The battery's reading lags the meter by a tick (207537). L1 reads
// 18.45 A at tick 5, and one relief step of -4694 W brings it to the line.
// The reading then catches up with that order while the meter stays put. A
// 0.4 A load on L1 starts at tick 15 and stays. Before, the catch-up
// counted as a move the meter did not show: the order stayed at -4694 W
// and L1 at 16.4 A to the end, and the heat model trips that after about
// three minutes. The catch-up is no new move, so the relief follows the
// load.
func TestFuseReliefFollowsALoadAfterTheReadingCatchesUp(t *testing.T) {
	r := pvDropRun(pvDropCase{
		ticks:   60,
		readLag: 1,
		pvW:     pvLowAfter5,
		extra: func(tick int) [3]float64 {
			if tick >= 15 {
				return [3]float64{0.4, 0, 0}
			}
			return [3]float64{}
		},
	})
	assertL1AtTheLine(t, r, 17, 60)
}

// L1 reads 18.45 A at tick 5 and the relief step is -4694 W. A balanced
// 2 A load per phase (a three-phase heat pump) starts in the reading that
// shows the step, so L1 reads 18.0 A, a move of 0.45 A. Before, that
// counted as a meter that does not show the battery: the order stayed at
// -4694 W with L1 at 18 A (1.125 x In) to the end, and the heat model
// trips that after about 94 s. The meter is live, so the relief goes on.
func TestFuseReliefGoesOnWhenALoadMasksTheStep(t *testing.T) {
	r := pvDropRun(pvDropCase{
		pvW: pvLowAfter5,
		extra: func(tick int) [3]float64 {
			if tick >= 6 {
				return [3]float64{2, 2, 2}
			}
			return [3]float64{}
		},
	})
	assertL1AtTheLine(t, r, 7, 40)
}

// A one-phase load on L1 rises 0.15 A a tick from tick 10 to tick 39,
// after the PV step has put L1 at the line. The battery follows, so L1
// never moves half an ampere, while L2 and L3 move with the battery.
// Before, after 690 W of following, the relief stopped until L1 had risen
// half an ampere past its 16.3 A reference: L1 climbed to 16.9 A, fell
// back and climbed again. Now L1 stays within one tick's rise of the line,
// and at the line once the rise stops.
func TestFuseReliefFollowsASlowLoadRise(t *testing.T) {
	r := pvDropRun(pvDropCase{
		ticks: 50,
		pvW: func(tick int) float64 {
			if tick >= 5 {
				return -1920
			}
			return -3200
		},
		extra: func(tick int) [3]float64 {
			return [3]float64{0.15 * float64(min(max(tick-9, 0), 30)), 0, 0}
		},
	})
	for i := 10; i < 40; i++ {
		if r.l1[i] > r.line+0.16 {
			t.Errorf("tick %d: L1 %.2f A, more than one 0.15 A rise over the %.1f A line", i, r.l1[i], r.line)
		}
	}
	assertL1AtTheLine(t, r, 41, 50)
}

// assertL1AtTheLine wants L1 at or under the line on ticks [from, to).
func assertL1AtTheLine(t *testing.T, r pvDropResult, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		if r.l1[i] > r.line+0.01 {
			t.Errorf("tick %d: L1 %.2f A, over the %.1f A line with a battery that has room", i, r.l1[i], r.line)
		}
	}
	if t.Failed() {
		t.Logf("targets %v", r.targets)
		t.Logf("L1 %v", r.l1)
	}
}
