package loadmodel

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// TestHourOfWeekStableAcrossDST — the bucket index must not shift when
// the same absolute instant is represented in a different timezone.
// Before the UTC coercion, evening-hour Predict calls around DST
// boundaries silently drew from the wrong bucket.
func TestHourOfWeekStableAcrossDST(t *testing.T) {
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skipf("Europe/Stockholm tzdata unavailable: %v", err)
	}
	instants := []time.Time{
		time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 15, 17, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 15, 20, 0, 0, 0, time.UTC),
	}
	for _, inst := range instants {
		if HourOfWeek(inst) != HourOfWeek(inst.In(stockholm)) {
			t.Errorf("HourOfWeek differs: utc=%d local=%d (inst=%v)",
				HourOfWeek(inst), HourOfWeek(inst.In(stockholm)), inst)
		}
	}
}

// synthetic household: 300W baseline, morning peak 2500W around 07:30,
// evening peak 3500W around 19:00.
func synthetic(t time.Time) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60.0
	base := 300.0
	morning := 2500.0 * math.Exp(-0.5*math.Pow((h-7.5)/1.0, 2))
	midday := 800.0 * math.Exp(-0.5*math.Pow((h-13)/2.5, 2))
	evening := 3500.0 * math.Exp(-0.5*math.Pow((h-19)/1.2, 2))
	return base + morning + midday + evening
}

func TestDayOnePriorIsUsefulEverywhere(t *testing.T) {
	// Before any training: predictions at any hour should be within
	// reasonable bounds (>0 overnight, elevated at peaks). The typical
	// prior is the safety net that covers cold start.
	m := NewModel(4000)
	overnight := time.Date(2026, 3, 17, 3, 0, 0, 0, time.UTC)
	morning := time.Date(2026, 3, 17, 7, 30, 0, 0, time.UTC)
	evening := time.Date(2026, 3, 17, 19, 0, 0, 0, time.UTC)
	o := m.PredictNoTemp(overnight)
	mo := m.PredictNoTemp(morning)
	e := m.PredictNoTemp(evening)
	if o < 100 || o > 800 {
		t.Errorf("overnight should be in [100, 800], got %f", o)
	}
	if mo < 1500 {
		t.Errorf("morning peak should be >= 1500, got %f", mo)
	}
	if e < 2000 {
		t.Errorf("evening peak should be >= 2000, got %f", e)
	}
}

func TestLearnsHouseholdPattern(t *testing.T) {
	m := NewModel(4000)
	rng := rand.New(rand.NewSource(42))
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // Monday
	// 10 weeks × 7 days × 24 hours = 1680 hourly samples, ~10 per bucket —
	// past MinTrustSamples, bucket EMA dominates the prior.
	for d := 0; d < 70; d++ {
		for h := 0; h < 24; h++ {
			t0 := start.Add(time.Duration(d*24+h) * time.Hour)
			actual := synthetic(t0) + (rng.Float64()*2-1)*50 // tiny noise
			m.Update(t0, actual, HeatingReferenceC)          // no heating
		}
	}
	// Check weekday prediction accuracy.
	test := time.Date(2026, 3, 2, 19, 0, 0, 0, time.UTC) // Monday 19:00
	want := synthetic(test)
	got := m.Predict(test, HeatingReferenceC)
	if math.Abs(got-want)/want > 0.10 {
		t.Errorf("evening prediction off: got %.0f want %.0f", got, want)
	}
	if m.Quality() < 0.5 {
		t.Errorf("quality should be ≥0.5 after 4 weeks, got %.3f", m.Quality())
	}
}

func TestHeatingConfiguredBoostsColdDayPrediction(t *testing.T) {
	// When operator sets HeatingW_per_degC, Predict adds heating on cold
	// days. Warm days (≥ reference) are unaffected.
	m := NewModel(4000)
	m.HeatingW_per_degC = 300
	t0 := time.Date(2026, 3, 17, 12, 0, 0, 0, time.UTC)
	warm := m.Predict(t0, 20) // above reference
	freezing := m.Predict(t0, -5)
	delta := freezing - warm
	// Expected heating contribution: 300 W/°C × (18 − (−5)) = 6900 W.
	if math.Abs(delta-6900) > 100 {
		t.Errorf("heating contribution: got %.0f W, want ~6900 W", delta)
	}
}

func TestHourOfWeekDeterministic(t *testing.T) {
	// Monday 00:00 UTC → 0
	mon := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	if idx := HourOfWeek(mon); idx != 0 {
		t.Errorf("Monday 00:00 should be bucket 0, got %d", idx)
	}
	// Sunday 23:00 UTC → 167
	sun := time.Date(2026, 1, 11, 23, 0, 0, 0, time.UTC)
	if idx := HourOfWeek(sun); idx != 167 {
		t.Errorf("Sunday 23:00 should be bucket 167, got %d", idx)
	}
}

func TestRejectsNegativeLoad(t *testing.T) {
	m := NewModel(4000)
	before := m.Samples
	m.Update(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC), -500, HeatingReferenceC)
	if m.Samples != before {
		t.Errorf("negative load should be rejected")
	}
}

func TestRejectsOutliers(t *testing.T) {
	m := NewModel(4000)
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		m.Update(start.Add(time.Duration(i)*time.Minute), 1500, HeatingReferenceC)
	}
	preMean := m.Bucket[HourOfWeek(start)].Mean
	m.Update(start.Add(500*time.Minute), 50000, HeatingReferenceC) // 33× typical
	postMean := m.Bucket[HourOfWeek(start.Add(500*time.Minute))].Mean
	if math.Abs(postMean-preMean) > 500 {
		t.Errorf("outlier should be rejected, mean drift %.0f", postMean-preMean)
	}
}

func TestPredictRespectsTrust(t *testing.T) {
	// A bucket with 0 samples returns pure prior.
	// After many samples, it returns the bucket's EMA.
	m := NewModel(4000)
	t0 := time.Date(2026, 1, 5, 19, 0, 0, 0, time.UTC)
	prior := typicalPrior(HourOfWeek(t0))
	predBefore := m.Predict(t0, HeatingReferenceC)
	if math.Abs(predBefore-prior) > 1 {
		t.Errorf("fresh bucket should return prior (%f), got %f", prior, predBefore)
	}
	// Feed 30 samples of a constant 1000W at this hour.
	for i := 0; i < 30; i++ {
		m.Update(t0.AddDate(0, 0, 7*i), 1000, HeatingReferenceC)
	}
	predAfter := m.Predict(t0, HeatingReferenceC)
	if math.Abs(predAfter-1000) > 100 {
		t.Errorf("trained bucket should be ~1000W, got %f", predAfter)
	}
}

// TestNightBucketNotPoisonedByHeatingSubtraction is a regression test for
// the bug reported in T32: night-hour buckets trained during cold Swedish
// winters ended up near 0 W because the code clamped (actualLoad − heatEst)
// to 0 when the heating estimate exceeded the measured load. The EMA then
// decayed toward 0, so summer-night predictions (when heating=0) returned
// ~0 W instead of the real ~300–600 W standby baseline.
//
// Expected behaviour after the fix:
//   - The bucket update is skipped when heatEst >= actualLoad; the bucket
//     mean (and thus the prediction) must remain ≥ the prior-based minimum.
//   - A warm-weather prediction for the same hour must reflect the actual
//     learned baseline, not a ghost 0.
func TestNightBucketNotPoisonedByHeatingSubtraction(t *testing.T) {
	m := NewModel(5520)
	m.HeatingW_per_degC = 300 // as configured on the production instance

	// Monday 03:00 UTC (typical overnight low-load hour).
	t0 := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)

	// Feed a full Swedish winter: 300 samples at 350 W actual load,
	// outdoor temp = 2°C → heatEst = 300*(18−2) = 4800 W >> actual load.
	// Before the fix: baseSample would be clamped to 0, poisoning bucket.Mean.
	// After the fix: the bucket update is skipped entirely.
	coldTemp := 2.0
	for i := 0; i < 300; i++ {
		m.Update(t0.Add(time.Duration(i)*24*time.Hour), 350, coldTemp)
	}

	// In summer the heating term is zero; prediction must reflect real standby.
	// With the old code bucket.Mean ≈ 0 (300 × 0.9^n decay) → prediction ≈ 0.
	// With the fix bucket.Mean was never updated by cold samples → stays at prior.
	warmPred := m.Predict(t0, 22.0) // outdoor temp 22°C — no heating contribution
	priorW := typicalPrior(HourOfWeek(t0))
	if warmPred < priorW*poisonFloor {
		t.Errorf("night bucket poisoned: warm prediction = %.0f W, prior = %.0f W (floor = %.0f W)",
			warmPred, priorW, priorW*poisonFloor)
	}

	// Feed 30 warm-weather samples at the real baseline (350 W, temp 20°C).
	// The model should now learn the actual overnight load.
	for i := 0; i < 30; i++ {
		m.Update(t0.Add(time.Duration(300+i*7)*24*time.Hour), 350, 20.0)
	}
	trainedPred := m.Predict(t0, 20.0)
	if math.Abs(trainedPred-350) > 100 {
		t.Errorf("trained overnight bucket should be ~350 W, got %.0f W", trainedPred)
	}
}

// TestRepairPoisonedBuckets verifies that repairPoisonedBuckets resets bucket
// means that are clearly below the prior floor while leaving healthy buckets
// untouched.
func TestRepairPoisonedBuckets(t *testing.T) {
	m := NewModel(5520)
	m.HeatingW_per_degC = 300

	// Artificially poison night bucket (3:00 UTC Monday) the old way:
	// drain it to ~0 with many zero-valued EMA updates.
	nightIdx := HourOfWeek(time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC))
	m.Bucket[nightIdx].Mean = 5.0
	m.Bucket[nightIdx].Samples = 260

	// Set a healthy evening bucket (19:00 UTC Monday) to its proper value.
	eveningIdx := HourOfWeek(time.Date(2026, 1, 5, 19, 0, 0, 0, time.UTC))
	m.Bucket[eveningIdx].Mean = 2200
	m.Bucket[eveningIdx].Samples = 260

	m.repairPoisonedBuckets()

	nightPrior := typicalPrior(nightIdx)
	if m.Bucket[nightIdx].Mean < nightPrior*poisonFloor {
		t.Errorf("poisoned bucket not repaired: got %.0f W, want >= %.0f W",
			m.Bucket[nightIdx].Mean, nightPrior*poisonFloor)
	}
	if m.Bucket[nightIdx].Samples != 0 {
		t.Errorf("repaired bucket samples should be reset to 0, got %d", m.Bucket[nightIdx].Samples)
	}

	// Evening bucket must be preserved — 2200 W is above floor.
	if m.Bucket[eveningIdx].Mean != 2200 {
		t.Errorf("healthy bucket should be untouched: got %.0f W, want 2200 W", m.Bucket[eveningIdx].Mean)
	}
}

// TestHeatingCoefLearnsFromMeasurements — a household whose load grows with
// the heating-degrees signal should converge to roughly the true sensitivity
// from measurements alone. The old behaviour was operator-only: coef stayed
// at whatever the human typed in (or 0 if untyped). With online adaptation,
// the model uses what it observes — including across mixed warm/cold days
// where the warm days anchor the bucket baseline.
func TestHeatingCoefLearnsFromMeasurements(t *testing.T) {
	const trueBase = 800.0
	const trueCoef = 250.0 // W per °C below 18°C
	m := NewModel(8000)
	rng := rand.New(rand.NewSource(7))
	t0 := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	// 80 weeks of hourly samples (~13.5k) — every bucket gets ~80 samples,
	// alternating warm (no heating signal) and cold (heating signal) days.
	for i := 0; i < 80*7*24; i++ {
		ts := t0.Add(time.Duration(i) * time.Hour)
		// Day index alternates warm vs cold so each bucket trains under
		// both regimes; bucket Mean anchors on warm samples, heating
		// coefficient learns from the cold-day excess.
		day := i / 24
		var tempC float64
		if day%2 == 0 {
			tempC = 19.0 // warm — no heating contribution
		} else {
			tempC = 6.0 // cold — deltaT = 12 → +3000 W heating contribution
		}
		deltaT := math.Max(0, HeatingReferenceC-tempC)
		actual := trueBase + trueCoef*deltaT + (rng.Float64()*2-1)*40
		m.Update(ts, actual, tempC)
	}
	if math.Abs(m.HeatingW_per_degC-trueCoef) > 80 {
		t.Errorf("heating coef should learn ~%.0f W/°C, got %.0f W/°C",
			trueCoef, m.HeatingW_per_degC)
	}
}

// TestHeatingCoefLearnsDownForUnheatedHome — operator may have configured a
// non-zero heating coefficient, but if the actual load shows no temperature
// dependence (district heating, all-electric with no thermostat coupling, or
// even an over-shaded house dominated by solar gain in shoulder season),
// the learned coefficient must adapt downward toward 0. Without this, a
// stale operator value silently inflates forecast load on every cold-day
// slot — which is exactly the failure mode that bit site .40 in May 2026
// (300 W/°C × 6.6°C cold → 1980 W of phantom load while the sun was out).
func TestHeatingCoefLearnsDownForUnheatedHome(t *testing.T) {
	const trueBase = 800.0
	m := NewModel(8000)
	m.HeatingW_per_degC = 500 // operator-set; actual home is unheated
	rng := rand.New(rand.NewSource(11))
	t0 := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 80*7*24; i++ {
		ts := t0.Add(time.Duration(i) * time.Hour)
		day := i / 24
		var tempC float64
		if day%2 == 0 {
			tempC = 19.0
		} else {
			tempC = 6.0
		}
		// Actual load is temperature-independent — pure noise around base.
		actual := trueBase + (rng.Float64()*2-1)*40
		m.Update(ts, actual, tempC)
	}
	if m.HeatingW_per_degC > 80 {
		t.Errorf("unheated home: heating coef should drift toward 0, got %.0f W/°C",
			m.HeatingW_per_degC)
	}
}

// TestHeatingFitWaitsForBucketTrust — the heating regression piggybacks on
// the bucket-baseline estimate; if buckets haven't accumulated enough
// samples, the residual `(actual − bucket.Mean)` is dominated by prior
// error, not by the heating term. Fitting heating off untrustworthy buckets
// produces wild swings — gate the fit on bucket trust the same way Predict
// gates the bucket EMA blend.
func TestHeatingFitWaitsForBucketTrust(t *testing.T) {
	m := NewModel(8000)
	// Feed exactly one cold sample at a single bucket. Bucket samples count
	// will be 1 — far below MinTrustSamples. The heating coef must not
	// jump from this single under-trusted observation.
	t0 := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	m.Update(t0, 5000, 5.0) // arbitrary big load, cold day
	if m.HeatingW_per_degC != 0 {
		t.Errorf("untrusted bucket must not drive heating fit, coef = %.0f", m.HeatingW_per_degC)
	}
}
