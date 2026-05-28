package pvmodel

import (
	"math"
	"sync"
	"time"
)

// Rolling residual correction — short-horizon additive bias on top of
// the structural RLS twin.
//
// Motivation: the RLS model's forgetting factor (~200-sample window @
// 60s cadence → ~3h half-life) is tuned to capture orientation,
// shading and slow soiling drift. It does NOT respond fast enough to
// "today's persistent NWP bias" — e.g. when measured cloud cover is
// heavier than the forecast assumed for the last 90 minutes, the
// twin's structural prediction stays biased high until RLS has chewed
// through enough samples.
//
// The residual buffer fits a very cheap signal (rolling mean of
// recent prediction-vs-actual residuals) and applies it as an
// additive bias to near-future predictions, fading linearly to zero
// over a 2h horizon. Beyond 2h the structural model is again the
// best estimate — weather fronts have rolled in, time-of-day is
// different, etc.
//
// This is intentionally NOT applied to load (other PRs in flight on
// load observability): load is multimodal — appliances kicking on
// and off can produce a high-mean / high-variance residual stream
// that a rolling-mean correction would chase as noise. The variance
// gate below would catch it most of the time but the risk/reward is
// poor without dedicated diagnostics. Revisit when load observability
// data is in.

// Tunables. Constants rather than fields on the buffer so they stay
// inspectable in `/api/pvmodel` diagnostics and so callers can't
// silently disable the gates by setting them to extreme values.
const (
	// ResidualBufferWindow is how far back the rolling residual reaches.
	// Matches NowAnchorHorizon (2h) on purpose: both mechanisms fade out
	// over the same window, so dispatch decisions see one consistent
	// "short-horizon" semantic.
	ResidualBufferWindow = 2 * time.Hour
	// residualBufferMaxSamples bounds memory regardless of sample
	// cadence. 240 = 1 sample every 30s for 2h, which is roughly twice
	// the live sampling rate of pvmodel.Service (60s cadence). Drops
	// oldest first.
	residualBufferMaxSamples = 240
	// residualMinSamples gates Correct() — below this we have too few
	// data points to estimate either mean or std reliably.
	residualMinSamples = 20
	// residualEpsilonW is the "no bias detected" floor on |mean|. The
	// twin's MAE on a converged 5 kW system is typically 100-300 W;
	// pushing a < 25 W mean correction into the plan adds noise.
	residualEpsilonW = 25.0
	// residualMaxCoVar gates Correct() on std/|mean|. Above 1.0 the
	// noise dominates the signal — applying the mean would chase
	// transients (a single inverter restart, a sensor glitch).
	residualMaxCoVar = 1.0
	// residualFullHorizon: dt ≤ this → factor = 1 (full correction).
	residualFullHorizon = 30 * time.Minute
	// residualFadeEnd: dt ≥ this → factor = 0 (no correction).
	residualFadeEnd = 2 * time.Hour
)

type residualSample struct {
	t         time.Time
	predicted float64 // what the twin said at time t (W, ≥ 0)
	actual    float64 // what the meter/PV sensor reported at time t (W, ≥ 0)
}

// ResidualBuffer is a thread-safe sliding window of recent (predicted,
// actual) PV samples used to compute a short-horizon additive
// correction to base predictions. Safe for concurrent use; cheap to
// poll because all reads take the lock for a few microseconds.
type ResidualBuffer struct {
	mu      sync.Mutex
	samples []residualSample
}

// ResidualDiag is a snapshot of the buffer state for /api/pvmodel.
type ResidualDiag struct {
	SampleCount   int     `json:"sample_count"`
	MeanW         float64 `json:"mean_w"`
	StdW          float64 `json:"std_w"`
	WindowMinutes int     `json:"window_minutes"`
}

// NewResidualBuffer returns an empty buffer with default window +
// capacity. Callers should hang onto a single instance per process —
// the buffer carries observed state, not config.
func NewResidualBuffer() *ResidualBuffer {
	return &ResidualBuffer{
		samples: make([]residualSample, 0, residualBufferMaxSamples),
	}
}

// Add records one (predicted, actual) pair at time t. Drops samples
// older than ResidualBufferWindow relative to t, then enforces the
// max-samples cap by dropping the oldest.
//
// Predicted is the twin's prediction for time t; actual is the
// summed measured PV power at time t (W, ≥ 0). Both use the
// generation-positive convention to match liveActualPV().
func (b *ResidualBuffer) Add(t time.Time, predicted, actual float64) {
	if b == nil {
		return
	}
	if math.IsNaN(predicted) || math.IsInf(predicted, 0) ||
		math.IsNaN(actual) || math.IsInf(actual, 0) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.samples = append(b.samples, residualSample{t: t, predicted: predicted, actual: actual})
	// Age off relative to the just-added sample (most recent), so the
	// window slides with real time even when called from tests using
	// fixed timestamps.
	cutoff := t.Add(-ResidualBufferWindow)
	idx := 0
	for ; idx < len(b.samples); idx++ {
		if !b.samples[idx].t.Before(cutoff) {
			break
		}
	}
	if idx > 0 {
		b.samples = b.samples[idx:]
	}
	// Cap.
	if over := len(b.samples) - residualBufferMaxSamples; over > 0 {
		b.samples = b.samples[over:]
	}
}

// Len returns the current sample count (mostly for tests).
func (b *ResidualBuffer) Len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.samples)
}

// Correct returns the additive correction (W) to apply to a base
// prediction targeting time tTarget, computed at wall time now.
// Returns 0 when no correction should be applied (insufficient
// samples, no bias detected, noise-dominated, target outside the
// horizon, target in the past).
//
// basePrediction is accepted for future extensions (multiplicative
// gating, sign-aware blends) but currently unused — the correction
// is purely additive on the residual mean.
func (b *ResidualBuffer) Correct(now, tTarget time.Time, basePrediction float64) float64 {
	if b == nil {
		return 0
	}
	_ = basePrediction
	// Ramp-off based on dt = tTarget - now.
	dt := tTarget.Sub(now)
	if dt <= 0 {
		return 0
	}
	if dt > residualFadeEnd {
		return 0
	}
	b.mu.Lock()
	// Drop samples older than window relative to now (the buffer ages
	// off on Add, but if Correct is called long after the last Add the
	// data is stale).
	cutoff := now.Add(-ResidualBufferWindow)
	idx := 0
	for ; idx < len(b.samples); idx++ {
		if !b.samples[idx].t.Before(cutoff) {
			break
		}
	}
	if idx > 0 {
		b.samples = b.samples[idx:]
	}
	n := len(b.samples)
	if n < residualMinSamples {
		b.mu.Unlock()
		return 0
	}
	var sum, sumSq float64
	for _, s := range b.samples {
		r := s.actual - s.predicted
		sum += r
		sumSq += r * r
	}
	b.mu.Unlock()
	mean := sum / float64(n)
	variance := sumSq/float64(n) - mean*mean
	if variance < 0 {
		variance = 0 // numerical
	}
	std := math.Sqrt(variance)
	if math.Abs(mean) < residualEpsilonW {
		return 0
	}
	denom := math.Abs(mean)
	if denom < 1 {
		denom = 1
	}
	if std/denom > residualMaxCoVar {
		return 0
	}
	factor := residualFadeFactor(dt)
	return mean * factor
}

// residualFadeFactor is the unit-less ramp-off:
//   - dt ≤ 30 min → 1.0
//   - 30 min < dt ≤ 120 min → linear 1.0 → 0.0
//   - dt > 120 min → 0.0
func residualFadeFactor(dt time.Duration) float64 {
	if dt <= residualFullHorizon {
		return 1.0
	}
	if dt >= residualFadeEnd {
		return 0.0
	}
	mins := dt.Minutes()
	full := residualFullHorizon.Minutes()
	end := residualFadeEnd.Minutes()
	return 1.0 - (mins-full)/(end-full)
}

// Diag returns a snapshot suitable for serialising to /api/pvmodel.
// Computes mean + std over the in-window samples; does not mutate.
func (b *ResidualBuffer) Diag(now time.Time) ResidualDiag {
	d := ResidualDiag{WindowMinutes: int(ResidualBufferWindow.Minutes())}
	if b == nil {
		return d
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := now.Add(-ResidualBufferWindow)
	var sum, sumSq float64
	n := 0
	for _, s := range b.samples {
		if s.t.Before(cutoff) {
			continue
		}
		r := s.actual - s.predicted
		sum += r
		sumSq += r * r
		n++
	}
	d.SampleCount = n
	if n == 0 {
		return d
	}
	mean := sum / float64(n)
	variance := sumSq/float64(n) - mean*mean
	if variance < 0 {
		variance = 0
	}
	d.MeanW = mean
	d.StdW = math.Sqrt(variance)
	return d
}
