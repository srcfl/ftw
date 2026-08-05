// Package pvmodel is a self-learning digital twin for a PV array.
// It turns (clear_sky_w, cloud_cover, time-of-day) + measured AC output
// into a linear RLS model that captures system-specific effects the
// generic clear-sky formula misses:
//
//   - Orientation / tilt: a south-facing array and a west-facing one
//     with the same rated kW produce wildly different curves.
//   - Shading: a tree or chimney attenuates output at specific hours.
//   - Module degradation: old panels produce less than nameplate.
//   - Cloud-enhancement bias: light clouds sometimes increase output
//     (reflection), which the (1−cloud)^1.5 heuristic can't see.
//   - Snow / soiling persistence (slow baseline drift).
//
// We use RLS because it has exact-by-construction SGD-like behavior
// with guaranteed convergence, tolerates low sample rates (minutes),
// and matches the approach already used for battery dynamics in this
// codebase — so operators have one mental model instead of two.
//
// Feature vector (7 slots, first is dead — see Features() — so effectively
// 6 active terms: clear-sky + cloud-attenuated + 1st + 2nd time-of-day harmonic):
//
//	x = [ 0,   ← dead; PV is proportional to clear-sky, no intercept (issue #134)
//	      clearsky_w,
//	      clearsky_w × (1 − cloud/100)^1.5,
//	      clearsky_w × sin(2π·hour/24), clearsky_w × cos(2π·hour/24),
//	      clearsky_w × sin(4π·hour/24), clearsky_w × cos(4π·hour/24) ]
//
// β=[0,0,rated/1000,0,0,0,0] reproduces the naive physics baseline, so
// starting the model there gives "as good as before" on day one while
// the remaining terms learn orientation + shading asymmetry — including
// sharper patterns (morning-only shade, afternoon tree line) thanks to
// the 2nd harmonic.
package pvmodel

import (
	"math"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/modelstate"
)

// NFeat is the number of features in the RLS regression.
const NFeat = 7

// Model is the learned PV predictor.
type Model struct {
	Beta       [NFeat]float64         `json:"beta"`
	P          [NFeat][NFeat]float64  `json:"p"` // covariance
	Forgetting float64                `json:"forgetting"`
	Samples    int64                  `json:"samples"`
	LastMs     int64                  `json:"last_ms"`
	MAE        float64                `json:"mae"`        // EMA of |err| (W)
	RatedW     float64                `json:"rated_w"`    // nominal plate rating (prior)
}

// NewModel returns a model anchored on the naive clear-sky prior.
func NewModel(ratedW float64) *Model {
	m := &Model{
		Forgetting: 0.995, // ~200-sample effective window
		RatedW:     ratedW,
	}
	// Large initial covariance → model quickly fits new evidence.
	for i := 0; i < NFeat; i++ {
		m.P[i][i] = 1000.0
	}
	// β[2] = rated / 1000 gives naive: P ≈ clearsky × cloudFactor × rated/1000.
	// Scale this: pv = rated × (clearsky/1000) × cloud_factor
	// So coefficient on clearsky*cloud_factor is rated/1000.
	if ratedW > 0 {
		m.Beta[2] = ratedW / 1000.0
	} else {
		m.Beta[2] = 1.0
	}
	return m
}

// Features returns the feature vector for a given forecast sample.
//
// Hour-of-day uses UTC so the harmonic phase is stable across DST
// transitions and matches sunpos's UTC convention (see sunpos.go).
//
// Slot 0 is held at 0.0 (not 1.0) on purpose: PV physics pass through
// the origin — zero sun ⇒ zero output. An RLS-learned intercept has no
// physical basis and, left free, drifted during training and leaked into
// night-time predictions (issue #133/#134). Keeping NFeat=7 with a dead
// first slot preserves on-disk Beta persistence; any loaded Beta[0] is
// multiplied by 0 and has no effect on predictions. The Update path
// also re-zeros Beta[0] so drifted persisted models self-heal.
func Features(clearSkyW, cloudPct float64, t time.Time) [NFeat]float64 {
	cloudFrac := cloudPct / 100.0
	if cloudFrac < 0 {
		cloudFrac = 0
	}
	if cloudFrac > 1 {
		cloudFrac = 1
	}
	cf := math.Pow(1-cloudFrac, 1.5)
	u := t.UTC()
	hour := float64(u.Hour()) + float64(u.Minute())/60.0
	h := 2 * math.Pi * hour / 24.0
	return [NFeat]float64{
		0.0, // dead slot — see doc comment above
		clearSkyW,
		clearSkyW * cf,
		clearSkyW * math.Sin(h),
		clearSkyW * math.Cos(h),
		clearSkyW * math.Sin(2*h),
		clearSkyW * math.Cos(2*h),
	}
}

// featureSemantics declares what the arguments to Features mean. It is the
// half of the fingerprint a probe cannot derive: pass clear-sky irradiance
// projected onto the array plane instead of the horizontal, or low-cloud
// cover instead of total, and Features returns exactly the same numbers for
// the same arguments while Beta is now fitted against a different physical
// quantity.
//
// CHANGE THIS STRING in the commit that changes what a caller passes in.
// Changes to the feature math itself need no edit here — featureProbe moves
// the fingerprint on its own.
const featureSemantics = "pvmodel/1 clearsky=horizontal_ghi_wm2 cloud=total_cover_pct hour=utc target=ac_w"

// featureProbe evaluates Features across a fixed grid of inputs. Its output
// is the automatic half of the fingerprint: add a harmonic, revive the dead
// slot, reorder a term or change the cloud exponent, and these numbers move.
//
// The grid spans night and full sun, clear and overcast, and four times of
// day chosen so both harmonics take distinct values — a probe that only
// looked at noon would miss a change to the phase of either one.
func featureProbe() []float64 {
	times := []time.Time{
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 6, 21, 5, 30, 0, 0, time.UTC),
		time.Date(2024, 6, 21, 12, 0, 0, 0, time.UTC),
		time.Date(2024, 12, 21, 17, 45, 0, 0, time.UTC),
	}
	clearSky := []float64{0, 137.5, 900}
	cloud := []float64{0, 42.5, 100}
	out := make([]float64, 0, len(times)*len(clearSky)*len(cloud)*NFeat)
	for _, t := range times {
		for _, cs := range clearSky {
			for _, cc := range cloud {
				x := Features(cs, cc, t)
				out = append(out, x[:]...)
			}
		}
	}
	return out
}

var featureHash = sync.OnceValue(func() string {
	return modelstate.Fingerprint(featureSemantics, featureProbe())
})

// FeatureHash fingerprints the feature space Beta is fitted against. Stored
// coefficients are only restored when their recorded hash matches this one;
// see internal/modelstate for why, and service.go for what happens when it
// does not.
func FeatureHash() string { return featureHash() }

// Predict returns the expected AC output in W (non-negative). Cold-start
// behavior: during the first WarmupSamples we blend the learned β with
// the naive physics prior so a wild β coefficient (which RLS can take a
// few samples to tame) doesn't produce an unreasonable forecast.
//
// After ~warmup samples we trust the learned model fully.
const WarmupSamples = 50

func (m Model) Predict(clearSkyW, cloudPct float64, t time.Time) float64 {
	// Physics gate: no sun above the horizon → no PV output. Mirrors the
	// Update-side guard (`clearSkyW < 50` skips training), so prediction
	// and training share one definition of "night". Without this gate the
	// intercept term in Features (x[0]=1.0) projects Beta[0] into every
	// night slot — and since RLS is free to pick a non-zero Beta[0] when
	// that minimizes daytime residual, the model routinely emits
	// 500-1500 W of phantom generation at 02:00. See issue #133.
	if clearSkyW < 50 {
		return 0
	}
	// Learned prediction.
	x := Features(clearSkyW, cloudPct, t)
	var learned float64
	for i := 0; i < NFeat; i++ {
		learned += m.Beta[i] * x[i]
	}

	// Naive physics prior: rated × (clear_sky / 1000) × (1-cloud)^1.5.
	// Same formula forecast.EstimatePVW uses; kept local to avoid a
	// package dep and because we evaluate it on every predict call.
	cf := 1.0
	if cloudPct > 0 {
		c := cloudPct / 100.0
		if c > 1 {
			c = 1
		}
		cf = math.Pow(1-c, 1.5)
	}
	prior := m.RatedW * (clearSkyW / 1000.0) * cf

	// Trust = samples / WarmupSamples, clipped to [0, 1].
	// samples=0  → 100% prior.
	// samples≥50 → 100% learned.
	trust := float64(m.Samples) / float64(WarmupSamples)
	if trust > 1 {
		trust = 1
	}
	y := trust*learned + (1-trust)*prior

	if y < 0 {
		return 0
	}
	// Hard cap at 105% of nameplate. Anything above is RLS having a bad
	// day — fall back to the physics prior, which is bounded by construction.
	if m.RatedW > 0 && y > 1.05*m.RatedW {
		return prior
	}
	return y
}

// Update runs one RLS step. Skipped when clearSky < threshold (night /
// near-night — little signal, mostly noise), or when the residual is a
// large-σ outlier (sensor glitch, inverter restart).
func (m *Model) Update(clearSkyW, cloudPct float64, t time.Time, actualPVW float64) (updated bool) {
	if clearSkyW < 50 {
		return false
	}
	if actualPVW < 0 {
		return false
	}
	// Physical sanity envelope: anything wildly above nameplate is sensor
	// noise (inverter restart, transient) — never feed it to RLS.
	if m.RatedW > 0 && actualPVW > 1.2*m.RatedW {
		return false
	}
	x := Features(clearSkyW, cloudPct, t)
	var yHat float64
	for i := 0; i < NFeat; i++ {
		yHat += m.Beta[i] * x[i]
	}
	err := actualPVW - yHat
	// Cold-start outlier guard: before the MAE-based filter kicks in, reject
	// samples where the predicted value is already absurd (>2× rated). This
	// stops a single bad sample from cascading into wild β coefficients.
	if m.RatedW > 0 && math.Abs(yHat) > 2*m.RatedW {
		return false
	}
	// After warm-up, reject 10σ outliers. MAE is in W; use it as a proxy
	// for σ (scales with system size, unlike a hard-coded threshold).
	if m.Samples > 50 {
		band := math.Max(m.MAE*10, 200)
		if math.Abs(err) > band {
			return false
		}
	}

	// K = P·x / (λ + x^T·P·x)
	var Px [NFeat]float64
	for i := 0; i < NFeat; i++ {
		var s float64
		for j := 0; j < NFeat; j++ {
			s += m.P[i][j] * x[j]
		}
		Px[i] = s
	}
	var xPx float64
	for i := 0; i < NFeat; i++ {
		xPx += x[i] * Px[i]
	}
	denom := m.Forgetting + xPx
	var K [NFeat]float64
	for i := 0; i < NFeat; i++ {
		K[i] = Px[i] / denom
	}

	// β += K · err
	for i := 0; i < NFeat; i++ {
		m.Beta[i] += K[i] * err
	}

	// P = (P − K·xᵀ·P) / λ
	//
	// Skip row/column 0 entirely: the intercept feature is hardcoded to 0
	// (see Features() — a dead slot since #134), so no information flows
	// into it. Letting the standard update run would still divide P[0][0]
	// by λ every tick — growth ~1.005× per sample compounds to Inf after
	// ~140k samples, after which Px[0] = Inf*0 = NaN poisons K, β, and
	// all predictions. Freezing row/column 0 at zero keeps the
	// dead-slot invariant numerically stable forever. Codex P1 on PR #136.
	var newP [NFeat][NFeat]float64
	for i := 1; i < NFeat; i++ {
		for j := 1; j < NFeat; j++ {
			var kxTP float64
			for k := 1; k < NFeat; k++ {
				kxTP += K[i] * x[k] * m.P[k][j]
			}
			newP[i][j] = (m.P[i][j] - kxTP) / m.Forgetting
		}
	}
	m.P = newP

	m.Samples++
	m.LastMs = t.UnixMilli()
	// MAE EMA: gives a ~99-sample window; good for outlier banding.
	if m.Samples == 1 {
		m.MAE = math.Abs(err)
	} else {
		m.MAE = 0.99*m.MAE + 0.01*math.Abs(err)
	}
	// Self-heal the intercept: Features[0] is pinned to 0 (see Features
	// doc), but off-diagonal covariance can still nudge Beta[0] via K[0]
	// each tick. Zeroing it here keeps the dead slot dead, and — importantly
	// — migrates models persisted before issue #134 whose Beta[0] had
	// drifted to a non-zero value during training.
	m.Beta[0] = 0
	return true
}

// Quality reports how confident we are in the model. 0 = untrained,
// 1.0+ = fully converged (matches rated_w → 5% MAE threshold).
func (m Model) Quality() float64 {
	if m.Samples < 30 || m.RatedW <= 0 {
		return 0
	}
	// Relative MAE vs. rated → inverse (lower MAE = higher quality).
	rel := m.MAE / m.RatedW
	if rel <= 0.05 {
		return 1.0
	}
	if rel >= 0.5 {
		return 0.0
	}
	return 1.0 - (rel-0.05)/0.45
}
