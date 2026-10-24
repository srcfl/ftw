package control

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// cutsSince returns the offers below the one before each, from `at` on.
func (s *evMeterSite) cutsSince(at time.Time) []evTimed {
	var out []evTimed
	prev := math.Inf(1)
	for _, o := range s.offers {
		if !o.at.Before(at) && o.w < prev {
			out = append(out, o)
		}
		prev = o.w
	}
	return out
}

// Field matrix, netting rows: Easee Cloud applies a cut after 30-120 s. A
// house step on L1 takes the car from 15 A to 13 A; while that cut travels,
// a kettle adds 3 A on L1. Core cuts once for the step and once for the
// kettle's part. On master the second cut came 20 s after the first, before
// either could show, and the kettle's took the car lower still.
func TestEVFuseSlowChargerCutCoversItsOverage(t *testing.T) {
	for _, tc := range []struct {
		delay    time.Duration
		declared float64
	}{
		{40 * time.Second, 180}, {2 * time.Minute, 180}, {time.Minute, 0},
	} {
		t.Run(fmt.Sprintf("delay_%v/declared_%.0fs", tc.delay, tc.declared), func(t *testing.T) {
			s := fuseHeatSite(t)
			s.chargerDelay, s.chargerMaxAgeS = tc.delay, tc.declared
			s.run(time.Minute)
			stepAt := s.now
			s.addHouseA(6, 0, 0) // L1 26 A
			took, offer := s.firstCut(2 * time.Minute)
			if took < 0 || offer != 8970 {
				t.Fatalf("step cut after %v to %.0f W, want 8,970 W", took, offer)
			}
			s.run(15 * time.Second)
			s.addHouseA(3, 0, 0) // L1 29 A while the cut travels
			s.run(tc.delay + time.Minute)
			cuts := offerWatts(s.cutsSince(stepAt))
			if len(cuts) != 2 || cuts[0] != 8970 || cuts[1] != 6900 {
				t.Fatalf("cuts %v, want 8,970 W for the step and 6,900 W for the kettle", cuts)
			}
			if a := s.phaseA(); s.evW != 6900 || a[0] > 24 {
				t.Fatalf("car %.0f W, L1 %.1f A after both cuts landed", s.evW, a[0])
			}
		})
	}
}

// cutRig feeds EVFuseBudget site readings one second apart and applies its
// cap to the charger's offer, in 1 A steps, as the controller does.
type cutRig struct {
	t      *testing.T
	store  *telemetry.Store
	state  *State
	now    time.Time
	offerW float64
	phases int // the phases the offer lands on
}

func newCutRig(t *testing.T, offerW, declaredS float64) *cutRig {
	t.Helper()
	store, state, _, _ := evMeterBudgetFixture(t, 0, nil, offerW, "3p")
	r := &cutRig{t: t, store: store, state: state, now: time.Unix(1_792_246_834, 0), offerW: offerW, phases: 3}
	state.clock = func() time.Time { return r.now }
	data, _ := json.Marshal(map[string]any{"power_max_age_s": declaredS})
	store.Update("charger", telemetry.DerEV, 0, nil, data)
	return r
}

// read delivers one reading with phase currents and returns the cap.
func (r *cutRig) read(phaseA [3]float64) float64 {
	r.t.Helper()
	data, _ := json.Marshal(map[string]any{"l1_a": phaseA[0], "l2_a": phaseA[1], "l3_a": phaseA[2]})
	return r.deliver((phaseA[0]+phaseA[1]+phaseA[2])*230, data)
}

// readGrid delivers one reading of total import only.
func (r *cutRig) readGrid(gridW float64) float64 {
	r.t.Helper()
	return r.deliver(gridW, []byte(`{}`))
}

func (r *cutRig) deliver(gridW float64, data []byte) float64 {
	r.t.Helper()
	r.now = r.now.Add(time.Second)
	r.store.Update("meter", telemetry.DerMeter, gridW, nil, data)
	r.store.Get("meter", telemetry.DerMeter).UpdatedAt = r.now
	_, _, points, configs := evMeterBudgetFixture(r.t, 0, nil, r.offerW, "3p")
	points[0].CommandedPhases = r.phases
	capW, _ := EVFuseBudget(r.store, r.state, points, configs, 17250)
	if capW < r.offerW {
		r.offerW = math.Floor(capW/690) * 690
	}
	return capW
}

// A raise on its way adds to the cut. The car draws 10 A while its 14 A
// raise travels, and the house adds 8 A on L1: 26 A on the 24 A working
// limit. Once the heat budget is spent, the cut takes the 2 A the meter
// shows and the 4 A it does not show yet, so L1 ends at 24 A when the raise
// lands. Taking the overage from the raise alone left L1 at 28 A.
func TestEVFuseRaiseOnItsWayAddsToCut(t *testing.T) {
	r := newCutRig(t, 6900, 180)
	for range 300 {
		r.read([3]float64{18, 15, 15}) // 8 A of house on L1, 5 A on the others
	}
	r.offerW = 9660 // the raise to 14 A, sent with room for it
	r.read([3]float64{18, 15, 15})
	var cutAt int
	for i := 1; i <= 150 && r.offerW == 9660; i++ {
		r.read([3]float64{26, 15, 15})
		cutAt = i
	}
	if r.offerW != 5520 {
		t.Fatalf("offer %.0f W after %d s over, want 9,660 W less 2 A measured and 4 A on its way = 5,520 W", r.offerW, cutAt)
	}
	for range 60 {
		if r.read([3]float64{26, 15, 15}); r.offerW != 5520 {
			t.Fatalf("cut again to %.0f W while the first cut travels", r.offerW)
		}
	}
}

// A cut that has not shown once its charger can have applied it covers
// nothing more, and the next reading over the limit cuts again: after the
// declared response time, or after evUnknownSettle when nothing is declared.
func TestEVFuseCutRetriedAfterResponseTime(t *testing.T) {
	for _, tc := range []struct {
		declared float64
		want     time.Duration
	}{
		{30, 30 * time.Second}, {180, 3 * time.Minute}, {0, evUnknownSettle},
	} {
		t.Run(fmt.Sprintf("declared_%.0fs", tc.declared), func(t *testing.T) {
			s := fuseHeatSite(t)
			s.chargerMaxAgeS, s.chargerIgnores = tc.declared, true
			s.run(time.Minute)
			stepAt := s.now
			s.addHouseA(6, 0, 0) // L1 26 A, under 1.25 x 25 A
			if took, offer := s.firstCut(2 * time.Minute); took < 0 || offer != 8970 {
				t.Fatalf("step cut after %v to %.0f W", took, offer)
			}
			s.run(tc.want + 10*time.Second)
			cuts := s.cutsSince(stepAt)
			if len(cuts) != 2 {
				t.Fatalf("cuts %v, want a second one after %v", offerWatts(cuts), tc.want)
			}
			if gap := cuts[1].at.Sub(cuts[0].at); gap < tc.want || gap > tc.want+5*time.Second {
				t.Fatalf("second cut %v after the first, want %v", gap, tc.want)
			}
		})
	}
}

// Severe overage cuts whatever is on its way. Above the instant factor the
// first cut goes at once. While that cut travels, L1 stays at 33 A; once its
// filtered current passes 1.25 x 25 A, the current a gG fuse carries for an
// hour, Core cuts again without waiting the charger's 180 s.
func TestEVFuseSevereOverageCutsWhateverIsOnItsWay(t *testing.T) {
	s := fuseHeatSite(t)
	s.chargerMaxAgeS, s.chargerIgnores = 180, true
	s.run(time.Minute)
	stepAt := s.now
	s.addHouseA(13, 0, 0) // L1 33 A
	if took, offer := s.firstCut(5 * time.Second); took != 0 || offer != 4140 {
		t.Fatalf("instant cut after %v to %.0f W, want at once to 4,140 W", took, offer)
	}
	s.run(150 * time.Second)
	cuts := s.cutsSince(stepAt)
	if len(cuts) != 2 || cuts[1].w != 0 {
		t.Fatalf("cuts %v, want a second to 0 W", offerWatts(cuts))
	}
	// From 20 A the filtered current passes 31.25 A after 109 s at 33 A.
	if gap := cuts[1].at.Sub(cuts[0].at); gap < 100*time.Second || gap > 115*time.Second {
		t.Fatalf("second cut %v after the first, want once the heat passes 1.25 x In", gap)
	}
}

// A cut covers only within the run over the limit it was sent in. A fast
// charger that declares nothing applies a cut in 5 s, but counts as slow
// for 180 s. Once the meter has shown L1 under the limit for evSettle, a
// second step on L1 is a new run, and the landed cut covers none of it.
// Within evSettle of the landing it is the same run, and the landed cut
// covers the step until the run's first cut has had 180 s: that is the
// cost of trusting a cut on its way, at under 1.25 x In.
func TestEVFuseLandedCutCoversOnlyItsRun(t *testing.T) {
	for _, tc := range []struct {
		after   time.Duration // the second step after the first cut
		maxOver time.Duration
	}{
		{30 * time.Second, 30 * time.Second}, {10 * time.Second, evUnknownSettle},
	} {
		t.Run(fmt.Sprintf("step_after_%v", tc.after), func(t *testing.T) {
			s := fuseHeatSite(t)
			s.run(time.Minute)
			s.addHouseA(11, 0, 0) // L1 31 A: instant cut of 7 A, lands in 5 s
			if took, offer := s.firstCut(5 * time.Second); took != 0 || offer != 5520 {
				t.Fatalf("instant cut after %v to %.0f W, want 5,520 W", took, offer)
			}
			s.run(tc.after)
			s.addHouseA(5, 0, 0) // L1 29 A with the car's cut landed
			var over time.Duration
			var peak float64
			for range 4 * 60 {
				s.step()
				if a := s.phaseA()[0]; a > 24 {
					over += time.Second
					peak = math.Max(peak, a)
				}
			}
			if over > tc.maxOver || peak > fuseHourFactor*s.state.SiteFuseAmps {
				t.Fatalf("L1 over the working limit for %v, up to %.1f A", over, peak)
			}
			t.Logf("L1 at %.1f A over the working limit for %v", peak, over)
		})
	}
}

// A car that draws less than its offer hides a cut: the cut comes off the
// offer, not the draw, and changes nothing the meter can show. The first cut
// covers for the charger's 180 s; once it has outlasted that, cuts cover for
// evSettle only, so the next ones follow as on master. With the car at 10 A
// of 15 A and L1 2 A over, L1 stays over for the heat budget (61 s), 180 s
// and three cuts 21 s apart; master took 2 min 36 s.
func TestEVFuseCarUnderItsOfferCutsAgainAfterResponseTime(t *testing.T) {
	s := fuseHeatSite(t)
	s.carMaxW = 6900 // 10 A of its 15 A offer
	s.run(2 * time.Minute)
	stepAt := s.now
	s.addHouseA(11, 0, 0) // L1 26 A
	var over, overFor time.Duration
	for range 10 * 60 {
		s.step()
		if s.phaseA()[0] > 24 {
			over += time.Second
			overFor = over
		} else {
			over = 0
		}
	}
	if s.phaseA()[0] > 24 || overFor > evUnknownSettle+150*time.Second {
		t.Fatalf("L1 %.1f A, over the working limit for %v; cuts %v", s.phaseA()[0], overFor, offerWatts(s.cutsSince(stepAt)))
	}
	t.Logf("L1 over the working limit for %v", overFor)
}

// Without phase currents Core cannot see the hottest phase, so a cut on its
// way covers for evSettle only, as before: the total import cuts again 20 s
// after the first cut while the charger has not applied it.
func TestEVFuseWithoutPhaseCurrentsCoversForSettle(t *testing.T) {
	r := newCutRig(t, 10350, 180)
	for range 30 {
		r.readGrid(15000)
	}
	r.readGrid(20000) // 2,750 W over 17,250 W
	if r.offerW != 7590 {
		t.Fatalf("first cut to %.0f W, want 7,590 W", r.offerW)
	}
	var again int
	for i := 1; i <= 60 && r.offerW == 7590; i++ {
		r.readGrid(20000)
		again = i
	}
	if r.offerW >= 7590 || again < int(evSettle/time.Second) {
		t.Fatalf("second cut after %d s to %.0f W, want one after evSettle", again, r.offerW)
	}
}

// A change of phases is no cut. A 15 A single-phase order replaced by a 6 A
// three-phase one lowers the current on the order's phase but adds 6 A to
// the other two, so it covers nothing: with L2 at 32 A on the 25 A fuse the
// cut takes the whole offer.
func TestEVFusePhaseChangeCoversNothing(t *testing.T) {
	r := newCutRig(t, 3450, 180)
	r.phases = 1
	for range 30 {
		r.read([3]float64{20, 5, 5})
	}
	r.offerW, r.phases = 4140, 3
	r.read([3]float64{20, 5, 5})
	if r.read([3]float64{20, 32, 20}); r.offerW != 0 {
		t.Fatalf("offer %.0f W with L2 at 32 A and a 6 A three-phase order on its way, want 0 W", r.offerW)
	}
}

// After a restart, the offer Core finds in force is one it did not send in
// this run. A cut takes it as shown: the first reading over the limit trims
// the overage from it, and while that cut travels to a slow charger no
// further reading counts the found offer as a raise on its way.
func TestEVFuseRestartTakesFoundOfferAsShown(t *testing.T) {
	s := fuseHeatSite(t)
	s.chargerDelay, s.chargerMaxAgeS = time.Minute, 180
	s.addHouseA(6, 0, 0)
	s.run(30 * time.Second)
	s.boot()
	bootAt := s.now
	if took, offer := s.firstCut(5 * time.Second); took != 0 || offer != 8970 {
		t.Fatalf("restart at 26 A cut after %v to %.0f W, want the first reading to 8,970 W", took, offer)
	}
	s.run(50 * time.Second)
	if cuts := offerWatts(s.cutsSince(bootAt)); len(cuts) != 1 {
		t.Fatalf("cuts %v while the first travels", cuts)
	}
}
