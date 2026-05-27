// Package battery is per-battery online learning (ARX(1) via RLS) plus the
// cascade controller glue (inner PI + inverse model + saturation curves).
// Port of the Rust src/battery_model.rs + src/control.rs cascade parts.
//
// Under the site sign convention (+ = into site, − = out of site):
//
//	command u = charge (+) or discharge (−)
//	actual  y = same convention
//
// The ARX(1) model y(t+1) = a·y(t) + b·u(t) is sign-agnostic — we learn the
// same `a` and `b` regardless of convention. Only the curves and clamps need
// to be convention-aware.
package battery

import (
	"math"
	"time"
)

// Constants tuned to match Rust src/battery_model.rs exactly.
const (
	DefaultForgetting = 0.99   // ~100-cycle effective window
	InitialCov        = 1000.0 // high initial uncertainty
	SoCBucket         = 0.05   // 5% SoC buckets for saturation curve
	SatDecay          = 0.9999 // slow decay so old peaks fade
	MinCommandForRLS  = 100.0  // W
	MinDeltaForRLS    = 20.0   // W after warmup
	OutlierSigma      = 5.0    // σ threshold for rejecting a residual
	GainHistoryLen    = 2000
	MinSatSeedW       = 1000.0 // regression guard (see clamp_to_saturation comment)
)

// Model holds the per-battery ARX(1) state + saturation envelope + health baseline.
type Model struct {
	Name string `json:"name"`

	// ARX(1) params: y(t+1) = A·y(t) + B·u(t) + noise
	A float64 `json:"a"`
	B float64 `json:"b"`

	// 2×2 covariance matrix for RLS
	P [2][2]float64 `json:"p"`
	// Running count + residual-variance EMA for confidence
	NSamples       uint64  `json:"n_samples"`
	ResidualVarEMA float64 `json:"residual_var_ema"`

	// Last (u, y) pair — regressor for the next RLS update
	LastU   float64 `json:"last_u"`
	LastY   float64 `json:"last_y"`
	HasPrev bool    `json:"has_prev"`

	// Saturation envelopes: per-SoC-bucket max observed |actual|
	// Stored as sorted slice of (soc, maxW).
	MaxChargeCurve    []SoCPoint `json:"max_charge_curve"`
	MaxDischargeCurve []SoCPoint `json:"max_discharge_curve"`

	// Empirical deadband (smallest command magnitude that produced response)
	DeadbandW float64 `json:"deadband_w"`

	// Hardware health — baseline set by self-tune
	BaselineGain   *float64 `json:"baseline_gain,omitempty"`
	BaselineTauS   *float64 `json:"baseline_tau_s,omitempty"`
	LastCalibrated *int64   `json:"last_calibrated_ts_ms,omitempty"`

	// Gain history for drift-per-day slope
	GainHistory []GainPoint `json:"gain_history"`

	LastUpdatedMs    int64   `json:"last_updated_ts_ms"`
	ForgettingFactor float64 `json:"forgetting_factor"`
}

// SoCPoint is one (SoC, value) pair in a saturation curve.
type SoCPoint struct {
	SoC   float64 `json:"soc"`
	Value float64 `json:"value"`
}

// GainPoint is one (ts_ms, gain) pair in the health history.
type GainPoint struct {
	TsMs int64   `json:"ts_ms"`
	Gain float64 `json:"gain"`
}

// New creates a fresh model with sensible defaults.
func New(name string) *Model {
	return &Model{
		Name:              name,
		A:                 0.7, // moderate memory
		B:                 0.3, // moderate gain (steady-state ≈ 1.0)
		P:                 [2][2]float64{{InitialCov, 0}, {0, InitialCov}},
		ResidualVarEMA:    1000,
		MaxChargeCurve:    []SoCPoint{},
		MaxDischargeCurve: []SoCPoint{},
		DeadbandW:         50,
		GainHistory:       []GainPoint{},
		ForgettingFactor:  DefaultForgetting,
	}
}

// SteadyStateGainRaw returns k = b/(1-a) without clamping. May be unphysical.
func (m *Model) SteadyStateGainRaw() float64 {
	denom := 1 - m.A
	if math.Abs(denom) < 1e-3 {
		return 1.0
	}
	return m.B / denom
}

// SteadyStateGain returns the gain clamped to [0.3, 1.5] for display/control.
func (m *Model) SteadyStateGain() float64 {
	g := m.SteadyStateGainRaw()
	if g < 0.3 {
		return 0.3
	}
	if g > 1.5 {
		return 1.5
	}
	return g
}

// TimeConstantS returns τ in seconds. Clamped to [0.05, 60].
func (m *Model) TimeConstantS(dtS float64) float64 {
	if m.A >= 0.999 || m.A <= 0 {
		return 999
	}
	tau := -dtS / math.Log(m.A)
	if tau < 0.05 {
		return 0.05
	}
	if tau > 60 {
		return 60
	}
	return tau
}

// Confidence in [0,1] from sample count and residual variance.
func (m *Model) Confidence() float64 {
	n := float64(m.NSamples) / 200.0
	if n > 1 {
		n = 1
	}
	v := 1 - (m.ResidualVarEMA / 10000.0)
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return n * v
}

// HealthScore in [0,1] — compares current gain to baseline set by self-tune.
// Returns 1.0 if no baseline.
func (m *Model) HealthScore() float64 {
	if m.BaselineGain == nil || math.Abs(*m.BaselineGain) < 0.1 {
		return 1.0
	}
	drift := math.Abs(m.SteadyStateGain()-*m.BaselineGain) / math.Abs(*m.BaselineGain)
	h := 1.0 - 2.0*drift
	if h < 0 {
		h = 0
	}
	if h > 1 {
		h = 1
	}
	return h
}

// HealthDriftPerDay = linear regression slope over GainHistory (gain/day).
// Negative = gain is declining over time (aging).
func (m *Model) HealthDriftPerDay() float64 {
	if len(m.GainHistory) < 10 {
		return 0
	}
	n := float64(len(m.GainHistory))
	var meanT, meanG float64
	for _, p := range m.GainHistory {
		meanT += float64(p.TsMs)
		meanG += p.Gain
	}
	meanT /= n
	meanG /= n
	var num, den float64
	for _, p := range m.GainHistory {
		dt := float64(p.TsMs) - meanT
		num += dt * (p.Gain - meanG)
		den += dt * dt
	}
	if den < 1 {
		return 0
	}
	slopePerMs := num / den
	return slopePerMs * 86400_000
}

// Inverse returns the command to SEND to achieve desired actual power.
// Falls back to passthrough if the raw gain is implausible (model hasn't
// converged, has diverged, or the plant misbehaves).
func (m *Model) Inverse(target float64) float64 {
	g := m.SteadyStateGainRaw()
	if math.Abs(g) < 0.3 || math.Abs(g) > 2.0 {
		return target // unhealthy model, don't trust
	}
	return target / m.SteadyStateGain()
}

// ClampToSaturation clips a target to the empirical max observed at this SoC.
// Site convention: + target = charging, − = discharging. Saturation curves
// store positive magnitudes, so we compare |target| vs curve value.
func (m *Model) ClampToSaturation(target, soc float64) (clamped float64, wasClamped bool) {
	if target > 0 {
		max := interpolate(m.MaxChargeCurve, soc, 5000)
		if target > max {
			return max, true
		}
	} else if target < 0 {
		max := interpolate(m.MaxDischargeCurve, soc, 5000)
		if -target > max {
			return -max, true
		}
	}
	return target, false
}

// Update feeds one (command, actual) observation into RLS and refreshes all
// derived state. Returns true if the model was updated, false if the
// observation was filtered out (too small to be informative).
func (m *Model) Update(command, actual, soc, dtS float64, nowMs int64) bool {
	if !finite(command) || !finite(actual) || !finite(soc) || !finite(dtS) ||
		soc < 0 || soc > 1 || dtS <= 0 {
		return false
	}

	// Saturation curve is always updated (decoupled from RLS gating)
	m.updateSaturationCurves(actual, soc)

	if !m.HasPrev {
		m.LastU = command
		m.LastY = actual
		m.HasPrev = true
		return false
	}

	// Gate: low signal → skip
	if math.Abs(m.LastU) < MinCommandForRLS {
		m.LastU = command
		m.LastY = actual
		return false
	}
	if math.Abs(actual-m.LastY) < MinDeltaForRLS && m.NSamples > 50 {
		m.LastU = command
		m.LastY = actual
		return false
	}

	// Regressor φ = [y(t-1), u(t-1)]
	phi := [2]float64{m.LastY, m.LastU}
	// Prediction and residual
	pred := m.A*phi[0] + m.B*phi[1]
	err := actual - pred

	// Outlier rejection
	std := math.Sqrt(m.ResidualVarEMA)
	if std < 10 {
		std = 10
	}
	if math.Abs(err) > OutlierSigma*std && m.NSamples > 20 {
		m.LastU = command
		m.LastY = actual
		return false
	}

	// RLS update
	pp := [2]float64{
		m.P[0][0]*phi[0] + m.P[0][1]*phi[1],
		m.P[1][0]*phi[0] + m.P[1][1]*phi[1],
	}
	denom := m.ForgettingFactor + phi[0]*pp[0] + phi[1]*pp[1]
	if math.Abs(denom) < 1e-9 {
		m.LastU = command
		m.LastY = actual
		return false
	}
	k := [2]float64{pp[0] / denom, pp[1] / denom}

	newA := m.A + k[0]*err
	newB := m.B + k[1]*err
	// Stability bounds
	newA = clamp(newA, 0.1, 0.99)
	newB = clamp(newB, -1.5, 1.5)
	m.A = newA
	m.B = newB

	// P update: P_new = (P - k · ppᵀ) / λ
	newP := [2][2]float64{
		{
			(m.P[0][0] - k[0]*pp[0]) / m.ForgettingFactor,
			(m.P[0][1] - k[0]*pp[1]) / m.ForgettingFactor,
		},
		{
			(m.P[1][0] - k[1]*pp[0]) / m.ForgettingFactor,
			(m.P[1][1] - k[1]*pp[1]) / m.ForgettingFactor,
		},
	}
	m.P = newP

	// Residual variance EMA
	m.ResidualVarEMA = 0.95*m.ResidualVarEMA + 0.05*err*err

	m.LastU = command
	m.LastY = actual
	m.NSamples++
	m.LastUpdatedMs = nowMs

	// Track gain history for drift detection
	g := m.SteadyStateGain()
	m.GainHistory = append(m.GainHistory, GainPoint{TsMs: nowMs, Gain: g})
	if len(m.GainHistory) > GainHistoryLen {
		m.GainHistory = m.GainHistory[len(m.GainHistory)-GainHistoryLen:]
	}
	return true
}

// SetBaseline records a clean (gain, τ) fit from a self-tune run. Future
// RLS drift is measured relative to this baseline via HealthScore.
func (m *Model) SetBaseline(gain, tauS float64, nowMs int64) {
	m.BaselineGain = &gain
	m.BaselineTauS = &tauS
	m.LastCalibrated = &nowMs
}

// SetFromStepFit overrides ARX params with a clean step-response fit
// (from self-tune). Resets covariance to indicate high confidence.
func (m *Model) SetFromStepFit(gain, tauS, dtS float64) {
	a := math.Exp(-dtS / math.Max(tauS, 0.1))
	a = clamp(a, 0.001, 0.99)
	b := clamp((1-a)*gain, -1.5, 1.5)
	m.A = a
	m.B = b
	m.P = [2][2]float64{{10, 0}, {0, 10}}
	m.ResidualVarEMA = 100
}

// ---- internals ----

func (m *Model) updateSaturationCurves(actual, soc float64) {
	bucket := math.Round(soc/SoCBucket) * SoCBucket
	if bucket < 0 {
		bucket = 0
	}
	if bucket > 1 {
		bucket = 1
	}

	if actual > 0 {
		m.MaxChargeCurve = updateCurve(m.MaxChargeCurve, bucket, actual)
	} else if actual < 0 {
		m.MaxDischargeCurve = updateCurve(m.MaxDischargeCurve, bucket, -actual)
	}
	// Decay so old over-optimistic peaks fade
	for i := range m.MaxChargeCurve {
		m.MaxChargeCurve[i].Value *= SatDecay
	}
	for i := range m.MaxDischargeCurve {
		m.MaxDischargeCurve[i].Value *= SatDecay
	}
}

// updateCurve inserts or raises a bucket; NEVER seeds a new bucket with small
// values (guards against the self-reinforcing clamp loop we fixed in Rust).
func updateCurve(curve []SoCPoint, bucket, value float64) []SoCPoint {
	for i := range curve {
		if math.Abs(curve[i].SoC-bucket) < 1e-6 {
			if value > curve[i].Value {
				curve[i].Value = value
			}
			return curve
		}
	}
	if value < MinSatSeedW {
		return curve
	}
	// Insert sorted
	out := append(curve, SoCPoint{SoC: bucket, Value: value})
	for i := len(out) - 1; i > 0 && out[i].SoC < out[i-1].SoC; i-- {
		out[i], out[i-1] = out[i-1], out[i]
	}
	return out
}

func interpolate(curve []SoCPoint, soc, fallback float64) float64 {
	if len(curve) == 0 {
		return fallback
	}
	if soc <= curve[0].SoC {
		return curve[0].Value
	}
	if soc >= curve[len(curve)-1].SoC {
		return curve[len(curve)-1].Value
	}
	for i := 0; i < len(curve)-1; i++ {
		a, b := curve[i], curve[i+1]
		if soc >= a.SoC && soc <= b.SoC {
			denom := b.SoC - a.SoC
			if denom < 1e-6 {
				return a.Value
			}
			t := (soc - a.SoC) / denom
			return a.Value + t*(b.Value-a.Value)
		}
	}
	return curve[len(curve)-1].Value
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// Duration helper for test readability
var _ = time.Second
