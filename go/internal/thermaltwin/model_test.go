package thermaltwin

import (
	"math"
	"testing"
)

// house is a zone with known physics, integrated finely so the twin is
// judged against a continuous system rather than its own discretisation.
type house struct {
	a, b, c float64 // 1/h, K/kWh, K/h
	tIn     float64
}

func (h *house) step(tOut, powerKW, hours float64) {
	const sub = 30
	dt := hours / sub
	for i := 0; i < sub; i++ {
		h.tIn += dt * (h.a*(tOut-h.tIn) + h.b*powerKW + h.c)
	}
}

// drive runs the house for n windows with a daily outdoor swing and a heat
// pump that cycles, and feeds every window to the twin.
func drive(tw *Twin, h *house, n int, quantize bool) {
	q := func(v float64) float64 {
		if !quantize {
			return v
		}
		return math.Round(v*10) / 10
	}
	for i := 0; i < n; i++ {
		hour := float64(i) * windowH
		tOut := -2 + 6*math.Sin(2*math.Pi*hour/24)
		power := 0.0
		// On for 2 h, off for 1 h, with a stronger burst every evening:
		// the variation a twin needs to separate inertia from input.
		if int(hour)%3 != 2 {
			power = 1.6
		}
		if int(hour)%24 >= 17 && int(hour)%24 < 19 {
			power = 3.0
		}
		start := q(h.tIn)
		h.step(tOut, power, windowH)
		tw.Observe(Sample{TInStartC: start, TInEndC: q(h.tIn), TOutC: tOut, PowerKW: power, Contiguous: true})
	}
}

func TestTwinLearnsKnownPhysics(t *testing.T) {
	h := &house{a: 1.0 / 40, b: 1.0 / 15, c: 0.05, tIn: 20}
	tw := New()
	drive(tw, h, 14*48, false)

	p := tw.Params()
	if p.TauH == nil || math.Abs(*p.TauH-40)/40 > 0.1 {
		t.Errorf("tau = %v h, want 40 ±10%%", deref(p.TauH))
	}
	if p.CapacityKWhPerK == nil || math.Abs(*p.CapacityKWhPerK-15)/15 > 0.1 {
		t.Errorf("capacity = %v kWh/K, want 15 ±10%%", deref(p.CapacityKWhPerK))
	}
	wantUA := 1000 * (1.0 / 40) / (1.0 / 15)
	if p.UAWPerK == nil || math.Abs(*p.UAWPerK-wantUA)/wantUA > 0.15 {
		t.Errorf("UA = %v W/K, want %.0f ±15%%", deref(p.UAWPerK), wantUA)
	}
	if tw.MAE3hK > 0.05 {
		t.Errorf("3 h error = %.3f K on noise-free data", tw.MAE3hK)
	}
	if got := tw.Status(); got != StatusTrusted {
		t.Errorf("status = %s, want trusted", got)
	}
}

// Real sensors step in 0.1 K. The twin must still predict three hours ahead
// well enough to earn trust.
func TestTwinEarnsTrustThroughSensorQuantisation(t *testing.T) {
	h := &house{a: 1.0 / 40, b: 1.0 / 15, c: 0.05, tIn: 20}
	tw := New()
	drive(tw, h, 14*48, true)
	if tw.MAE3hK > trustMAEK {
		t.Errorf("3 h error = %.3f K, want ≤ %.1f", tw.MAE3hK, trustMAEK)
	}
	if got := tw.Status(); got != StatusTrusted {
		t.Errorf("status = %s, want trusted (params %+v)", got, tw.Params())
	}
}

// A week is the minimum. Good data for a day earns nothing yet.
func TestTwinDoesNotTrustEarly(t *testing.T) {
	h := &house{a: 1.0 / 40, b: 1.0 / 15, c: 0.05, tIn: 20}
	tw := New()
	drive(tw, h, 48, false)
	if got := tw.Status(); got != StatusLearning {
		t.Errorf("status after a day = %s, want learning", got)
	}
}

func TestTwinWithoutHeatInputSaysSo(t *testing.T) {
	tw := New()
	for i := 0; i < 400; i++ {
		tw.Observe(Sample{TInStartC: 20, TInEndC: 19.95, TOutC: 0, Contiguous: true})
	}
	if got := tw.Status(); got != StatusNoHeatInput {
		t.Errorf("status = %s, want no_heat_input", got)
	}
	if p := tw.Params(); p.CapacityKWhPerK != nil || p.UAWPerK != nil {
		t.Errorf("params = %+v; capacity and UA need heat input", p)
	}
}

// A house that changes in a way the model can follow (new insulation, a
// colder season) is relearned and stays trusted: trust is about prediction,
// not about the parameters staying put.
func TestTwinFollowsAChangedHouse(t *testing.T) {
	h := &house{a: 1.0 / 40, b: 1.0 / 15, c: 0.05, tIn: 20}
	tw := New()
	drive(tw, h, 14*48, false)
	h.a = 2.0 / 40
	// A week: the forgetting half-life, chosen to follow seasons, not
	// to chase a single cold night.
	drive(tw, h, 7*48, false)
	if got := tw.Status(); got != StatusTrusted || tw.MAE3hK > trustMAEK {
		t.Errorf("status = %s with 3 h error %.2f K, want trusted within %.1f", got, tw.MAE3hK, trustMAEK)
	}
	// The inertia itself follows more slowly than the prediction recovers:
	// for a while the unexplained gain c absorbs the difference. That is
	// enough for three hours ahead and not for how long a house can coast,
	// which is why a planner must not use τ from a twin whose house just
	// changed. Measured here so the lag is a known number, not a surprise.
	p := tw.Params()
	if p.TauH == nil || *p.TauH >= 40 {
		t.Fatalf("tau = %v h after a week, want moving from 40 toward 20", deref(p.TauH))
	}
	drive(tw, h, 14*48, false)
	if p := tw.Params(); p.TauH == nil || math.Abs(*p.TauH-20)/20 > 0.2 {
		t.Errorf("tau = %v h after three weeks, want 20 ±20%%", deref(p.TauH))
	}
}

// A trusted twin that stops predicting, here a sensor moved next to a heat
// source that comes and goes with no pattern, must say drifting the same
// day, not quietly keep its trust.
func TestTwinReportsDrift(t *testing.T) {
	h := &house{a: 1.0 / 40, b: 1.0 / 15, c: 0.05, tIn: 20}
	tw := New()
	drive(tw, h, 14*48, false)
	if tw.Status() != StatusTrusted {
		t.Fatalf("precondition: twin not trusted")
	}
	seed := uint32(1)
	gain := func() float64 {
		seed = seed*1664525 + 1013904223
		return 2.5 * (float64(seed>>8)/float64(1<<24) - 0.5) // ±1.25 K/h
	}
	sawDrift := false
	for i := 0; i < 48 && !sawDrift; i++ {
		h.c = 0.05 + gain()
		drive(tw, h, 1, false)
		sawDrift = tw.Status() == StatusDrifting
	}
	if !sawDrift {
		t.Errorf("status stayed %s with 3 h error %.2f K", tw.Status(), tw.MAE3hK)
	}
}

func TestTwinRejectsImplausibleWindows(t *testing.T) {
	cases := map[string]Sample{
		"NaN temperature": {TInStartC: math.NaN(), TInEndC: 20, TOutC: 0},
		"jump of 5 K":     {TInStartC: 20, TInEndC: 25, TOutC: 0},
		"freezing sensor": {TInStartC: -30, TInEndC: -30, TOutC: 0},
		"negative power":  {TInStartC: 20, TInEndC: 20, TOutC: 0, PowerKW: -1},
		"absurd outdoor":  {TInStartC: 20, TInEndC: 20, TOutC: 80},
		"window too long": {TInStartC: 20, TInEndC: 20, TOutC: 0, DurationH: 3},
		"infinite power":  {TInStartC: 20, TInEndC: 20, TOutC: 0, PowerKW: math.Inf(1)},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			tw := New()
			before := tw.Theta
			if tw.Observe(s) {
				t.Fatal("accepted")
			}
			if tw.Theta != before || tw.Samples != 0 || tw.Rejected != 1 {
				t.Errorf("rejected window changed the twin: %+v", tw)
			}
		})
	}
}

// A gap breaks the three-hour stretches that span it. Judging across one
// would score the twin on a simulation fed inputs that never arrived.
func TestTwinGapRestartsEvaluation(t *testing.T) {
	tw := New()
	for i := 0; i < EvalHorizonWindows-1; i++ {
		tw.Observe(Sample{TInStartC: 20, TInEndC: 20, TOutC: 0, PowerKW: 1, Contiguous: true})
	}
	tw.Observe(Sample{TInStartC: 20, TInEndC: 20, TOutC: 0, PowerKW: 1, Contiguous: false})
	if tw.Evaluations != 0 {
		t.Errorf("evaluations = %d across a gap, want 0", tw.Evaluations)
	}
	if len(tw.Pending) != 1 {
		t.Errorf("pending = %d after a gap, want 1", len(tw.Pending))
	}
}

func TestFeatureHashIsStable(t *testing.T) {
	if a, b := FeatureHash(), FeatureHash(); a != b || len(a) != 16 {
		t.Errorf("hash = %q, %q", a, b)
	}
}

func deref(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
