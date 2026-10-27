package control

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// C2 of the 27 Oct rig triage: a battery charge rise fits each phase's room,
// (fuseLineA − measured phase − our unshown rises) × V × phases, before it is
// sent. 16 A fuse, 230 V, 3 phases.

// riseStore holds one meter reading with these phase currents, all import,
// and the battery's reading.
func riseStore(t *testing.T, phaseA [3]float64, batW float64) *telemetry.Store {
	t.Helper()
	data := map[string]any{}
	var totalW float64
	for i, a := range phaseA {
		data[fmt.Sprintf("l%d_a", i+1)] = math.Abs(a)
		data[fmt.Sprintf("l%d_w", i+1)] = a * 230
		totalW += a * 230
	}
	return phaseDirectionStore(t, totalW, batW, data)
}

// riseState asks the battery for askW of charge (Charge mode at a charge
// limit of askW), so the fuse path alone shapes the order.
func riseState(askW float64) *State {
	st := phaseDirectionState(0)
	st.Mode = ModeCharge
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st.clock = func() time.Time { return now }
	st.batteryCommands = map[string]commandLog{}
	ask(st, askW)
	return st
}

func ask(st *State, w float64) {
	st.DriverLimits = map[string]PowerLimits{"bat": {MaxChargeW: w, MaxDischargeW: 10000}}
}

func batteryOrder(t *testing.T, store *telemetry.Store, st *State) float64 {
	t.Helper()
	for _, o := range ComputeDispatch(store, st, map[string]float64{"bat": 15200}, 16*230*3) {
		if o.Driver == "bat" {
			return o.TargetW
		}
	}
	t.Fatal("no battery target")
	return 0
}

// Seed 207530 at 897 s: L3 reads 16.04 A, over the 16 A line, with the
// battery at 491 W after a cut to 491 W. A plan target of 3350 W is no rise
// room: the order stays at or below the battery's reading.
func TestBatteryRiseIntoPhaseOverTheLine(t *testing.T) {
	store := riseStore(t, [3]float64{8.5, 10.5, 16.04}, 491)
	st := riseState(3350)
	sent(st, "bat", 3349, 491)
	if got := batteryOrder(t, store, st); got > 491+1e-6 {
		t.Fatalf("order %.0f W with L3 at 16.04 A and the battery at 491 W; want <= 491 W", got)
	}
}

// Seed 207535 at 37-41 s, with no margin. L3 reads 14.27 A with the battery
// at 12 W: 871 W fits under the line and goes out whole. Two seconds later
// the battery reads 871 W, but the meter's reading is from before the
// battery moved and still shows L3 at 14.27 A. The 871 W rise is ours and
// not shown, so a further ask may add only what is left above it: the order
// stays at or below 12 W + (16 − 14.27) × 690 W.
func TestBatteryRiseCountsOwnUnshownRise(t *testing.T) {
	phases := [3]float64{13.77, 13.97, 14.27}
	limitW := 12 + (16-14.27)*690

	st := riseState(871)
	sent(st, "bat", 12)
	if got := batteryOrder(t, riseStore(t, phases, 12), st); math.Abs(got-871) > 1e-6 {
		t.Fatalf("first ask: order %.1f W, want the whole 871 W (it fits under %.1f W)", got, limitW)
	}

	st = riseState(1500)
	sent(st, "bat", 12, 871)
	got := batteryOrder(t, riseStore(t, phases, 871), st)
	if got > limitW+1e-6 {
		t.Fatalf("order %.1f W while the meter does not show our 871 W yet: L3 would reach %.2f A; want <= %.1f W",
			got, 14.27+(got-12)/690, limitW)
	}
	if got < 871-1e-6 {
		t.Fatalf("order %.1f W cut below our 871 W hold; a rise check never cuts", got)
	}
}

// Closed loop, battery only, no margin: house loads 13.77/13.97/14.27 A and
// a charge ask that grows 300 W a tick from 600 W to 3000 W, as a PV
// surplus does. The battery reaches each order one tick later; the meter
// shows the battery one tick after the battery's own reading does. L3
// never passes the 16 A line, the order never moves down (no hunting), and
// it ends at the line: 1193.7 W.
func TestBatteryRiseClosedLoopStopsAtTheLine(t *testing.T) {
	house := [3]float64{13.77, 13.97, 14.27}
	st := riseState(600)
	now := st.now()
	st.clock = func() time.Time { return now }
	var orders, l3 []float64
	batW, meterBatW := 0.0, 0.0
	for tick := 0; tick < 40; tick++ {
		ask(st, math.Min(3000, 600+300*float64(tick)))
		var a [3]float64
		for i := range a {
			a[i] = house[i] + meterBatW/690
		}
		order := batteryOrder(t, riseStore(t, a, batW), st)
		orders = append(orders, math.Round(order*10)/10)
		l3 = append(l3, math.Round(a[2]*1000)/1000)
		meterBatW, batW = batW, order
		now = now.Add(2 * time.Second)
	}
	for i := range orders {
		if l3[i] > 16+1e-3 {
			t.Errorf("tick %d: L3 %.3f A over the 16 A line", i, l3[i])
		}
		if i > 0 && orders[i] < orders[i-1]-0.5 {
			t.Errorf("tick %d: order fell %.1f -> %.1f W", i, orders[i-1], orders[i])
		}
	}
	if want := (16 - 14.27) * 690; math.Abs(orders[len(orders)-1]-want) > 1 {
		t.Errorf("final order %.1f W, want %.1f W: L3 at the line", orders[len(orders)-1], want)
	}
	if t.Failed() {
		t.Logf("orders %v", orders)
		t.Logf("L3 %v", l3)
	}
}

// Car first: the car's raise from 6 A to 10 A per phase went out 2 s ago
// and the meter does not show it yet: L3 reads 11 A with the car at 6 A.
// The raise takes L3 to 15 A, so the battery's 2000 W charge ask gets the
// 1 A left, 690 W. Once the meter shows the car at 10 A and the battery at
// 690 W, L3 sits at the 16 A line and the battery holds. With the car's demand
// and without it (a raise still on its way after the demand left).
func TestBatteryRiseLeavesRoomForUnshownCarRaise(t *testing.T) {
	for _, demand := range []bool{true, false} {
		t.Run(fmt.Sprintf("demand=%v", demand), func(t *testing.T) {
			st := riseState(2000)
			now := st.now()
			st.clock = func() time.Time { return now }
			sent(st, "bat", 0)
			var log commandLog
			log = log.keep(sentCommand{at: now.Add(-evSettle - time.Second), w: 4140, a: 6, n: 3}, now.Add(-evSettle))
			log = log.keep(sentCommand{at: now.Add(-2 * time.Second), w: 6900, a: 10, n: 3}, now.Add(-evSettle))
			st.evLimit.offers = map[string]chargerOffers{"car": {log: log, settle: evSettle}}
			st.EVCommandedW = 6900
			if demand {
				st.evFuseDemands = []evFuseDemand{{"car", 6900, "3p"}}
			}
			got := batteryOrder(t, riseStore(t, [3]float64{9, 9, 11}, 0), st)
			if got > 690+1e-6 {
				t.Fatalf("battery %.1f W beside the car's unshown 4 A: L3 would reach %.2f A", got, 11+4+got/690)
			}

			now = now.Add(evSettle)
			got = batteryOrder(t, riseStore(t, [3]float64{13 + got/690, 13 + got/690, 15 + got/690}, got), st)
			if math.Abs(got-690) > 1 {
				t.Fatalf("battery %.1f W once the meter shows the car, want 690 W: L3 at the 16 A line", got)
			}
		})
	}
}
