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
	st := phaseDirectionState(0)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st.clock = func() time.Time { return now }
	st.SetBatteryManualHold(BatteryManualHold{PowerW: -3005, ExpiresAt: now.Add(time.Hour)})

	store := telemetry.NewStore()
	load := [3]float64{23.44, 11.04, 11.54} // house and car, A import per phase
	pvW, batW := -3200.0, -3005.0
	line := st.fuseLineA()
	var targets, l1 []float64
	for tick := 0; tick < 40; tick++ {
		switch tick {
		case 5:
			pvW = -1920
		case 30:
			pvW = -3200
		}
		var a [3]float64
		data := map[string]any{}
		var totalW float64
		for i := range a {
			a[i] = load[i] + (pvW+batW)/690
			data[fmt.Sprintf("l%d_a", i+1)] = math.Abs(a[i])
			data[fmt.Sprintf("l%d_w", i+1)] = a[i] * 230
			totalW += a[i] * 230
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
		targets = append(targets, math.Round(batW))
		l1 = append(l1, math.Round(a[0]*100)/100)
		now = now.Add(5 * time.Second)
	}
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
