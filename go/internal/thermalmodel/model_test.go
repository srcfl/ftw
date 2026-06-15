package thermalmodel

import (
	"math"
	"testing"
	"time"
)

// simulate one true-physics step of the 1R1C model.
func trueStep(indoorC, outdoorC, heatW, dt, a, b float64) float64 {
	return indoorC + dt*(a*(outdoorC-indoorC)+b*heatW)
}

// TestRLSConvergesToTrueParams drives the model with a synthetic building
// whose (a, b) differ from the prior and checks the estimator recovers
// them — and therefore the thermal time constant τ.
func TestRLSConvergesToTrueParams(t *testing.T) {
	const (
		trueA = 1.0 / 7200.0 // τ = 2h building (draughtier than the 4h prior)
		trueB = 3.0e-7       // stronger heater coupling than prior
		dt    = 900.0        // 15-min steps
	)
	m := NewModel()

	indoor := 20.0
	outdoor := 2.0
	now := time.Now().UnixMilli()
	// Drive ~2000 steps with a varying heating pattern so both features
	// are excited (constant heat would make a/b unidentifiable).
	for k := 0; k < 2000; k++ {
		heat := 0.0
		if k%3 != 0 { // duty-cycle the heater
			heat = 1500.0 + 500.0*math.Sin(float64(k)/5.0)
		}
		next := trueStep(indoor, outdoor, heat, dt, trueA, trueB)
		m.Update(indoor, next, outdoor, heat, dt, now)
		indoor = next
		now += int64(dt) * 1000
		// occasionally nudge outdoor so heat-loss term is well excited
		if k%50 == 0 {
			outdoor = -5.0 + 10.0*math.Sin(float64(k)/30.0)
		}
		// keep indoor in a realistic band by re-centering if it drifts off
		if indoor > 24 {
			indoor = 24
		}
		if indoor < 16 {
			indoor = 16
		}
	}

	gotA, gotB := m.Beta[0], m.Beta[1]
	if rel := math.Abs(gotA-trueA) / trueA; rel > 0.15 {
		t.Errorf("heat-loss coef a: got %.3e want %.3e (rel err %.1f%%)", gotA, trueA, rel*100)
	}
	if rel := math.Abs(gotB-trueB) / trueB; rel > 0.15 {
		t.Errorf("heating gain b: got %.3e want %.3e (rel err %.1f%%)", gotB, trueB, rel*100)
	}
	gotTau := m.TauSeconds()
	wantTau := 1.0 / trueA
	if rel := math.Abs(gotTau-wantTau) / wantTau; rel > 0.15 {
		t.Errorf("tau: got %.0fs want %.0fs", gotTau, wantTau)
	}
}

// TestPredictNextTracksPhysics checks a trained model's one-step forecast
// stays close to the true dynamics.
func TestPredictNextTracksPhysics(t *testing.T) {
	const trueA, trueB, dt = 1.0 / 10800.0, 2.0e-7, 900.0
	m := NewModel()
	indoor, outdoor := 21.0, 5.0
	now := time.Now().UnixMilli()
	for k := 0; k < 1000; k++ {
		heat := 1000.0 * float64(k%4)
		next := trueStep(indoor, outdoor, heat, dt, trueA, trueB)
		m.Update(indoor, next, outdoor, heat, dt, now)
		indoor, now = next, now+int64(dt)*1000
		if indoor > 24 || indoor < 18 {
			indoor = 21
		}
	}
	// Forecast vs. truth at a fresh operating point.
	pred := m.PredictNext(20.0, 0.0, 2000.0, dt)
	truth := trueStep(20.0, 0.0, 2000.0, dt, trueA, trueB)
	if math.Abs(pred-truth) > 0.1 {
		t.Errorf("PredictNext %.3f°C vs truth %.3f°C", pred, truth)
	}
}

// TestHeatToHoldW recovers the steady-state holding power.
func TestHeatToHoldW(t *testing.T) {
	m := NewModel()
	m.Samples = WarmupSamples // trust learned coefs
	m.Beta[0] = 1.0 / 7200.0
	m.Beta[1] = 2.0e-7
	// At hold, a·(Tout−Tin)+b·P = 0 ⇒ P = a·(Tin−Tout)/b
	want := m.Beta[0] * (21.0 - 1.0) / m.Beta[1]
	got := m.HeatToHoldW(21.0, 1.0)
	if math.Abs(got-want) > 1 {
		t.Errorf("HeatToHoldW got %.0fW want %.0fW", got, want)
	}
	// A zone already warmer than target needs no heat.
	if w := m.HeatToHoldW(18.0, 22.0); w != 0 {
		t.Errorf("warm zone should need 0 W, got %.0f", w)
	}
}

// TestUpdateRejectsGlitch ensures an impossible temperature jump is ignored.
func TestUpdateRejectsGlitch(t *testing.T) {
	m := NewModel()
	now := time.Now().UnixMilli()
	// 10°C jump in one minute → ~10°C/min, far above the 2°C/min cap.
	if m.Update(20.0, 30.0, 5.0, 1000.0, 60.0, now) {
		t.Error("expected glitch step to be rejected")
	}
	if m.Samples != 0 {
		t.Errorf("rejected sample must not increment Samples, got %d", m.Samples)
	}
}

// TestPriorIsPhysical sanity-checks the cold-start prior produces a
// reasonable holding power and time constant before any training.
func TestPriorIsPhysical(t *testing.T) {
	m := NewModel()
	if tau := m.TauSeconds(); math.Abs(tau-14400) > 1 {
		t.Errorf("prior tau got %.0fs want 14400s (4h)", tau)
	}
	// Holding 21°C against 0°C outdoor with a 4h τ should be a few hundred W
	// to low kW — sanity bound, not exact.
	w := m.HeatToHoldW(21.0, 0.0)
	if w < 100 || w > 5000 {
		t.Errorf("prior HeatToHoldW %.0fW outside sane [100,5000] band", w)
	}
}

// TestQualityRampsWithSamples verifies the confidence score behaves.
func TestQualityRampsWithSamples(t *testing.T) {
	m := NewModel()
	if q := m.Quality(); q != 0 {
		t.Errorf("fresh model quality should be 0, got %.2f", q)
	}
	m.Samples = 100
	m.MAE = 0.05
	if q := m.Quality(); q < 0.99 {
		t.Errorf("well-trained low-MAE model should score ~1, got %.2f", q)
	}
	m.MAE = 0.5
	if q := m.Quality(); q > 0.01 {
		t.Errorf("high-MAE model should score ~0, got %.2f", q)
	}
}
