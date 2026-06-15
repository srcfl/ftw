// Package thermalmodel is a self-learning digital twin for a building's
// thermal dynamics — the "thermal battery" a thermostat lets the EMS
// charge (pre-heat when energy is cheap) and discharge (coast through
// expensive hours while staying inside the comfort band).
//
// We model a single thermal zone as a first-order RC network (1R1C):
//
//	C · dT_in/dt = (T_out − T_in)/R + η·P_heat
//
// Dividing by C and discretizing over a step Δt (seconds):
//
//	T_in[k+1] = T_in[k] + Δt·( a·(T_out[k] − T_in[k]) + b·P_heat[k] )
//
// where
//
//	a = 1/(R·C)   [1/s]            — heat-loss rate (couples to outdoor ΔT)
//	b = η/C       [°C/(W·s)]       — heating gain (how fast power warms the zone)
//
// The thermal time constant τ = R·C = 1/a is the headline number an
// operator cares about: a well-insulated apartment has τ ≈ 4–8 h, a
// draughty house τ ≈ 1–2 h. τ sets how many hours of pre-heat the MPC
// can bank before comfort decays.
//
// We fit (a, b) with RLS over the observed temperature deltas — the same
// estimator the PV twin (pvmodel) and battery dynamics (battery/arx) use,
// so operators have one mental model. RLS converges fast at minute-scale
// sampling and tolerates the slow, noisy signal a room thermometer gives.
//
// Feature vector (2 active terms, no intercept — at thermal equilibrium
// with no heating the zone sits at outdoor temp, so the dynamics pass
// through the origin):
//
//	x = [ (T_out − T_in),  P_heat ]
//	y = (T_in[k+1] − T_in[k]) / Δt          ← observed warming rate
//	β = [ a, b ]
package thermalmodel

import (
	"math"
	"time"
)

// NFeat is the number of features in the RLS regression: heat-loss term
// (T_out − T_in) and heating term P_heat.
const NFeat = 2

// WarmupSamples is how many accepted updates before the learned model is
// trusted fully; below it we blend toward the physics prior.
const WarmupSamples = 40

// Model is the learned single-zone thermal predictor. Persisted verbatim
// as JSON (mirrors pvmodel.Model).
type Model struct {
	Beta       [NFeat]float64        `json:"beta"`        // [a, b]
	P          [NFeat][NFeat]float64 `json:"p"`           // RLS covariance
	Forgetting float64               `json:"forgetting"`  // exponential discount
	Samples    int64                 `json:"samples"`
	LastMs     int64                 `json:"last_ms"`
	MAE        float64               `json:"mae"`         // EMA of |Δtemp err| per step (°C)
}

// Default priors. τ ≈ 4 h ⇒ a ≈ 1/14400 s⁻¹. b is chosen to be physically
// consistent with a — i.e. so the implied steady-state holding power
// P_hold = a·ΔT/b is reasonable for a typical zone (~2 kW at ΔT=21 °C,
// UA ≈ 95 W/°C): b = a·ΔT/P_hold = (1/14400)·21/2000 ≈ 7.3e-7 °C/(W·s).
// That also implies a 2 kW heater warms the zone by ~b·P·Δt ≈ 1.3 °C over
// a 15-min step before losses — a sensible prior the RLS learns away from.
const (
	priorA = 1.0 / 14400.0 // heat-loss rate for τ≈4h
	priorB = 7.3e-7        // heating gain, consistent with priorA
)

// NewModel returns a model anchored on physically reasonable priors so
// day-one predictions are usable before any training.
func NewModel() *Model {
	m := &Model{
		Forgetting: 0.997, // ~330-sample effective window — thermal params drift slowly
	}
	// Large initial covariance → RLS adapts quickly away from the prior
	// once real evidence arrives.
	for i := 0; i < NFeat; i++ {
		m.P[i][i] = 1.0
	}
	m.Beta[0] = priorA
	m.Beta[1] = priorB
	return m
}

// Features returns the regression input for one observed transition.
func Features(outdoorC, indoorC, heatW float64) [NFeat]float64 {
	return [NFeat]float64{
		outdoorC - indoorC,
		heatW,
	}
}

// TauSeconds returns the learned thermal time constant τ = 1/a in
// seconds, or 0 if the model hasn't produced a positive heat-loss rate.
func (m Model) TauSeconds() float64 {
	if m.Beta[0] <= 0 {
		return 0
	}
	return 1.0 / m.Beta[0]
}

// PredictNext returns the expected indoor temperature (°C) one step of
// dtSeconds ahead, given the current indoor/outdoor temps and the heating
// power applied over the step. This is the function the flex-load
// scheduler rolls forward over the horizon to evaluate a setpoint plan.
func (m Model) PredictNext(indoorC, outdoorC, heatW, dtSeconds float64) float64 {
	a, b := m.effectiveCoef()
	rate := a*(outdoorC-indoorC) + b*heatW
	return indoorC + dtSeconds*rate
}

// HeatToHoldW returns the steady-state heating power (W, ≥0) needed to
// hold the zone at targetC against the given outdoor temperature — the
// power at which dT/dt = 0. Useful as a baseline for the scheduler.
func (m Model) HeatToHoldW(targetC, outdoorC float64) float64 {
	a, b := m.effectiveCoef()
	if b <= 0 {
		return 0
	}
	w := a * (targetC - outdoorC) / b
	if w < 0 {
		return 0
	}
	return w
}

// effectiveCoef blends learned β toward the prior during warmup so an
// RLS coefficient that hasn't settled yet can't produce a wild forecast.
func (m Model) effectiveCoef() (a, b float64) {
	trust := float64(m.Samples) / float64(WarmupSamples)
	if trust > 1 {
		trust = 1
	}
	a = trust*m.Beta[0] + (1-trust)*priorA
	b = trust*m.Beta[1] + (1-trust)*priorB
	// Physical guards: heat always leaks toward outdoor (a>0) and heating
	// never cools the zone (b≥0). RLS noise can briefly flip a sign.
	if a <= 0 {
		a = priorA
	}
	if b < 0 {
		b = 0
	}
	return a, b
}

// Update feeds one observed transition into the RLS estimator:
//
//	indoorC      — zone temperature at the START of the step
//	nextIndoorC  — zone temperature at the END of the step
//	outdoorC     — outdoor temperature over the step
//	heatW        — average heating power applied over the step (W, ≥0)
//	dtSeconds    — step length in seconds
//
// Returns true if the sample was accepted (false = rejected as an
// outlier or a degenerate/zero-length step).
func (m *Model) Update(indoorC, nextIndoorC, outdoorC, heatW, dtSeconds float64, nowMs int64) (updated bool) {
	if dtSeconds <= 0 {
		return false
	}
	// Reject implausible steps: a real building's indoor temp can't slew
	// more than a few °C per minute. Guards against sensor glitches and
	// gaps where dtSeconds is large but the reading jumped.
	ratePerMin := math.Abs(nextIndoorC-indoorC) / (dtSeconds / 60.0)
	if ratePerMin > 2.0 {
		return false
	}

	x := Features(outdoorC, indoorC, heatW)
	y := (nextIndoorC - indoorC) / dtSeconds // observed warming rate [°C/s]

	// Prediction error in rate space, then RLS gain.
	var pred float64
	for i := 0; i < NFeat; i++ {
		pred += m.Beta[i] * x[i]
	}
	rateErr := y - pred

	// Outlier rejection on the resulting per-step temperature error once
	// we have a few samples and an MAE band to compare against.
	stepErr := math.Abs(rateErr) * dtSeconds
	if m.Samples > 20 && m.MAE > 0 && stepErr > 10*m.MAE && stepErr > 0.5 {
		return false
	}

	lambda := m.Forgetting
	if lambda <= 0 {
		lambda = 0.997
	}

	// Px = P·x
	var Px [NFeat]float64
	for i := 0; i < NFeat; i++ {
		var s float64
		for j := 0; j < NFeat; j++ {
			s += m.P[i][j] * x[j]
		}
		Px[i] = s
	}
	// denom = λ + xᵀ·P·x
	denom := lambda
	for i := 0; i < NFeat; i++ {
		denom += x[i] * Px[i]
	}
	if denom == 0 {
		return false
	}
	// Gain K = Px / denom
	var K [NFeat]float64
	for i := 0; i < NFeat; i++ {
		K[i] = Px[i] / denom
	}
	// β += K·err
	for i := 0; i < NFeat; i++ {
		m.Beta[i] += K[i] * rateErr
	}
	// P = (P − K·(Px)ᵀ) / λ
	for i := 0; i < NFeat; i++ {
		for j := 0; j < NFeat; j++ {
			m.P[i][j] = (m.P[i][j] - K[i]*Px[j]) / lambda
		}
	}

	// Keep coefficients physical so a noisy update can't poison the model.
	if m.Beta[0] <= 0 {
		m.Beta[0] = priorA
	}
	if m.Beta[1] < 0 {
		m.Beta[1] = 0
	}

	// Maintain MAE as an EMA of the per-step temperature error.
	if m.MAE == 0 {
		m.MAE = stepErr
	} else {
		m.MAE = 0.95*m.MAE + 0.05*stepErr
	}
	m.Samples++
	m.LastMs = nowMs
	return true
}

// Quality returns a [0,1] confidence score the MPC can blend on, mirroring
// pvmodel.Quality: needs a minimum sample count and a tight error band.
func (m Model) Quality() float64 {
	if m.Samples < 30 {
		return float64(m.Samples) / 30.0 * 0.5 // ramp toward 0.5 during warmup
	}
	// MAE is per-step °C error; 0.05 °C is excellent, 0.5 °C is poor.
	switch {
	case m.MAE <= 0.05:
		return 1.0
	case m.MAE >= 0.5:
		return 0.0
	default:
		return 1.0 - (m.MAE-0.05)/0.45
	}
}

// AgeMs returns how long since the last accepted sample, for staleness
// checks by callers.
func (m Model) AgeMs(now time.Time) int64 {
	if m.LastMs == 0 {
		return math.MaxInt64
	}
	return now.UnixMilli() - m.LastMs
}
