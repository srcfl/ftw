package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The battery fuse paths read each phase with its direction and count the
// batteries' own move. 16 A fuse, 230 V, 3 phases; the working limit is
// 15 A.

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
// battery that follows its order keeps L1 at the working limit on every
// tick instead of returning to 0 W when the overage stops showing.
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
	for i := 1; i < len(l1); i++ {
		if l1[i] > 15 {
			t.Fatalf("tick %d: L1 %.1f A over the 15 A working limit while the overload persists; targets %v, L1 %v", i, l1[i], targets, l1)
		}
	}
}
