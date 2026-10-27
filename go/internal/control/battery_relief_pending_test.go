package control

import (
	"math"
	"testing"
)

// Reproductions ported from srcfl/ftw#1816 (segran2) to master's
// capBatteryCharge and clampEVBatteryHeadroom. Master's keepEndingDischarge
// (#1847) holds them. Import direction only: master leaves the export
// direction to the fuse pipeline.
//
// Site: 25 A, three phases, 230 V, 1 A margin. Phases read 22.3/12.3/12.3 A,
// which leaves 1.7 A on L1, or 1,173 W of balanced battery change.
const reliefRoomW = (24 - 22.3) * 690

func reliefFixturePhases() []float64 { return []float64{22.3, 12.3, 12.3} }

// l1AfterA is L1 once the batteries move from what they measure to their
// targets, all else unchanged.
func l1AfterA(out []DispatchTarget, measured map[string]float64) float64 {
	a := 22.3
	for _, target := range out {
		a += (target.TargetW - measured[target.Driver]) / 690
	}
	return a
}

// Ending a discharge is our raise. The battery measures -2,000 W and our
// newer -1,000 W order may not show yet, so 1,000 W of the raise is already
// on its way. Ending the rest at once spends the same room twice: L1 would
// reach 25.2 A. The battery keeps -827 W and L1 lands on 24 A.
func TestEndingReliefReservesPendingRise(t *testing.T) {
	store, state := evSplitFixture(t, 10787, reliefFixturePhases(), 4140, "3p")
	sent(state, "ev", 4140, 4140)
	sent(state, "first", -2000, -1000)
	batteryReading(store, "first", -2000, .5)
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: 0}}, store, state, 17250)
	got := targetW(out, "first")
	if math.Abs(got-(-2000+reliefRoomW)) > .01 {
		t.Fatalf("battery %.1f W, want %.1f W", got, -2000+reliefRoomW)
	}
	if l1 := l1AfterA(out, map[string]float64{"first": -2000}); l1 > 24.001 {
		t.Fatalf("L1 %.3f A, over the 24 A line", l1)
	}
}

// The relief kept never goes past the room the phase lacks, the battery's
// latest order or its command bounds.
func TestEndingReliefPendingRiseBounds(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		pending, room, last, lowest float64
		want                        float64
	}{
		{"pending", 1000, 1173, -1000, -5000, -827},
		{"settled", 0, 1173, -1000, -5000, 0},
		{"room for both", 1000, 2500, -1000, -5000, 0},
		{"discharge blocked", 1000, 1173, -1000, 0, 0},
		{"bounds limit the relief", 1000, 1173, -1000, -500, -500},
		{"no relief to keep", 2000, 1173, 0, -5000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := capBatteryCharge([]DispatchTarget{{Driver: "first", TargetW: 0}},
				map[string]float64{"first": tc.last}, map[string]float64{"first": tc.lowest},
				tc.pending, tc.room, tc.room)
			if got := out[0].TargetW; math.Abs(got-tc.want) > 1e-6 {
				t.Fatalf("battery %.3f W, want %.3f W", got, tc.want)
			}
		})
	}
}

// Two batteries end their relief: first at -1,000 W, second at -2,000 W.
// The phase takes 1,173 W of the 3,000 W raise, so 1,827 W must stay. A
// battery that cannot keep its share must not take it from one that can:
// in #1816's old head, first got 609 W it could not keep and second kept
// 1,218 W, so L1 reached 24.88 A.
func TestEndingReliefSharesOnlyAvailableSupply(t *testing.T) {
	for _, tc := range []struct {
		name         string
		firstSoC     float64
		firstMaxDisW float64
		wantFirst    float64 // NaN: only the total and bounds are checked.
		wantSecond   float64
	}{
		{"first at its SoC floor", .01, 5000, 0, -1827},
		{"first bounded to 300 W", .5, 300, math.NaN(), math.NaN()},
	} {
		for _, reverse := range []bool{false, true} {
			name := tc.name
			if reverse {
				name += " reversed"
			}
			t.Run(name, func(t *testing.T) {
				store, state := evSplitFixture(t, 10787, reliefFixturePhases(), 4140, "3p")
				state.DriverLimits["first"] = PowerLimits{MaxChargeW: 10000, MaxDischargeW: tc.firstMaxDisW}
				sent(state, "ev", 4140, 4140)
				sent(state, "first", -1000)
				sent(state, "second", -2000)
				batteryReading(store, "first", -1000, tc.firstSoC)
				batteryReading(store, "second", -2000, .5)
				targets := []DispatchTarget{{Driver: "first", TargetW: 0}, {Driver: "second", TargetW: 0}}
				if reverse {
					targets[0], targets[1] = targets[1], targets[0]
				}
				out := clampEVBatteryHeadroom(targets, store, state, 17250)
				first, second := targetW(out, "first"), targetW(out, "second")
				if !math.IsNaN(tc.wantFirst) && (math.Abs(first-tc.wantFirst) > .01 || math.Abs(second-tc.wantSecond) > .01) {
					t.Fatalf("first %.1f W, second %.1f W; want %.1f W, %.1f W", first, second, tc.wantFirst, tc.wantSecond)
				}
				if first > 0 || first < -tc.firstMaxDisW-1e-6 || second > 0 || second < -2000-1e-6 {
					t.Fatalf("relief past what each battery can keep: first %.1f W, second %.1f W", first, second)
				}
				if l1 := l1AfterA(out, map[string]float64{"first": -1000, "second": -2000}); math.Abs(l1-24) > .001 {
					t.Fatalf("L1 %.3f A, want 24 A", l1)
				}
			})
		}
	}
}

// The same sharing at capBatteryCharge, with bounds as lowest orders.
func TestEndingReliefSharingBounds(t *testing.T) {
	for _, tc := range []struct {
		name                                          string
		lastFirst, lowestFirst, lowestSecond, pending float64
		wantFirst, wantSecond                         float64
	}{
		{"first blocked", -1000, 0, -5000, 0, 0, -1827},
		{"too little supply", -1000, -100, -700, 0, -100, -700},
		{"first had no relief", 0, -5000, -5000, 0, 0, -827},
		{"pending past supply", -1000, 0, -5000, 1000, 0, -2000},
	} {
		for _, reverse := range []bool{false, true} {
			name := tc.name
			if reverse {
				name += " reversed"
			}
			t.Run(name, func(t *testing.T) {
				targets := []DispatchTarget{{Driver: "first", TargetW: 0}, {Driver: "second", TargetW: 0}}
				if reverse {
					targets[0], targets[1] = targets[1], targets[0]
				}
				out := capBatteryCharge(targets,
					map[string]float64{"first": tc.lastFirst, "second": -2000},
					map[string]float64{"first": tc.lowestFirst, "second": tc.lowestSecond},
					tc.pending, reliefRoomW, reliefRoomW)
				if first, second := targetW(out, "first"), targetW(out, "second"); math.Abs(first-tc.wantFirst) > 1e-6 || math.Abs(second-tc.wantSecond) > 1e-6 {
					t.Fatalf("first %.3f W, second %.3f W; want %.3f W, %.3f W", first, second, tc.wantFirst, tc.wantSecond)
				}
			})
		}
	}
}
