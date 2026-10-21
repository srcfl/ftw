package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// secondsToSpend runs the model at 1 s readings from fromA to atA and
// returns how long after the first reading at atA the filtered current is
// over limitA.
func secondsToSpend(fromA, atA, limitA float64) int {
	var h fuseHeat
	t := time.Unix(0, 0)
	h.step(t, []float64{fromA})
	for s := 1; s <= 3600; s++ {
		h.step(t.Add(time.Duration(s)*time.Second), []float64{atA})
		if h.sq[0] > limitA*limitA {
			return s - 1
		}
	}
	return -1
}

// From 20 A on a 25 A fuse the budget runs out at the working limit of
// 24 A after about a minute at 26 A, 37 s at 28 A and 26 s at 30 A. A 10 s
// spike to 32 A leaves 22.3 A of heat.
func TestFuseHeatSpendsBudgetFrom20A(t *testing.T) {
	for _, tc := range []struct {
		atA  float64
		want int
	}{{26, 61}, {28, 37}, {30, 26}} {
		if got := secondsToSpend(20, tc.atA, 24); math.Abs(float64(got-tc.want)) > 1 {
			t.Errorf("%.0f A spent the budget after %d s, want ~%d s", tc.atA, got, tc.want)
		}
	}
	var h fuseHeat
	t0 := time.Unix(0, 0)
	h.step(t0, []float64{20})
	for s := 1; s <= 11; s++ {
		a := 32.0
		if s == 11 {
			a = 20 // the reading that ends the spike
		}
		h.step(t0.Add(time.Duration(s)*time.Second), []float64{a})
	}
	if got := h.worstA(); math.Abs(got-22.3) > .05 {
		t.Fatalf("10 s at 32 A left %.2f A of heat, want 22.3 A", got)
	}
}

// fuseHeatState is a 25 A three-phase site with phase currents on the meter.
func fuseHeatState() (*telemetry.Store, *State) {
	state := NewState(0, 0, "meter")
	state.SiteFuseAmps, state.SiteFuseVoltage, state.SiteFusePhases = 25, 230, 3
	return telemetry.NewStore(), state
}

func meterAt(store *telemetry.Store, at time.Time, phaseA [3]float64) *telemetry.DerReading {
	raw, _ := json.Marshal(map[string]float64{"l1_a": phaseA[0], "l2_a": phaseA[1], "l3_a": phaseA[2]})
	store.Update("meter", telemetry.DerMeter, (phaseA[0]+phaseA[1]+phaseA[2])*230, nil, raw)
	r := store.Get("meter", telemetry.DerMeter)
	r.UpdatedAt = at
	return r
}

// A phase's current takes its direction from the same phase's power: P1
// reports current as a magnitude, Pixii signs it. Without the power the
// current counts as import.
func TestPhaseCurrentsTakeDirectionFromPhasePower(t *testing.T) {
	_, state := fuseHeatState()
	for _, tc := range []struct {
		name string
		data map[string]float64
		want []float64
	}{
		{"p1", map[string]float64{"l1_a": 20, "l2_a": 3, "l3_a": 5, "l1_w": -4600, "l2_w": 690, "l3_w": 0}, []float64{-20, 3, 5}},
		{"signed", map[string]float64{"l1_a": -20, "l2_a": -3, "l3_a": 5, "l1_w": -4600, "l2_w": -690, "l3_w": 1150}, []float64{-20, -3, 5}},
		{"no phase power", map[string]float64{"l1_a": -20, "l2_a": 3, "l3_a": 5}, []float64{20, 3, 5}},
	} {
		raw, _ := json.Marshal(tc.data)
		amps, present, valid := phaseCurrentsA(&telemetry.DerReading{Data: raw}, state)
		if !present || !valid || len(amps) != 3 || amps[0] != tc.want[0] || amps[1] != tc.want[1] || amps[2] != tc.want[2] {
			t.Errorf("%s: %v (present %v, valid %v), want %v", tc.name, amps, present, valid, tc.want)
		}
	}
}

// While the meter is silent the heat holds. A gap counts at the last
// current seen: the reading after it neither fills the gap with its own
// current nor erases an overload from before it.
func TestFuseHeatCountsGapAtLastCurrentSeen(t *testing.T) {
	gap := 30 * time.Second
	if gap <= evSettle {
		t.Fatalf("test gap %v must exceed evSettle %v", gap, evSettle)
	}
	for _, tc := range []struct {
		name          string
		before, after float64
	}{
		{"spike_after_gap", 20, 28},
		{"overload_before_gap", 28, 20},
	} {
		store, state := fuseHeatState()
		t0 := time.Unix(1_800_000_000, 0)
		state.fusePhasesAt(meterAt(store, t0.Add(-time.Hour), [3]float64{20, 20, 20}))
		state.fusePhasesAt(meterAt(store, t0, [3]float64{tc.before, 20, 20}))
		held := state.fuseHeat.sq
		silent := store.Get("meter", telemetry.DerMeter)
		for range 30 {
			state.fusePhasesAt(silent) // ticks with no new reading
		}
		if state.fuseHeat.sq != held {
			t.Fatalf("%s: heat moved to %v without a reading", tc.name, state.fuseHeat.sq)
		}
		state.fusePhasesAt(meterAt(store, t0.Add(gap), [3]float64{tc.after, 20, 20}))
		a := tc.before
		want := a*a - (a*a-held[0])*math.Exp(-gap.Seconds()/60)
		if got := state.fuseHeat.sq[0]; math.Abs(got-want) > 1e-9 {
			t.Fatalf("%s: heat after a %v gap %.3f A², want %.3f A²", tc.name, gap, got, want)
		}
		if state.fuseHeat.sq[1] != 400 {
			t.Fatalf("%s: a steady phase moved to %.3f A²", tc.name, state.fuseHeat.sq[1])
		}
	}
}

// A Core restart has no memory: the first reading sets the heat.
func TestFuseHeatStartsAtFirstReadingAfterRestart(t *testing.T) {
	store, state := fuseHeatState()
	state.fusePhasesAt(meterAt(store, time.Unix(1_800_000_000, 0), [3]float64{26, 3, 0}))
	if state.fuseHeat.sq != [3]float64{676, 9, 0} {
		t.Fatalf("first reading set %v", state.fuseHeat.sq)
	}
}

// fuseHeatSite charges at 15 A beside 5 A of house load on each phase of a
// 25 A fuse with no import ceiling: 20 A per phase, long enough for the heat
// to settle there.
func fuseHeatSite(t *testing.T) *evMeterSite {
	t.Helper()
	s := newEVMeterSite(t, 0, 3450)
	s.run(10 * time.Minute)
	if s.offer() != 10350 || s.evW != 10350 || math.Abs(s.state.fuseHeat.worstA()-20) > .01 {
		t.Fatalf("setup: offer %.0f draw %.0f heat %.2f A", s.offer(), s.evW, s.state.fuseHeat.worstA())
	}
	return s
}

// addHouseA adds current to the house on each phase.
func (s *evMeterSite) addHouseA(l1, l2, l3 float64) {
	s.housePhaseA[0] += l1
	s.housePhaseA[1] += l2
	s.housePhaseA[2] += l3
	s.houseW += (l1 + l2 + l3) * 230
}

// firstCut runs until the offer drops below 10,350 W and returns how long
// after the first reading of the run that was.
func (s *evMeterSite) firstCut(limit time.Duration) (time.Duration, float64) {
	s.t.Helper()
	first := s.now.Add(time.Second)
	for s.now.Sub(first) < limit {
		s.step()
		if s.offer() < 10350 {
			return s.now.Sub(first), s.offer()
		}
	}
	return -1, s.offer()
}

// A house step on L1 from 20 A cuts once the heat budget is spent, not on
// the first reading over the working limit, and the cut brings L1 back
// under it.
func TestEVHardLimitPhaseCutWaitsForHeatBudget(t *testing.T) {
	for _, tc := range []struct {
		atA    float64
		after  time.Duration
		offerW float64
	}{
		{26, 61 * time.Second, 8970}, // 2 A over
		{28, 37 * time.Second, 7590}, // 4 A over
		{30, 26 * time.Second, 6210}, // 6 A over, at 1.2 × In exactly
	} {
		s := fuseHeatSite(t)
		s.addHouseA(tc.atA-20, 0, 0)
		took, offer := s.firstCut(3 * time.Minute)
		if took < tc.after-2*time.Second || took > tc.after+2*time.Second {
			t.Errorf("%.0f A on L1 cut after %v, want ~%v", tc.atA, took, tc.after)
		}
		if offer != tc.offerW {
			t.Errorf("%.0f A on L1 cut to %.0f W, want %.0f W", tc.atA, offer, tc.offerW)
		}
		s.run(time.Minute)
		if a := s.phaseA(); a[0] > 24+1e-9 {
			t.Errorf("%.0f A on L1: L1 at %.2f A after the cut", tc.atA, a[0])
		}
	}
}

// Short house spikes, even past the fuse on all three phases at once, fit
// in the heat budget and never move the offer.
func TestEVHardLimitHeatBudgetCarriesHouseSpikes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		l1, l2, l3 float64
		lasts      time.Duration
	}{
		// A three-phase heat pump compressor start: 28 A on every phase,
		// 19.3 kW in total against 16.9 kW of fuse, for 5 s.
		{"heat_pump_start", 8, 8, 8, 5 * time.Second},
		// 29.5 A on L1, just under 1.2 × In, for 10 s.
		{"l1_10s_under_instant", 9.5, 0, 0, 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fuseHeatSite(t)
			from := s.now
			s.addHouseA(tc.l1, tc.l2, tc.l3)
			s.run(tc.lasts)
			if s.maxGridW <= 16905 && tc.l2 > 0 {
				t.Fatalf("setup: the spike never passed the total fuse ceiling (%.0f W)", s.maxGridW)
			}
			s.addHouseA(-tc.l1, -tc.l2, -tc.l3)
			s.run(2 * time.Minute)
			for _, w := range offerWatts(s.offersSince(from)) {
				if w != 10350 {
					t.Fatalf("spike moved the offer: %v", offerWatts(s.offersSince(from)))
				}
			}
		})
	}
}

// A three-phase oven that stays on spends the budget once and cuts once,
// by what the meter shows over the working limit, then holds.
func TestEVHardLimitSustainedOvenCutsOnceAfterBudget(t *testing.T) {
	s := fuseHeatSite(t)
	from := s.now
	s.addHouseA(5.5, 5.5, 5.5) // 25.5 A per phase, 17.6 kW against 16.9 kW
	took, offer := s.firstCut(3 * time.Minute)
	// θ goes from 400 toward 650.25 A² and passes 576 after
	// 60 × ln(250.25/74.25) = 72.9 s.
	if took < 71*time.Second || took > 75*time.Second || offer != 8970 {
		t.Fatalf("oven cut after %v to %.0f W, want ~73 s to 8,970 W", took, offer)
	}
	s.run(5 * time.Minute)
	got := offerWatts(s.offersSince(from))
	for i, w := range got {
		if w != 10350 && w != 8970 || i > 0 && w > got[i-1] {
			t.Fatalf("oven gave offers %v, want one cut to 8,970 W", got)
		}
	}
	if a := s.phaseA(); a[0] > 24 || s.state.fuseHeat.worstA() > 24 {
		t.Fatalf("after the cut L1 %.2f A, heat %.2f A", a[0], s.state.fuseHeat.worstA())
	}
}

// Above 1.2 × In a phase gets no budget: the first reading cuts it back to
// the working limit, even for a spike that would fit in the heat budget.
func TestEVHardLimitCutsAtOnceAboveInstantLimit(t *testing.T) {
	for _, tc := range []struct {
		atA    float64
		offerW float64
	}{{31, 5520}, {32, 4830}} {
		s := fuseHeatSite(t)
		s.addHouseA(tc.atA-20, 0, 0)
		took, offer := s.firstCut(10 * time.Second)
		if took != 0 || offer != tc.offerW {
			t.Fatalf("%.0f A cut after %v to %.0f W, want the first reading to %.0f W", tc.atA, took, offer, tc.offerW)
		}
	}
}

// A silent meter holds the heat and counts the gap at the last current it
// showed. A house step during the gap starts to count from the first reading
// that shows it, so the cut lands a budget after the meter returns. An
// overload the meter never showed is not counted.
func TestEVHardLimitHeatBudgetAcrossMeterGap(t *testing.T) {
	s := fuseHeatSite(t)
	s.addHouseA(6, 0, 0) // 26 A on L1
	s.meterSilent = true
	held := s.state.fuseHeat.sq
	s.run(30 * time.Second)
	if s.offer() != 10350 || s.state.fuseHeat.sq != held {
		t.Fatalf("during the gap: offer %.0f W heat %v, want %v", s.offer(), s.state.fuseHeat.sq, held)
	}
	s.meterSilent = false
	took, offer := s.firstCut(2 * time.Minute)
	if took < 59*time.Second || took > 63*time.Second || offer != 8970 {
		t.Fatalf("26 A after a 30 s gap cut %v after the meter returned, to %.0f W; want ~61 s to 8,970 W", took, offer)
	}
}

// After a Core restart the heat starts at the first reading. Restarting
// 30 s into 26 A on L1 counts the fuse as already at 26 A and cuts at once,
// which is conservative only because that reading is high.
func TestEVHardLimitRestartStartsHeatAtFirstReading(t *testing.T) {
	s := fuseHeatSite(t)
	s.addHouseA(6, 0, 0)
	s.run(30 * time.Second)
	if s.offer() != 10350 {
		t.Fatalf("setup: cut before the budget: %.0f W", s.offer())
	}
	s.boot()
	took, offer := s.firstCut(5 * time.Second)
	if took != 0 || offer != 8970 {
		t.Fatalf("restart at 26 A cut after %v to %.0f W, want the first reading to 8,970 W", took, offer)
	}
}

// peaks runs the model on 17 A for 10 s and 14 A for gap, 30 times from
// 14 A, at 1 s readings, and returns the highest filtered current and how
// long after the first 17 A reading it was first over limitA.
func peaks(gap int, limitA float64) (maxA float64, firstOver int) {
	var h fuseHeat
	t := time.Unix(0, 0)
	h.step(t, []float64{14})
	for s := 1; s <= 30*(10+gap); s++ {
		a := 14.0
		if (s-1)%(10+gap) < 10 {
			a = 17
		}
		h.step(t.Add(time.Duration(s)*time.Second), []float64{a})
		maxA = math.Max(maxA, h.worstA())
		if firstOver == 0 && h.sq[0] > limitA*limitA {
			firstOver = s - 1
		}
	}
	return maxA, firstOver
}

// Fredrik, 17 Oct 2026: on a 16 A fuse a steady 17 A must come down, but
// 10 s peaks to 17 A with 14 A between them may be fine in total, since a
// fuse trips on heat. The working limit is 15 A. A phase cuts when its
// filtered current is strictly over the limit; exactly on it is inside, as
// for a reading. The 30 s pattern goes 0.011 A over at the end of its
// seventh peak, so either rule counts it as over.
func TestFuseHeatSixteenAmpPeaks(t *testing.T) {
	for _, tc := range []struct {
		gap       int
		maxA      float64
		firstOver int // 0: never
	}{
		{50, 14.78, 0},
		{30, 15.01, 250},
		{20, 15.24, 98},
		{10, 15.70, 48},
	} {
		maxA, firstOver := peaks(tc.gap, 15)
		if math.Abs(maxA-tc.maxA) > .005 || firstOver != tc.firstOver {
			t.Errorf("17 A 10 s / 14 A %d s: max %.3f A, over at %d s; want %.2f A, %d s", tc.gap, maxA, firstOver, tc.maxA, tc.firstOver)
		}
	}
	if got := secondsToSpend(14, 17, 15); got != 23 {
		t.Errorf("a steady 17 A spent the budget after %d s, want 23 s", got)
	}
}

// fuse16Site is Fredrik's case end to end: a 16 A fuse, 4 A of house load
// and the car at 10 A on each phase, 14 A in all, settled.
func fuse16Site(t *testing.T) *evMeterSite {
	t.Helper()
	s := newEVMeterSite(t, 0, 2760)
	s.state.SiteFuseAmps = 16
	s.ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	s.ctrl.SetFuseEVMax(func() (float64, bool) {
		return EVFuseBudget(s.store, s.state, s.mgr.States(), s.mgr.Configs(), 16*690)
	})
	s.run(10 * time.Minute)
	if s.offer() != 6900 || s.phaseA()[0] != 14 || math.Abs(s.state.fuseHeat.worstA()-14) > .01 {
		t.Fatalf("setup: offer %.0f W L1 %.2f A heat %.2f A", s.offer(), s.phaseA()[0], s.state.fuseHeat.worstA())
	}
	return s
}

// The same peaks on L1 with the car charging: patterns whose heat stays
// under 15 A while the meter shows a peak never move the offer; the others
// cut once when the budget is spent and settle where the peaks fit. A rise reads the heat as well as
// the reading, so the car does not climb back between peaks and cut again.
func TestEVHardLimitSixteenAmpPeaksCutOnceAndSettle(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gap      time.Duration // 0: the 17 A stays
		cutAfter time.Duration // 0: never
		settleW  float64
	}{
		{"14A_50s", 50 * time.Second, 0, 6900},
		// The heat passes 15 A only as the peak ends, when the meter
		// already shows 14 A: nothing is left to cut.
		{"14A_30s", 30 * time.Second, 0, 6900},
		{"14A_20s", 20 * time.Second, 98 * time.Second, 6210},
		{"14A_10s", 10 * time.Second, 48 * time.Second, 5520},
		{"steady_17A", 0, 23 * time.Second, 5520},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fuse16Site(t)
			from, maxHeat := s.now, 0.0
			run := func(d time.Duration) {
				for end := s.now.Add(d); s.now.Before(end); {
					s.step()
					maxHeat = math.Max(maxHeat, s.state.fuseHeat.worstA())
				}
			}
			if tc.gap == 0 {
				s.addHouseA(3, 0, 0)
				run(10 * time.Minute)
			} else {
				for range 30 {
					s.addHouseA(3, 0, 0)
					run(10 * time.Second)
					s.addHouseA(-3, 0, 0)
					run(tc.gap)
				}
			}
			var cuts []evTimed
			offers := s.offersSince(from)
			for i := 1; i < len(offers); i++ {
				if offers[i].w < offers[i-1].w {
					cuts = append(cuts, offers[i])
				}
			}
			want := 0
			if tc.cutAfter > 0 {
				want = 1
			}
			if len(cuts) != want {
				t.Fatalf("cuts %v, want %d", offerWatts(cuts), want)
			}
			if tc.cutAfter > 0 {
				// From the first reading of the first peak.
				if took := cuts[0].at.Sub(from) - time.Second; took < tc.cutAfter-2*time.Second || took > tc.cutAfter+2*time.Second {
					t.Errorf("cut after %v, want ~%v", took, tc.cutAfter)
				}
			}
			if s.offer() != tc.settleW {
				t.Errorf("settled at %.0f W, want %.0f W", s.offer(), tc.settleW)
			}
			if maxHeat > 15.2 {
				t.Errorf("heat reached %.3f A on a 15 A working limit", maxHeat)
			}
		})
	}
}

// Göran, 14:20:34Z on v0.147.2-beta.1: one Pixii reading gave 19,174 W of
// total import while its worst phase was 5.7 A on a 25 A fuse. The total
// against 16,905 W cut the car from 10,350 W to 8,081 W, and it took about a
// minute to climb back. With phase currents the phases judge the fuse, so
// the offer holds; without them the same total still cuts.
func TestEVHardLimitHoldsWhenTotalImportDisagreesWithPhases(t *testing.T) {
	store, state, points, configs := evMeterBudgetFixture(t, 0, nil, 10350, "3p")
	state.SiteFuseSafetyA = 0.5 // 17,250 - 345 = 16,905 W, as on the site
	t0 := time.Unix(1_792_246_834, 0)
	now := t0
	state.clock = func() time.Time { return now }
	read := func(at time.Time, gridW float64, phaseA []float64) float64 {
		t.Helper()
		now = at
		data := map[string]any{"control_power_available": true, "control_power_observed_at": at.Format(time.RFC3339Nano)}
		if phaseA != nil {
			data["l1_a"], data["l2_a"], data["l3_a"] = phaseA[0], phaseA[1], phaseA[2]
		}
		raw, _ := json.Marshal(data)
		store.Update("meter", telemetry.DerMeter, gridW, nil, raw)
		store.Get("meter", telemetry.DerMeter).UpdatedAt = at
		capW, _ := EVFuseBudget(store, state, points, configs, 17250)
		return capW
	}
	for i, r := range []struct {
		gridW  float64
		phaseA []float64
	}{
		{15334, []float64{12.3, 9.5, 7.9}},
		{19174, []float64{5.7, 4.9, 4.1}}, // the reading that cut
		{15334, []float64{12.3, 9.5, 7.9}},
	} {
		if capW := read(t0.Add(time.Duration(i)*time.Second), r.gridW, r.phaseA); capW < 10350 {
			t.Fatalf("reading %d (%.0f W, worst phase %.1f A) cut the cap to %.0f W", i, r.gridW, r.phaseA[0], capW)
		}
	}

	// Without phase currents the total stands in, shared over the phases,
	// under the same heat budget: one reading over cuts nothing, and a
	// sustained one cuts its overage once the budget is spent.
	store, state, points, configs = evMeterBudgetFixture(t, 0, nil, 10350, "3p")
	state.SiteFuseSafetyA = 0.5
	now = t0
	state.clock = func() time.Time { return now }
	read(t0, 15334, nil)
	if capW := read(t0.Add(time.Second), 19174, nil); capW < 10350 {
		t.Fatalf("without phase currents one 19,174 W reading cut the cap to %.1f W", capW)
	}
	for i := 2; i <= 120; i++ {
		if capW := read(t0.Add(time.Duration(i)*time.Second), 19174, nil); capW < 10350 {
			if math.Abs(capW-(10350-(19174-16560))) > .01 || i < 10 {
				t.Fatalf("a sustained 19,174 W cut to %.1f W after %d s, want 10,350 - 2,614 = 7,736 W once the budget is spent", capW, i)
			}
			return
		}
	}
	t.Fatal("a sustained 19,174 W never cut")
}
