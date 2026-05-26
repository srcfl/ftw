package battery

import (
	"encoding/json"
	"math"
	"testing"
)

// ---- RLS convergence ----

func simulateAndFit(trueA, trueB float64, n int, u func(int) float64) *Model {
	m := New("test")
	y := 0.0
	var now int64
	for i := 0; i < n; i++ {
		c := u(i)
		newY := trueA*y + trueB*c
		m.Update(c, newY, 0.5, 5.0, now)
		y = newY
		now += 5000
	}
	return m
}

func TestRLSConvergesOnSyntheticARX(t *testing.T) {
	// Known plant: a=0.6, b=0.4 → gain = 1.0
	m := simulateAndFit(0.6, 0.4, 500, func(i int) float64 {
		switch (i / 30) % 4 {
		case 0:
			return 1000
		case 1:
			return -1000
		case 2:
			return 500
		default:
			return -500
		}
	})
	if math.Abs(m.A-0.6) > 0.1 {
		t.Errorf("a: got %.3f, want ~0.6", m.A)
	}
	if math.Abs(m.B-0.4) > 0.15 {
		t.Errorf("b: got %.3f, want ~0.4", m.B)
	}
	g := m.SteadyStateGain()
	if math.Abs(g-1.0) > 0.15 {
		t.Errorf("gain: got %.3f, want ~1.0", g)
	}
}

func TestRLSRecoversLowerGain(t *testing.T) {
	// Typical efficiency-loss battery: a=0.5, b=0.425 → gain 0.85
	m := simulateAndFit(0.5, 0.425, 500, func(i int) float64 {
		if (i/20)%2 == 0 {
			return 1500
		}
		return -1500
	})
	g := m.SteadyStateGain()
	if math.Abs(g-0.85) > 0.15 {
		t.Errorf("gain: got %.3f, want ~0.85", g)
	}
}

// ---- Gating ----

func TestSkipsLowSignalObservations(t *testing.T) {
	m := New("quiet")
	for i := 0; i < 100; i++ {
		m.Update(20, 18, 0.5, 5, 1000) // commands < MinCommandForRLS
	}
	if m.NSamples != 0 {
		t.Errorf("low-signal should skip RLS; got %d samples", m.NSamples)
	}
}

func TestUpdateRejectsInvalidInputsWithoutMutation(t *testing.T) {
	cases := []struct {
		name    string
		command float64
		actual  float64
		soc     float64
		dtS     float64
	}{
		{"nan_command", math.NaN(), 800, 0.5, 5},
		{"inf_actual", 1000, math.Inf(1), 0.5, 5},
		{"nan_soc", 1000, 800, math.NaN(), 5},
		{"low_soc", 1000, 800, -0.1, 5},
		{"high_soc", 1000, 800, 1.1, 5},
		{"zero_dt", 1000, 800, 0.5, 0},
		{"nan_dt", 1000, 800, 0.5, math.NaN()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New("guard")
			m.Update(1000, 800, 0.5, 5, 1000)
			before, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal before: %v", err)
			}
			if m.Update(tc.command, tc.actual, tc.soc, tc.dtS, 2000) {
				t.Fatal("invalid input should not update model")
			}
			after, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal after: %v", err)
			}
			if string(after) != string(before) {
				t.Fatalf("model mutated on invalid input\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
}

func TestOutlierRejection(t *testing.T) {
	// Warmup with good data
	m := simulateAndFit(0.6, 0.4, 200, func(i int) float64 {
		if i%2 == 0 {
			return 1000
		}
		return -1000
	})
	before := m.NSamples
	// Inject obvious outlier
	m.LastU = 1000
	m.LastY = 600
	m.Update(1000, 10000, 0.5, 5, 9999)
	if m.NSamples != before {
		t.Error("outlier should not bump sample count")
	}
}

// ---- Parameter bounds ----

func TestParamsStayBounded(t *testing.T) {
	m := New("chaos")
	var now int64
	for i := 0; i < 1000; i++ {
		u := float64((i*137)%5000 - 2500)
		y := u*0.9 + float64((i*71)%200-100)
		m.Update(u, y, 0.5, 5, now)
		now += 5000
	}
	if m.A < 0.1 || m.A > 0.99 {
		t.Errorf("a: %.3f", m.A)
	}
	if m.B < -1.5 || m.B > 1.5 {
		t.Errorf("b: %.3f", m.B)
	}
}

// ---- Inverse + clamp ----

func TestInversePassesThroughWhenUnhealthy(t *testing.T) {
	m := New("bad")
	m.A = 0.99
	m.B = 0.001 // raw gain ≈ 0.1 → unhealthy
	cmd := m.Inverse(1000)
	if math.Abs(cmd-1000) > 1 {
		t.Errorf("fallback should be passthrough, got %f", cmd)
	}
}

func TestInverseScalesCommand(t *testing.T) {
	m := New("normal")
	m.A = 0.5
	m.B = 0.4 // gain 0.8
	cmd := m.Inverse(1000)
	// To GET actual=1000, we need cmd = 1000/0.8 = 1250
	if math.Abs(cmd-1250) > 1 {
		t.Errorf("expected 1250, got %f", cmd)
	}
}

func TestClampToSaturationUsesCurve(t *testing.T) {
	m := New("sat")
	// Site convention: charge is +. Curve stores positive magnitudes.
	m.MaxChargeCurve = []SoCPoint{
		{0.0, 5000}, {0.5, 5000}, {0.95, 1000}, {1.0, 0},
	}
	// At SoC=0.95, charging +3000 → clamped to +1000
	v, was := m.ClampToSaturation(3000, 0.95)
	if !was || math.Abs(v-1000) > 1 {
		t.Errorf("expected clamped to 1000, got %f was=%v", v, was)
	}
	// At SoC=0.5, no clamp
	v, was = m.ClampToSaturation(3000, 0.5)
	if was || v != 3000 {
		t.Errorf("should not clamp at SoC 0.5, got %f was=%v", v, was)
	}
}

func TestSaturationCurveIgnoresSmallObservations(t *testing.T) {
	// Regression test: small clamp-induced values must not seed a bucket.
	m := New("locked")
	for i := 0; i < 20; i++ {
		m.Update(-500, -255, 0.5, 5, 1000)
	}
	if len(m.MaxDischargeCurve) > 0 {
		t.Errorf("small observations should not seed bucket, got %+v", m.MaxDischargeCurve)
	}
}

func TestSaturationCurveGrowsFromLargeObservations(t *testing.T) {
	m := New("growing")
	// Feed 2500W (above MIN_SAT_SEED_W) at SoC=0.5
	m.Update(-3000, -2500, 0.5, 5, 0)
	m.Update(-3000, -2500, 0.5, 5, 5000)
	if len(m.MaxDischargeCurve) == 0 {
		t.Error("curve should grow from large observations")
	}
}

// ---- Health ----

func TestHealthScoreNeutralWithoutBaseline(t *testing.T) {
	m := New("nobase")
	if m.HealthScore() != 1.0 {
		t.Errorf("no baseline → 1.0, got %f", m.HealthScore())
	}
}

func TestHealthScoreDegradesWithDrift(t *testing.T) {
	m := New("drift")
	m.A = 0.5
	m.B = 0.5 // gain 1.0
	m.SetBaseline(1.0, 2.0, 1000)
	if math.Abs(m.HealthScore()-1.0) > 0.01 {
		t.Errorf("at baseline, health=1; got %f", m.HealthScore())
	}
	// Drift: b=0.4, a=0.5 → gain 0.8, 20% drift → health = 1-0.4 = 0.6
	m.B = 0.4
	h := m.HealthScore()
	if math.Abs(h-0.6) > 0.05 {
		t.Errorf("20%% drift → health ≈0.6, got %f", h)
	}
}

func TestDriftPerDayDetectsFallingGain(t *testing.T) {
	m := New("falling")
	day := int64(86400_000)
	for i := 0; i < 100; i++ {
		t := int64(i) * day / 10     // 10 days worth of points
		g := 1.0 - float64(i)/1000.0 // 1.0 → 0.9 over 100 points
		m.GainHistory = append(m.GainHistory, GainPoint{TsMs: t, Gain: g})
	}
	drift := m.HealthDriftPerDay()
	// Over 10 days, gain went from 1.0 to 0.9 → slope ≈ -0.01/day
	if drift > -0.005 || drift < -0.02 {
		t.Errorf("expected ≈-0.01/day, got %f", drift)
	}
}

// ---- Serde roundtrip ----

func TestJSONRoundtrip(t *testing.T) {
	m := New("rt")
	m.Update(1000, 900, 0.5, 5, 1000)
	m.Update(1000, 920, 0.5, 5, 6000)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Model
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Name != "rt" {
		t.Errorf("name: %s", back.Name)
	}
	if math.Abs(back.A-m.A) > 1e-9 {
		t.Errorf("a")
	}
	if math.Abs(back.B-m.B) > 1e-9 {
		t.Errorf("b")
	}
	if back.NSamples != m.NSamples {
		t.Errorf("samples")
	}
}

// ---- SetFromStepFit ----

func TestSetFromStepFit(t *testing.T) {
	m := New("fit")
	m.SetFromStepFit(0.92, 1.5, 5)
	g := m.SteadyStateGain()
	tau := m.TimeConstantS(5)
	if math.Abs(g-0.92) > 0.05 {
		t.Errorf("gain: got %.3f", g)
	}
	if math.Abs(tau-1.5) > 0.3 {
		t.Errorf("τ: got %.2f", tau)
	}
}

func TestTimeConstantRelationship(t *testing.T) {
	m := New("τ")
	m.A = 0.6
	tau := m.TimeConstantS(5)
	// -5 / ln(0.6) ≈ 9.79
	if math.Abs(tau-9.79) > 0.1 {
		t.Errorf("τ: got %.2f, want ~9.79", tau)
	}
}

// ---- Baseline + calibration ----

func TestSetBaselineStoresValues(t *testing.T) {
	m := New("cal")
	m.SetBaseline(0.95, 2.0, 1700000)
	if m.BaselineGain == nil || *m.BaselineGain != 0.95 {
		t.Error("baseline gain")
	}
	if m.BaselineTauS == nil || *m.BaselineTauS != 2.0 {
		t.Error("baseline τ")
	}
	if m.LastCalibrated == nil || *m.LastCalibrated != 1700000 {
		t.Error("calibrated ts")
	}
}
