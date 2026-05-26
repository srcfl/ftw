// Package control is the site's closed-loop control system. Pure Go port of
// the Rust src/control.rs logic, with the site sign convention applied
// throughout (no sign flips needed above the driver layer).
package control

// PIController is a minimal PI controller with anti-windup on the integral term.
// Port of the Rust `pid` crate's behavior for our use case. Kept tiny rather
// than importing a whole controller library.
//
//	error   = setpoint - measurement
//	output  = clamp(Kp·error + I,  -outputLimit, +outputLimit)
//	I      += Ki·error · dt        (clamped to ±iLimit after update)
type PIController struct {
	Setpoint float64

	Kp float64
	Ki float64

	// Integral windup guard
	IntegralLimit float64
	// Final output clamp
	OutputLimit float64

	integral float64
}

// NewPI creates a controller with the given gains and anti-windup limits.
// `outputLimit` caps the final correction; `iLimit` caps the integral term
// so it can't run away during saturation.
func NewPI(kp, ki, iLimit, outputLimit float64) *PIController {
	return &PIController{
		Kp:            kp,
		Ki:            ki,
		IntegralLimit: iLimit,
		OutputLimit:   outputLimit,
	}
}

// Update feeds a new measurement and returns the control output.
// Matches the `pid` crate semantics: error = setpoint - measurement.
func (p *PIController) Update(measurement float64) PIOutput {
	err := p.Setpoint - measurement
	pTerm := p.Kp * err
	p.integral += p.Ki * err
	// Clamp integral (anti-windup)
	if p.integral > p.IntegralLimit {
		p.integral = p.IntegralLimit
	} else if p.integral < -p.IntegralLimit {
		p.integral = -p.IntegralLimit
	}
	out := pTerm + p.integral
	if out > p.OutputLimit {
		out = p.OutputLimit
	} else if out < -p.OutputLimit {
		out = -p.OutputLimit
	}
	return PIOutput{P: pTerm, I: p.integral, Output: out, Error: err}
}

// Reset zeroes the integral. Use when retuning or after a long idle.
func (p *PIController) Reset() { p.integral = 0 }

// DecayIntegral multiplies the integral by factor (0 ≤ factor ≤ 1).
// Use when the controller is observed to be commanding the wrong
// direction relative to live error sign — the accumulated integral is
// load-bearing in the wrong direction and clamping the output alone
// leaves it stuck. A factor in the 0.3–0.6 range unwinds saturation
// over a few cycles without dropping memory of legitimate offsets.
//
// Conditional integration (skip-on-clamp) would be cleaner in theory
// but doesn't help our specific case: the dispatch wrong-direction
// clamp only catches the IS-wrong condition after the integral has
// already saturated. Active decay is what gets us out.
func (p *PIController) DecayIntegral(factor float64) {
	if factor < 0 {
		factor = 0
	}
	if factor > 1 {
		factor = 1
	}
	p.integral *= factor
}

// Integral exposes the current integral term. Read-only outside the
// controller — set/reset through Reset / DecayIntegral.
func (p *PIController) Integral() float64 { return p.integral }

// PIOutput is one update's full breakdown.
type PIOutput struct {
	P, I, Output, Error float64
}
