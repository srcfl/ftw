package pvmodel

import (
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// TestResetPersistsSurvivesRestart verifies that calling Reset() writes the
// clean model to SQLite so that a fresh NewService (simulating a restart)
// loads the reset state, not the old trained state.
func TestResetPersistsSurvivesRestart(t *testing.T) {
	db := openTestDB(t)

	ratedW := 5000.0
	cs := func(time.Time) float64 { return 500 }
	cl := func(time.Time) (float64, bool) { return 20, true }

	// --- Phase 1: seed a "trained" model and persist it ---
	svc := NewService(db, nil, cs, cl, ratedW)
	// Mutate the model to simulate training.
	svc.mu.Lock()
	svc.model.Samples = 200
	svc.model.MAE = 42
	svc.model.Beta[0] = 999 // clearly non-default
	svc.mu.Unlock()
	svc.persist()

	// Verify the trained state is in the DB.
	js, ok := db.LoadConfig(stateKey)
	if !ok || js == "" {
		t.Fatal("trained model not persisted")
	}
	var trained Model
	if err := json.Unmarshal([]byte(js), &trained); err != nil {
		t.Fatal(err)
	}
	if trained.Samples != 200 {
		t.Fatalf("expected 200 samples in stored model, got %d", trained.Samples)
	}

	// --- Phase 2: reset ---
	svc.Reset()

	// Verify the reset state is now in the DB (samples=0, fresh beta).
	js2, ok := db.LoadConfig(stateKey)
	if !ok || js2 == "" {
		t.Fatal("reset model not persisted")
	}
	var reset Model
	if err := json.Unmarshal([]byte(js2), &reset); err != nil {
		t.Fatal(err)
	}
	if reset.Samples != 0 {
		t.Fatalf("expected 0 samples after reset, got %d", reset.Samples)
	}
	if reset.Beta[0] != 0 {
		t.Fatalf("expected Beta[0]=0 after reset, got %f", reset.Beta[0])
	}

	// --- Phase 3: simulate restart ---
	svc2 := NewService(db, nil, cs, cl, ratedW)
	m := svc2.Model()
	if m.Samples != 0 {
		t.Fatalf("after restart: expected 0 samples, got %d", m.Samples)
	}
	if m.Beta[0] != 0 {
		t.Fatalf("after restart: expected Beta[0]=0, got %f", m.Beta[0])
	}
	// Cold-start beta[2] should be ratedW/1000.
	expected := ratedW / 1000
	if m.Beta[2] != expected {
		t.Fatalf("after restart: expected Beta[2]=%f, got %f", expected, m.Beta[2])
	}
}

func openTestDB(t *testing.T) *state.Store {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// ---- applyNowAnchor (pure) ----

// Xorath's motivating scenario: basePred 1064 W, reality 8380 W. Correction
// at dt=0 should pull the prediction all the way up to (capped at 5×).
func TestApplyNowAnchor_CorrectsUpwardAtT0(t *testing.T) {
	got := applyNowAnchor(1064, 1064, 8380, 0)
	want := 1064 * NowAnchorClamp // 7.87× clamped to 5.0
	if math.Abs(got-want) > 0.5 {
		t.Errorf("got %f, want %f (cap at %.1f×)", got, want, NowAnchorClamp)
	}
}

// Symmetric downward correction: reality is 1/10 of model's belief.
// Should clamp to 1/clamp, not go to zero.
func TestApplyNowAnchor_CorrectsDownwardAtT0(t *testing.T) {
	got := applyNowAnchor(5000, 5000, 500, 0)
	want := 5000 * (1.0 / NowAnchorClamp) // 0.2×
	if math.Abs(got-want) > 0.5 {
		t.Errorf("got %f, want %f (floor at 1/%.1f)", got, want, NowAnchorClamp)
	}
}

// At the horizon edge the correction fades to zero — result equals the
// raw base prediction.
func TestApplyNowAnchor_DecayAtHorizonReturnsBase(t *testing.T) {
	got := applyNowAnchor(2000, 1000, 8000, NowAnchorHorizon)
	if math.Abs(got-2000) > 0.01 {
		t.Errorf("at horizon decay=0, want basePred=2000, got %f", got)
	}
}

// Mid-horizon: decay = 0.5. correction = 8000/1000 = 8× clamped to 5×.
// anchored = base × (1 + (5-1)×0.5) = base × 3.
func TestApplyNowAnchor_DecayAtHalfHorizon(t *testing.T) {
	got := applyNowAnchor(1000, 1000, 8000, NowAnchorHorizon/2)
	want := 1000 * 3.0
	if math.Abs(got-want) > 0.5 {
		t.Errorf("mid-horizon got %f, want %f", got, want)
	}
}

// Beyond the horizon (future > 2h) we don't trust the correction.
func TestApplyNowAnchor_BeyondHorizonNoCorrection(t *testing.T) {
	got := applyNowAnchor(1500, 1000, 8000, 3*time.Hour)
	if math.Abs(got-1500) > 0.01 {
		t.Errorf("beyond horizon, want basePred=1500, got %f", got)
	}
}

// dt < 0 is treated as dt=0 (full correction). This handles the natural
// microsecond drift between the caller's time.Now() and the one read
// inside Predict, plus the case where a slot has just started.
// Historical slots (hours ago) would also get correction here — they're
// an expected non-use-case because MPC doesn't re-predict the past.
func TestApplyNowAnchor_NegativeDtClampsToNow(t *testing.T) {
	got := applyNowAnchor(1000, 1000, 4000, -1*time.Second)
	want := 1000 * 4.0 // correction=4 at decay=1
	if math.Abs(got-want) > 0.5 {
		t.Errorf("slight negative dt should act like now (full correction), got %f want %f", got, want)
	}
}

// Night / driver outage: priorNow or actualNow below 50 W → skip.
// Below the threshold the ratio is meaningless; fall back to base.
func TestApplyNowAnchor_NightSkipsCorrection(t *testing.T) {
	if got := applyNowAnchor(0, 10, 10, 0); got != 0 {
		t.Errorf("night prior: want 0, got %f", got)
	}
	if got := applyNowAnchor(500, 600, 20, 0); math.Abs(got-500) > 0.01 {
		t.Errorf("low actual: want 500, got %f", got)
	}
	if got := applyNowAnchor(500, 20, 600, 0); math.Abs(got-500) > 0.01 {
		t.Errorf("low prior: want 500, got %f", got)
	}
}

// Negative basePred (shouldn't happen, but RLS can misbehave) is
// floored at 0 before the multiplier so we never emit a negative
// "prediction".
func TestApplyNowAnchor_NegativeBaseFlooredToZero(t *testing.T) {
	got := applyNowAnchor(-500, 1000, 8000, 0)
	if got != 0 {
		t.Errorf("negative base should floor at 0, got %f", got)
	}
}

// ---- Service.Predict integration with telemetry ----

// Plug a telemetry Store with live PV and verify that Service.Predict
// folds in the live-vs-model correction end-to-end. The predicted
// timestamp must be within NowAnchorHorizon of the real wall clock,
// or the correction is deliberately skipped — use t=now (dt≈0) so the
// decay factor is ~1.
func TestService_PredictAnchorsOnLiveTelemetry(t *testing.T) {
	tel := telemetry.NewStore()
	// Site convention: PV is negative. 8000 W = -8000 stored.
	tel.Update("pv", telemetry.DerPV, -8000, nil, nil)

	svc := &Service{
		Tele:     tel,
		ClearSky: func(time.Time) float64 { return 800 },
		Cloud:    func(time.Time) (float64, bool) { return 80, true }, // forecast says 80% cloudy
		model:    NewModel(10000),
	}

	// Without the anchor, Predict(now) would use the untrained model's prior
	// = rated × (cs/1000) × (1-0.8)^1.5 ≈ 10000 × 0.8 × 0.0894 ≈ 715 W.
	// With live anchor: actual 8000, prior ~715, correction = 8000/715 ≈ 11
	// → clamped to 5×. At dt≈0, decay=1. Result ≈ 715 × 5 = 3575 W.
	got := svc.Predict(time.Now(), 80)
	if got < 3000 || got > 4000 {
		t.Errorf("with live 8kW vs forecast 80%% cloud, anchored Predict should land ~3.5 kW, got %.0f W", got)
	}
}

// No live telemetry → Predict falls back to the raw model/prior.
// Regression guard: never crash / never emit nonsense when Tele is nil
// or has no PV readings.
func TestService_PredictFallsBackWhenNoTelemetry(t *testing.T) {
	svc := &Service{
		Tele:     nil,
		ClearSky: func(time.Time) float64 { return 500 },
		Cloud:    func(time.Time) (float64, bool) { return 50, true },
		model:    NewModel(5000),
	}
	got := svc.Predict(time.Now(), 50)
	// Raw prior at cs=500, rated=5000, cloud=50: 5000 × 0.5 × (0.5)^1.5 ≈ 884 W
	if got < 500 || got > 1500 {
		t.Errorf("fallback should land near the raw prior (~900 W), got %.0f", got)
	}
}

// ---- PredictStructural (unanchored) ----

// TestPredict_AppliesNowAnchor — same scenario as
// TestService_PredictAnchorsOnLiveTelemetry but framed as the explicit
// pre-condition for PredictStructural's contract: with live telemetry
// disagreeing with the model, Predict IS pulled toward the measurement.
func TestPredict_AppliesNowAnchor(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("pv", telemetry.DerPV, -8000, nil, nil)

	svc := &Service{
		Tele:     tel,
		ClearSky: func(time.Time) float64 { return 800 },
		Cloud:    func(time.Time) (float64, bool) { return 80, true },
		model:    NewModel(10000),
	}

	rawPrior := svc.model.Predict(800, 80, time.Now())
	anchored := svc.Predict(time.Now(), 80)
	// The anchored prediction must be pulled UP toward 8000 W relative
	// to the raw prior (~715 W) — the live PV is dramatically larger
	// than what the forecast cloud cover suggests.
	if anchored <= rawPrior*1.5 {
		t.Errorf("Predict (anchored) should be pulled up by live 8 kW; got %.0f W (raw prior %.0f W)", anchored, rawPrior)
	}
}

// TestPredictStructural_DoesNotApplyNowAnchor: under the same live-
// disagreement scenario, PredictStructural returns the raw RLS output
// untouched. This is the property that prevents double-correction when
// the residual buffer is also wired.
func TestPredictStructural_DoesNotApplyNowAnchor(t *testing.T) {
	tel := telemetry.NewStore()
	// 8 kW live, but model predicts ~715 W from the prior.
	tel.Update("pv", telemetry.DerPV, -8000, nil, nil)

	svc := &Service{
		Tele:     tel,
		ClearSky: func(time.Time) float64 { return 800 },
		Cloud:    func(time.Time) (float64, bool) { return 80, true },
		model:    NewModel(10000),
	}

	now := time.Now()
	rawPrior := svc.model.Predict(800, 80, now)
	structural := svc.PredictStructural(now, 80)
	if math.Abs(structural-rawPrior) > 1e-6 {
		t.Errorf("PredictStructural = %.6f W, want %.6f W (no anchor; live telemetry must be ignored)", structural, rawPrior)
	}

	// Sanity: the anchored Predict path DOES respond — confirms the
	// telemetry plumbing is wired so the "doesn't respond" assertion
	// above is meaningful, not a no-op.
	anchored := svc.Predict(now, 80)
	if anchored <= structural*1.5 {
		t.Fatalf("test setup broken: anchored Predict (%.0f W) should differ from structural (%.0f W); live telemetry not reaching applyNowAnchor", anchored, structural)
	}
}

// TestPredictStructural_StillRespectsRLS: the unanchored variant is not
// a frozen baseline — it tracks the RLS coefficients. Feed enough
// training samples to shift Beta and verify the structural prediction
// follows.
func TestPredictStructural_StillRespectsRLS(t *testing.T) {
	svc := &Service{
		ClearSky: func(time.Time) float64 { return 800 },
		Cloud:    func(time.Time) (float64, bool) { return 20, true },
		model:    NewModel(5000),
	}
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	before := svc.PredictStructural(now, 20)

	// Drive ~60 RLS updates against a synthetic "actual" that is well
	// above what the cold-start prior would predict. RLS should track
	// the new operating point.
	target := before * 1.6
	for i := 0; i < 60; i++ {
		svc.mu.Lock()
		svc.model.Update(800, 20, now, target)
		svc.mu.Unlock()
	}
	after := svc.PredictStructural(now, 20)
	if after <= before*1.1 {
		t.Errorf("PredictStructural did not track RLS update: before=%.0f W, after=%.0f W (target was %.0f W)", before, after, target)
	}
}

// TestResidualBufferSampler_UsesStructuralPrediction: when sample()
// records into the residual buffer, the "predicted" component must be
// the structural prediction (no now-anchor), not the anchored Predict.
// Otherwise the buffer captures (anchored, actual) and the residual
// double-counts the same correction the anchor already applied —
// exactly the bug we're fixing.
//
// We set up a Service where Predict would anchor heavily upward (live
// 8 kW vs raw prior ~715 W) and then drive a single sample(). The
// recorded `predicted` should match the raw model prior, not the
// anchored 3.5 kW.
func TestResidualBufferSampler_UsesStructuralPrediction(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("pv", telemetry.DerPV, -8000, nil, nil)

	svc := &Service{
		Tele:         tel,
		ClearSky:     func(time.Time) float64 { return 800 },
		Cloud:        func(time.Time) (float64, bool) { return 80, true },
		model:        NewModel(10000),
		Residuals:    NewResidualBuffer(),
		PersistEvery: 10, // avoid mod-by-zero in sample(); no Store means persist() no-ops
	}

	// Verify the test setup: anchored vs structural disagree.
	now := time.Now()
	structural := svc.PredictStructural(now, 80)
	anchored := svc.Predict(now, 80)
	if anchored <= structural*1.5 {
		t.Fatalf("test setup broken: need anchored ≫ structural to detect the bug, got anchored=%.0f structural=%.0f", anchored, structural)
	}

	svc.sample()

	if got := svc.Residuals.Len(); got != 1 {
		t.Fatalf("residual buffer should have exactly one sample after sample(), got %d", got)
	}
	svc.Residuals.mu.Lock()
	rec := svc.Residuals.samples[0]
	svc.Residuals.mu.Unlock()
	// Recorded "predicted" must be the structural value (within
	// floating noise), not the anchored 3.5 kW.
	if math.Abs(rec.predicted-structural) > 1.0 {
		t.Errorf("residual sampler captured predicted=%.2f W; want structural=%.2f W (anchored would have been %.2f W)", rec.predicted, structural, anchored)
	}
	if rec.predicted >= anchored*0.5 {
		t.Errorf("residual sampler captured %.0f W which looks anchored (anchored=%.0f); must be the unanchored structural %.0f", rec.predicted, anchored, structural)
	}
}
