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

// maxLearningW is a numerical sensor-unit guard, not a site hardware limit.
// It permits up to 10 MW; only ACLimitW represents a verified inverter limit.
const maxLearningW = 10_000_000.0

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Model is the learned PV predictor.
type Model struct {
	ConfigRevision    string                `json:"config_revision,omitempty"`
	LearningStartedMS int64                 `json:"learning_started_ms,omitempty"`
	Beta              [NFeat]float64        `json:"beta"`
	P                 [NFeat][NFeat]float64 `json:"p"` // covariance
	Forgetting        float64               `json:"forgetting"`
	Samples           int64                 `json:"samples"`
	LastMs            int64                 `json:"last_ms"`
	MAE               float64               `json:"mae"` // EMA of |err| (W)
	// RelMAE is MAE expressed as a share of the prediction it belongs to
	// (0..1), over the same EMA window. The planner sizes each slot's PV
	// downside against that slot's own expected generation, which a watt
	// figure cannot do. Absent from state persisted before #1020; 0 there
	// reads as "not learned yet" and the planner keeps the flat haircut.
	RelMAE             float64    `json:"rel_mae"`
	RatedW             float64    `json:"rated_w"`    // nominal plate rating (prior)
	ACLimitW           float64    `json:"ac_limit_w"` // verified AC limit only; zero is unknown
	InferredScaleKnown bool       `json:"inferred_scale_known"`
	InferredScaleW     float64    `json:"inferred_scale_w"`
	ScaleCandidateW    float64    `json:"scale_candidate_w"`
	ScaleSamples       uint16     `json:"scale_samples"`
	ScaleDays          uint16     `json:"scale_days"`
	ScaleLastDay       int64      `json:"scale_last_day"`
	ScaleStartMs       int64      `json:"scale_start_ms"`
	ScaleLastMs        int64      `json:"scale_last_ms"`
	ScaleDirection     int        `json:"scale_direction"`
	CoverageDays       [24]uint16 `json:"coverage_days"`
	CoverageLastDay    [24]int64  `json:"coverage_last_day"`
	ChangeCount        int        `json:"change_count"`
	ChangeSign         int        `json:"change_sign"`
	ChangeStartMs      int64      `json:"change_start_ms"`
	ChangeLastMs       int64      `json:"change_last_ms"`
}

// relMAEMinPredictedW gates the relative-error EMA. Below it the denominator
// is small enough that ordinary watt-level noise yields ratios of several
// hundred percent, and the average would report a storm on a clear morning.
const relMAEMinPredictedW = 500.0

// NewModel returns a model anchored on the naive clear-sky prior.
func NewModel(ratedW float64) *Model {
	if !finite(ratedW) || ratedW < 0 || ratedW > maxLearningW {
		ratedW = 0
	}
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

// Predict returns non-negative AC output. Separate days of coverage at the
// target hour control the blend between the learned shape and the scale prior.
// WarmupSamples remains the minimum history for the residual outlier filter.
const WarmupSamples = 50

func (m Model) Predict(clearSkyW, cloudPct float64, t time.Time) float64 {
	if !finite(clearSkyW) || !finite(cloudPct) || clearSkyW > 2000 {
		return 0
	}
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
	scale := m.RatedW
	if m.InferredScaleKnown || m.InferredScaleW > 0 {
		scale = m.InferredScaleW
	}
	prior := scale * (clearSkyW / 1000.0) * cf

	// Dense samples from one morning cannot grant full-day trust.
	trust := m.Trust(t)
	if !finite(learned) {
		learned = prior
	}
	y := trust*learned + (1-trust)*prior

	if y < 0 {
		return 0
	}
	// Only a verified AC boundary limits an otherwise valid forecast.
	if m.ACLimitW > 0 {
		y = math.Min(y, m.ACLimitW)
	}
	return math.Min(y, maxLearningW)
}

// Trust is coverage at the target UTC hour, earned on separate days. Legacy
// states retain their coefficients but start with no earned coverage.
func (m Model) Trust(t time.Time) float64 {
	hour := t.UTC().Hour()
	days := t.Unix()/86400 - m.CoverageLastDay[hour]
	if days < 0 {
		return 0
	}
	// Coverage from a distant season cannot grant full weight to today's
	// solar response. The first fortnight covers the normal planning horizon;
	// older support decays with a 30-day time scale.
	age := math.Exp(-math.Max(0, float64(days-14)) / 30)
	return math.Min(1, float64(m.CoverageDays[hour])/7) * age
}

// Update runs one RLS step. Skipped when clearSky < threshold (night /
// near-night — little signal, mostly noise), or when the residual is a
// large-σ outlier (sensor glitch, inverter restart).
func (m *Model) Update(clearSkyW, cloudPct float64, t time.Time, actualPVW float64) (updated bool) {
	if !finite(clearSkyW) || !finite(cloudPct) || !finite(actualPVW) || clearSkyW > 2000 || actualPVW > maxLearningW {
		return false
	}
	if clearSkyW < 50 {
		return false
	}
	if actualPVW < 0 {
		return false
	}
	// A verified AC limit can reject impossible readings; a size guess cannot.
	if m.ACLimitW > 0 && actualPVW > 1.2*m.ACLimitW {
		return false
	}
	x := Features(clearSkyW, cloudPct, t)
	var yHat float64
	for i := 0; i < NFeat; i++ {
		yHat += m.Beta[i] * x[i]
	}
	err := actualPVW - yHat
	// Recover corrupt numerical state instead of rejecting every later sample.
	if !finite(yHat) || math.Abs(yHat) > maxLearningW*10 {
		fresh := NewModel(m.RatedW)
		m.Beta = fresh.Beta
		m.P = fresh.P
		m.Forgetting = fresh.Forgetting
		yHat = 0
		for i := 0; i < NFeat; i++ {
			yHat += m.Beta[i] * x[i]
		}
		err = actualPVW - yHat
	}
	// After warm-up, reject 10σ outliers. MAE is in W; use it as a proxy
	// for σ (scales with system size, unlike a hard-coded threshold).
	if m.Samples > WarmupSamples {
		band := math.Max(m.MAE*10, 200)
		if math.Abs(err) > band {
			sign := 1
			if err < 0 {
				sign = -1
			}
			if m.ChangeSign != sign || t.UnixMilli()-m.ChangeLastMs > int64((2*time.Hour)/time.Millisecond) {
				m.ChangeCount = 0
				m.ChangeStartMs = t.UnixMilli()
			}
			m.ChangeSign = sign
			m.ChangeCount++
			m.ChangeLastMs = t.UnixMilli()
			if m.ChangeCount < 5 || t.UnixMilli()-m.ChangeStartMs < int64((15*time.Minute)/time.Millisecond) {
				return false
			}
			// Repeated evidence of a new operating regime is not a sensor
			// spike. Retire the old fit and earn coverage again. Resetting its
			// covariance also avoids amplifying ill-conditioned old harmonics.
			fresh := NewModel(m.RatedW)
			m.Beta = [NFeat]float64{}
			if x[2] >= 50 {
				m.Beta[2] = actualPVW / x[2]
			} else {
				m.Beta[1] = actualPVW / clearSkyW
			}
			m.P = fresh.P
			m.CoverageDays = [24]uint16{}
			m.CoverageLastDay = [24]int64{}
			m.ChangeCount = 0
			m.ChangeSign = 0
			yHat = actualPVW
			err = 0
		} else {
			m.ChangeCount = 0
			m.ChangeSign = 0
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
	if !finite(m.Forgetting) || m.Forgetting <= 0 || m.Forgetting > 1 {
		m.Forgetting = 0.995
	}
	denom := m.Forgetting + xPx
	if !finite(denom) || denom <= 0 {
		// A lost covariance direction must not permanently stop learning.
		// Rebuild uncertainty while retaining the finite coefficient estimate.
		m.P = [NFeat][NFeat]float64{}
		xPx = 0
		for i := 1; i < NFeat; i++ {
			m.P[i][i] = 1
			Px[i] = x[i]
			xPx += x[i] * x[i]
		}
		Px[0] = 0
		denom = m.Forgetting + xPx
	}
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
	// Joseph covariance update preserves symmetry and positive directions
	// under repeated, nearly identical feature vectors. The short subtractive
	// form can lose both and produce large extrapolation errors after days.
	var a, ap, newP [NFeat][NFeat]float64
	for i := 1; i < NFeat; i++ {
		for j := 1; j < NFeat; j++ {
			a[i][j] = -K[i] * x[j]
			if i == j {
				a[i][j]++
			}
		}
	}
	for i := 1; i < NFeat; i++ {
		for j := 1; j < NFeat; j++ {
			for k := 1; k < NFeat; k++ {
				ap[i][j] += a[i][k] * m.P[k][j]
			}
		}
	}
	var trace float64
	for i := 1; i < NFeat; i++ {
		for j := i; j < NFeat; j++ {
			v := K[i] * K[j]
			for k := 1; k < NFeat; k++ {
				v += ap[i][k] * a[j][k] / m.Forgetting
			}
			if i == j {
				v = math.Max(0, v)
				trace += v
			}
			newP[i][j] = v
			newP[j][i] = v
		}
	}
	// Bound unobserved covariance growth (not PV power). Scaling the whole
	// matrix preserves its positive directions and finite numerical range.
	if trace > 1e6 {
		for i := 1; i < NFeat; i++ {
			for j := 1; j < NFeat; j++ {
				newP[i][j] *= 1e6 / trace
			}
		}
	}
	m.P = newP

	m.Samples++
	m.LastMs = t.UnixMilli()
	hour, day := t.UTC().Hour(), t.Unix()/86400
	if m.CoverageDays[hour] == 0 || m.CoverageLastDay[hour] != day {
		m.CoverageDays[hour] = min(365, m.CoverageDays[hour]+1)
		m.CoverageLastDay[hour] = day
	}
	m.observeScale(clearSkyW, cloudPct, t, actualPVW)
	// MAE EMA: gives a ~99-sample window; good for outlier banding.
	if m.Samples == 1 {
		m.MAE = math.Abs(err)
	} else {
		m.MAE = 0.99*m.MAE + 0.01*math.Abs(err)
	}
	// Relative twin of the MAE EMA, same window. The ratio is clamped at 1
	// because a 3× miss and a 1× miss both mean "the forecast was worthless",
	// while an unclamped outlier would hold the average up for days. RelMAE
	// == 0 means unseeded — the first qualifying sample sets it outright, so a
	// site that has just learned its error is hedged immediately rather than
	// ramping up from nothing over a hundred samples.
	if yHat >= relMAEMinPredictedW {
		ratio := math.Abs(err) / yHat
		if ratio > 1 {
			ratio = 1
		}
		if m.RelMAE == 0 {
			m.RelMAE = ratio
		} else {
			m.RelMAE = 0.99*m.RelMAE + 0.01*ratio
		}
	}
	// Self-heal the intercept: Features[0] is pinned to 0 (see Features
	// doc), but off-diagonal covariance can still nudge Beta[0] via K[0]
	// each tick. Zeroing it here keeps the dead slot dead, and — importantly
	// — migrates models persisted before issue #134 whose Beta[0] had
	// drifted to a non-zero value during training.
	m.Beta[0] = 0
	return true
}

// observeScale changes the prior only after separate observations support a
// persistent change. Low-light cloud normalization cannot resize the plant.
// A drop requires three separate days (seven for near-zero production), while
// repeated growth can correct an undersized guess within an hour.
func (m *Model) observeScale(cs, cloud float64, t time.Time, actual float64) {
	if cs < 300 || cloud < 0 || cloud > 20 {
		return
	}
	if m.ScaleLastMs > 0 && t.UnixMilli()-m.ScaleLastMs < int64(15*time.Minute/time.Millisecond) {
		return
	}
	scale := actual / (cs / 1000 * math.Pow(1-cloud/100, 1.5))
	base := m.RatedW
	if m.InferredScaleKnown || m.InferredScaleW > 0 {
		base = m.InferredScaleW
	}
	direction := 0
	if scale > base*1.2+50 {
		direction = 1
	} else if scale < base*.8-50 {
		direction = -1
	}
	if direction == 0 {
		m.ScaleDirection = 0
		m.ScaleSamples = 0
		m.ScaleDays = 0
		m.ScaleLastMs = t.UnixMilli()
		return
	}
	if direction != m.ScaleDirection || t.UnixMilli()-m.ScaleLastMs > int64(14*24*time.Hour/time.Millisecond) {
		m.ScaleSamples = 0
		m.ScaleDays = 0
		m.ScaleStartMs = t.UnixMilli()
	}
	m.ScaleDirection = direction
	m.ScaleSamples = min(96, m.ScaleSamples+1)
	if m.ScaleSamples == 1 {
		m.ScaleCandidateW = scale
	} else {
		m.ScaleCandidateW += (scale - m.ScaleCandidateW) / float64(min(16, m.ScaleSamples))
	}
	day := t.Unix() / 86400
	if m.ScaleDays == 0 || day != m.ScaleLastDay {
		m.ScaleDays = min(365, m.ScaleDays+1)
		m.ScaleLastDay = day
	}
	m.ScaleLastMs = t.UnixMilli()
	if m.ScaleSamples < 4 || t.UnixMilli()-m.ScaleStartMs < int64(45*time.Minute/time.Millisecond) {
		return
	}
	if direction < 0 {
		need := uint16(3)
		if m.ScaleCandidateW < base*.05 {
			need = 7
		}
		if m.ScaleDays < need {
			return
		}
	}
	m.InferredScaleW = math.Min(maxLearningW, m.ScaleCandidateW)
	m.InferredScaleKnown = true
}

// Quality is a training-fit diagnostic limited by independent coverage.
// It is not out-of-sample forecast accuracy and must not select a model.
func (m Model) Quality() float64 {
	scale := m.RatedW
	if m.InferredScaleKnown || m.InferredScaleW > 0 {
		scale = m.InferredScaleW
	}
	if m.Samples < 30 || scale <= 0 {
		return 0
	}
	// Relative MAE vs. rated → inverse (lower MAE = higher quality).
	rel := m.MAE / scale
	var coverage float64
	var seen int
	for _, days := range m.CoverageDays {
		if days > 0 {
			coverage += math.Min(1, float64(days)/7)
			seen++
		}
	}
	if seen == 0 {
		return 0
	}
	coverage /= float64(seen)
	if rel <= 0.05 {
		return coverage
	}
	if rel >= 0.5 {
		return 0.0
	}
	return coverage * (1.0 - (rel-0.05)/0.45)
}
