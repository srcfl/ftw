package pvmodel

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// TestFeaturesStableAcrossDST — the time-of-day harmonic phase must
// not shift when the same instant is passed in a different timezone.
// Otherwise DST transitions silently re-score the learned β coefficients.
func TestFeaturesStableAcrossDST(t *testing.T) {
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skipf("Europe/Stockholm tzdata unavailable: %v", err)
	}
	instants := []time.Time{
		time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 15, 10, 30, 0, 0, time.UTC),
	}
	for _, inst := range instants {
		fUTC := Features(500, 30, inst)
		fLocal := Features(500, 30, inst.In(stockholm))
		for i := range fUTC {
			if fUTC[i] != fLocal[i] {
				t.Errorf("Features[%d] diverges: utc=%.4f local=%.4f (inst=%v)",
					i, fUTC[i], fLocal[i], inst)
			}
		}
	}
}

// synthetic simulates a real rooftop: rated 10 kW south-facing with a
// morning shading lobe (soft -30% around 08:00, tapering to 1.0 by 11:00),
// a 92% of rated peak (module aging), and a slightly stronger cloud
// response than the naive (1-c)^1.5 heuristic.
func synthetic(clearSkyW, cloudPct float64, t time.Time) float64 {
	hour := float64(t.Hour()) + float64(t.Minute())/60.0
	// Smooth shade lobe centered at 8 with width 3h (cosine window).
	shade := 1.0
	if hour < 11 {
		dx := (hour - 8) / 3.0
		if math.Abs(dx) < 1 {
			shade = 1 - 0.3*(0.5+0.5*math.Cos(math.Pi*dx))
		}
	}
	cf := math.Pow(1-cloudPct/100.0, 1.6)
	y := clearSkyW * cf * (10000 * 0.92 / 1000.0) * shade
	if y < 0 {
		y = 0
	}
	return y
}

// naivePredict replicates the pre-twin forecast so we can show the twin
// is actually an improvement.
func naivePredict(clearSkyW, cloudPct float64, ratedW float64) float64 {
	cf := math.Pow(1-cloudPct/100.0, 1.5)
	return clearSkyW * cf * ratedW / 1000.0
}

// clearSkySim: triangular curve peaking at 1000 W/m² at solar noon, 0
// at 06:00 and 18:00. Good enough for learning tests.
func clearSkySim(t time.Time) float64 {
	hour := float64(t.Hour()) + float64(t.Minute())/60.0
	if hour <= 6 || hour >= 18 {
		return 0
	}
	peak := 12.0
	return 1000.0 * (1 - math.Abs(hour-peak)/6.0)
}

func TestLearnsSyntheticSystem(t *testing.T) {
	m := NewModel(10000)
	r := rand.New(rand.NewSource(42))
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Train 30 days × 24h × 4 samples/hour = 2880 samples
	for d := 0; d < 30; d++ {
		for h := 0; h < 24; h++ {
			for q := 0; q < 4; q++ {
				t0 := start.Add(time.Duration(d*24+h)*time.Hour + time.Duration(q*15)*time.Minute)
				cs := clearSkySim(t0)
				cloud := 20 + r.Float64()*60 // 20–80% cloud
				actual := synthetic(cs, cloud, t0)
				// Add 2% measurement noise
				actual += (r.Float64()*2 - 1) * 0.02 * actual
				m.Update(cs, cloud, t0, actual)
			}
		}
	}
	// Compare twin vs. naive across 24 hourly buckets at 20% cloud. Twin
	// must be at LEAST as accurate as naive on average — ideally better.
	var twinErr, naiveErr float64
	var samples int
	for h := 6; h <= 18; h++ {
		testT := time.Date(2026, 2, 1, h, 0, 0, 0, time.UTC)
		cs := clearSkySim(testT)
		if cs < 50 {
			continue
		}
		want := synthetic(cs, 20, testT)
		twinErr += math.Abs(m.Predict(cs, 20, testT) - want)
		naiveErr += math.Abs(naivePredict(cs, 20, 10000) - want)
		samples++
	}
	twinErr /= float64(samples)
	naiveErr /= float64(samples)
	t.Logf("twin MAE %.0fW, naive MAE %.0fW, improvement %.1f%%", twinErr, naiveErr, (1-twinErr/naiveErr)*100)
	if twinErr >= naiveErr {
		t.Errorf("twin should beat naive on average: twin=%.0fW naive=%.0fW", twinErr, naiveErr)
	}
	// Quality should cross 0.3 for a well-fit 10kW system (MAE ≲ 1.5kW).
	if m.Quality() < 0.3 {
		t.Errorf("quality should be >0.3 after 2880 samples, got %.2f (MAE=%.0fW)", m.Quality(), m.MAE)
	}
}

func TestSkipsNightSamples(t *testing.T) {
	m := NewModel(5000)
	before := m.Samples
	m.Update(10, 50, time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), 0)
	if m.Samples != before {
		t.Errorf("should skip low-clearsky samples")
	}
}

func TestRejectsOutliers(t *testing.T) {
	m := NewModel(5000)
	// Converge with clean data
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		t0 := start.Add(time.Duration(i) * time.Minute)
		m.Update(800, 30, t0, 3000)
	}
	preBeta := m.Beta
	// Inject a 10× outlier
	m.Update(800, 30, start.Add(500*time.Minute), 30000)
	// β shouldn't move meaningfully
	var drift float64
	for i := 0; i < NFeat; i++ {
		drift += math.Abs(m.Beta[i] - preBeta[i])
	}
	if drift > 0.1 {
		t.Errorf("outlier should be rejected, but β drifted %.4f", drift)
	}
}

func TestInitialPredictionMatchesNaive(t *testing.T) {
	// Fresh model should give predictions close to naive clear-sky × cloud × rated/1000.
	m := NewModel(5000)
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cs := 900.0
	cloud := 20.0
	want := cs * math.Pow(0.8, 1.5) * 5000.0 / 1000.0
	got := m.Predict(cs, cloud, t0)
	if math.Abs(got-want) > 1 {
		t.Errorf("initial prediction should match naive: got %f want %f", got, want)
	}
}

func TestQualityStartsLow(t *testing.T) {
	m := NewModel(5000)
	if q := m.Quality(); q != 0 {
		t.Errorf("untrained quality should be 0, got %f", q)
	}
}

func TestCapsAtDoubleRated(t *testing.T) {
	m := NewModel(5000)
	// Force bad β that would predict huge value
	m.Beta[1] = 100
	got := m.Predict(1000, 0, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	if got > 1.3*5000 {
		t.Errorf("should cap at 1.3× rated, got %f", got)
	}
}

// Night-time gate (issue #133). Any clear-sky below the Update-side
// threshold must yield 0 W regardless of Beta drift. Covers the
// operator-reported case where the planner showed −1.1 kW of PV at
// 02:00 because Beta[0] (intercept) had drifted during daytime training.
func TestPredictAtNightReturnsZero(t *testing.T) {
	m := NewModel(10000)
	// Force Beta[0] to a large positive value as if RLS had drifted
	// the intercept during training. Without the gate, Predict at
	// clearSky=0 would return this value.
	m.Beta[0] = 1100
	m.Samples = 100 // past warmup → full trust on learned model

	t02 := time.Date(2026, 4, 20, 2, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		clearSkyW float64
	}{
		{"pitch-dark-midnight", 0},
		{"astronomical-twilight", 10},
		{"civil-twilight-just-below-gate", 49},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := m.Predict(c.clearSkyW, 50, t02)
			if got != 0 {
				t.Errorf("night (clearSky=%.0f, Beta[0]=%.0f) must return 0, got %.2f W",
					c.clearSkyW, m.Beta[0], got)
			}
		})
	}
}

// Intercept is a dead coefficient (issue #134). Any Beta[0] value must
// produce the same daytime prediction, i.e. Beta[0] is fully decoupled
// from output. Complements the night-gate guard in
// TestPredictAtNightReturnsZero (which covers the night-time case).
func TestInterceptDoesNotAffectDaytimePredict(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cs := 900.0
	cloud := 20.0

	m := NewModel(5000)
	m.Samples = 100 // past warmup → full trust on learned model
	baseline := m.Predict(cs, cloud, t0)

	for _, b0 := range []float64{-2000, -500, 0, 500, 2000, 5000} {
		m2 := NewModel(5000)
		m2.Samples = 100
		m2.Beta[0] = b0
		got := m2.Predict(cs, cloud, t0)
		if math.Abs(got-baseline) > 1e-9 {
			t.Errorf("Beta[0]=%.0f changed prediction: %.4f vs baseline %.4f — intercept must be a dead coefficient",
				b0, got, baseline)
		}
	}
}

// Codex P1 on PR #136: with Features[0]=0 (dead slot), the standard
// covariance update divides P[0][0] by λ every tick → ~1.005× growth
// compounds to Inf after ~140k updates → Px[0] = Inf*0 = NaN poisons
// K, β, and predictions. The Update() fix freezes row/column 0 of P.
// This test runs enough Updates to catch any residual growth and asserts
// the whole matrix + β stay finite.
func TestUpdatePKeepsDeadInterceptFinite(t *testing.T) {
	m := NewModel(10000)
	base := time.Date(2026, 6, 1, 6, 0, 0, 0, time.UTC)
	// 2000 Updates — well past the point where the old code would have
	// started accelerating P[0][0] but not long enough to explode even
	// the buggy version, so the assertion has to be tight, not loose.
	for i := 0; i < 2000; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		// Vary clearSky + cloud so the update actually fires (>50 gate)
		// and doesn't get outlier-rejected.
		cs := 400.0 + math.Mod(float64(i), 600)
		cloud := math.Mod(float64(i), 100)
		actual := 0.8 * cs * math.Pow(1-cloud/100, 1.5) * 10.0
		_ = m.Update(cs, cloud, ts, actual)
	}
	// Every P cell finite.
	for i := 0; i < NFeat; i++ {
		for j := 0; j < NFeat; j++ {
			if math.IsNaN(m.P[i][j]) || math.IsInf(m.P[i][j], 0) {
				t.Fatalf("P[%d][%d] = %v — covariance went infinite", i, j, m.P[i][j])
			}
		}
	}
	// Row 0 and column 0 must stay at zero (or at least not wandering):
	// the fix skips their update so whatever they started at (col 0 all
	// 0, row 0 diagonal 1000) persists — but since row 0 and col 0
	// never get written into, the `newP` zero-value clears them to 0.
	for j := 1; j < NFeat; j++ {
		if m.P[0][j] != 0 {
			t.Errorf("P[0][%d] = %v, want 0 (row 0 is dead)", j, m.P[0][j])
		}
		if m.P[j][0] != 0 {
			t.Errorf("P[%d][0] = %v, want 0 (col 0 is dead)", j, m.P[j][0])
		}
	}
	// β stays finite.
	for i := 0; i < NFeat; i++ {
		if math.IsNaN(m.Beta[i]) || math.IsInf(m.Beta[i], 0) {
			t.Fatalf("Beta[%d] = %v — coefficient went non-finite", i, m.Beta[i])
		}
	}
}

// Self-heal: Update zeros Beta[0] at the end of each RLS step so drifted
// persisted models don't keep the stale intercept forever.
func TestUpdateZeroesIntercept(t *testing.T) {
	m := NewModel(10000)
	m.Beta[0] = 1100 // pretend this was persisted from a pre-#134 drift
	if !m.Update(800, 30, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC), 6000) {
		t.Fatal("update should have run (clearSky > gate, actual sane)")
	}
	if m.Beta[0] != 0 {
		t.Errorf("Beta[0] should be zeroed after Update, got %f", m.Beta[0])
	}
}

// Sunrise transition: the moment clearSky crosses the gate threshold the
// model is live again. Anchors the gate boundary so it doesn't silently
// creep upward.
func TestPredictAtGateThresholdBoundary(t *testing.T) {
	m := NewModel(10000)
	// Reasonable sunrise instant; exact time-of-day doesn't matter
	// because we vary clearSky directly.
	tt := time.Date(2026, 4, 20, 5, 0, 0, 0, time.UTC)
	if got := m.Predict(49, 0, tt); got != 0 {
		t.Errorf("clearSky=49 should be gated to 0, got %.2f", got)
	}
	if got := m.Predict(50, 0, tt); got <= 0 {
		t.Errorf("clearSky=50 should be above the gate and produce nonzero, got %.2f", got)
	}
}

// frozenModel zeroes the RLS covariance so K = P·x/(λ + xᵀPx) is 0 and Beta
// never moves. Every Update then predicts the same watt figure, which makes
// the error EMAs hand-checkable across several samples.
func frozenModel(ratedW float64) *Model {
	m := NewModel(ratedW)
	m.P = [NFeat][NFeat]float64{}
	return m
}

// With rated 5 kW and clear sky 200 W/m² at zero cloud, the frozen prior
// predicts exactly 1000 W, so each sample's relative error is the actual
// divided by 1000.
func TestRelMAETracksRelativeErrorOverTheMAEWindow(t *testing.T) {
	m := frozenModel(5000)
	tt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// 1300 W against a 1000 W prediction: ratio 0.3, and the first
	// qualifying sample seeds outright rather than ramping from zero.
	if !m.Update(200, 0, tt, 1300) {
		t.Fatal("first update should have run")
	}
	if math.Abs(m.RelMAE-0.3) > 1e-12 {
		t.Fatalf("seed RelMAE = %v, want 0.3", m.RelMAE)
	}

	// 900 W: ratio 0.1 → 0.99·0.3 + 0.01·0.1.
	if !m.Update(200, 0, tt, 900) {
		t.Fatal("second update should have run")
	}
	if want := 0.298; math.Abs(m.RelMAE-want) > 1e-12 {
		t.Fatalf("RelMAE = %v, want %v", m.RelMAE, want)
	}

	// A perfect sample folds a 0 in at the same weight.
	if !m.Update(200, 0, tt, 1000) {
		t.Fatal("third update should have run")
	}
	if want := 0.29502; math.Abs(m.RelMAE-want) > 1e-12 {
		t.Fatalf("RelMAE = %v, want %v", m.RelMAE, want)
	}
}

// A 4× miss and a 1× miss both mean the forecast was worthless. Without the
// clamp one such sample would hold the planner's hedge above 100 % of
// generation for days.
func TestRelMAEClampsRatioAtOne(t *testing.T) {
	m := frozenModel(5000)
	tt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	if !m.Update(200, 0, tt, 5000) { // 5000 vs 1000 predicted → raw ratio 4
		t.Fatal("update should have run")
	}
	if m.RelMAE != 1 {
		t.Fatalf("RelMAE = %v, want 1 (ratio clamped)", m.RelMAE)
	}
	if !m.Update(200, 0, tt, 5000) {
		t.Fatal("update should have run")
	}
	if m.RelMAE != 1 {
		t.Fatalf("RelMAE = %v, want 1 — the clamp must hold across samples", m.RelMAE)
	}
}

// Below the gate the denominator is small enough that watt-level noise
// produces meaningless ratios, so those samples train MAE but not RelMAE.
func TestRelMAEIgnoresPredictionsBelowTheGate(t *testing.T) {
	m := frozenModel(1000) // Beta[2] = 1 → prediction equals clear sky
	tt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	if !m.Update(400, 0, tt, 800) { // predicted 400 W, under the 500 W gate
		t.Fatal("update should have run")
	}
	if m.MAE == 0 {
		t.Fatal("MAE must still train on sub-gate samples")
	}
	if m.RelMAE != 0 {
		t.Fatalf("RelMAE = %v, want 0: sub-gate ratios must not be folded in", m.RelMAE)
	}

	if !m.Update(600, 0, tt, 900) { // predicted 600 W, ratio 300/600 = 0.5
		t.Fatal("update should have run")
	}
	if math.Abs(m.RelMAE-0.5) > 1e-12 {
		t.Fatalf("RelMAE = %v, want 0.5 from the first qualifying sample", m.RelMAE)
	}
}

// The gate is a threshold, not a range: exactly 500 W qualifies.
func TestRelMAEGateBoundary(t *testing.T) {
	tt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	below := frozenModel(1000)
	if !below.Update(relMAEMinPredictedW-1, 0, tt, 600) {
		t.Fatal("update should have run")
	}
	if below.RelMAE != 0 {
		t.Errorf("prediction just below the gate must not train RelMAE, got %v", below.RelMAE)
	}

	at := frozenModel(1000)
	if !at.Update(relMAEMinPredictedW, 0, tt, 600) {
		t.Fatal("update should have run")
	}
	if at.RelMAE <= 0 {
		t.Errorf("prediction exactly at the gate must train RelMAE, got %v", at.RelMAE)
	}
}

// Night samples never reach the EMA at all — Update returns before it.
func TestRelMAEUnchangedByNightSamples(t *testing.T) {
	m := frozenModel(5000)
	tt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if !m.Update(200, 0, tt, 1300) {
		t.Fatal("daylight update should have run")
	}
	seeded := m.RelMAE
	if m.Update(10, 0, tt, 0) {
		t.Fatal("night sample should have been skipped")
	}
	if m.RelMAE != seeded {
		t.Errorf("RelMAE = %v, want it unchanged at %v", m.RelMAE, seeded)
	}
}
