// Package thermaltwin learns how a heated zone behaves, in shadow.
//
// One first-order model per zone, proposed in docs/adr/0012:
//
//	dT_in/dt = a·(T_out − T_in) + b·P + c
//
// a is 1/τ, the zone's inertia; b is how many kelvin one kWh of heat input
// moves it; c is what neither explains (sun, people, other heat). The heat
// loss UA follows as a/b. Learned online by recursive least squares over
// fixed windows.
//
// The twin only predicts. Nothing in Core plans or dispatches from it. It is
// judged the only way a model may be: against measured temperature, by
// simulating each three-hour stretch from the parameters it had at the
// start, fed the inputs that really happened. A prediction never verifies
// itself.
//
// A small error is not yet skill. In a house a thermostat holds still, "the
// temperature stays as it is" is as accurate as any model, and τ and the
// capacity cannot be learned there: outdoor temperature and heat input move
// together and the room does not move at all. So a twin is trusted only
// when it also beats that guess over the same stretches, and when the heat
// input varied within enough of them to tell the two terms apart.
package thermaltwin

import (
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/modelstate"
)

const (
	// Window is the length of one learning sample. Room sensors report in
	// 0.1 K steps and a zone moves a few tenths of a kelvin an hour, so a
	// shorter window would learn mostly quantisation.
	Window = 30 * time.Minute
	// EvalHorizonWindows is how far ahead a twin is judged: three hours,
	// the scale of a price peak the planner would heat ahead of.
	EvalHorizonWindows = 6

	windowH = 0.5

	// Forgetting factor. A half-life of about 350 windows, a week, follows
	// the season without losing a cold snap within days.
	lambda = 0.998
)

// Proposed thresholds, ADR 0012. Not used for control by anything yet.
const (
	minSamplesForTrust     = 7 * 48 // a week of windows
	minEvaluationsForTrust = 48     // a day of three-hour judgements
	trustMAEK              = 0.3
	driftMAEK              = 0.5
	maeMemory              = 24 // evaluations, one per window

	// Skill is judged over a week of evaluations, the span the twin needs
	// to learn at all: one still day must not cost a twin its standing, and
	// one lively day must not earn it.
	skillMemory = 7 * 48
	// The twin's three-hour error must be at most this share of the error
	// of assuming no change. At 1.0 a twin that predicts "no change" would
	// pass by a rounding step.
	skillVsPersistence = 0.8
	// Heat input varied within a stretch when its lowest window is at most
	// this share of its highest, and the highest is real heat rather than
	// standby. A pump that follows the cold moves a few per cent in three
	// hours; one that cycles, or is set back, moves far more.
	heatVariedRatio = 0.7
	heatVariedMinKW = 0.1
	// The share of judged stretches in which heat input must have varied.
	minHeatVariedShare = 0.2
)

// Status is what a twin can honestly say about itself.
type Status string

const (
	StatusNoHeatInput Status = "no_heat_input"
	StatusLearning    Status = "learning"
	// StatusUnproven is a twin whose error is within the limit but which
	// has not shown skill: it does not beat "no change", or heat input
	// varied too little to learn from. Nothing may plan from it.
	StatusUnproven Status = "unproven"
	StatusTrusted  Status = "trusted"
	StatusDrifting Status = "drifting"
)

// Sample is one closed window, as the service measured it.
type Sample struct {
	TInStartC float64 // zone temperature at the window's start
	TInEndC   float64 // zone temperature at the window's end
	TOutC     float64 // outdoor temperature over the window
	PowerKW   float64 // mean heat input over the window; 0 without a source
	// DurationH is the time between the two temperatures. Zero means a
	// whole window.
	DurationH float64
	// Contiguous is false when the previous window was not observed. A gap
	// breaks the three-hour simulations that span it.
	Contiguous bool
}

func (s Sample) durationH() float64 {
	if s.DurationH > 0 {
		return s.DurationH
	}
	return windowH
}

type pending struct {
	TInStartC float64    `json:"t_in_start_c"`
	TInEndC   float64    `json:"t_in_end_c"`
	TOutC     float64    `json:"t_out_c"`
	PowerKW   float64    `json:"power_kw"`
	DurationH float64    `json:"duration_h"`
	Theta     [3]float64 `json:"theta"`
}

// Twin is one zone's learned model. Its JSON is the persisted state.
type Twin struct {
	Theta [3]float64    `json:"theta"`
	P     [3][3]float64 `json:"p"`

	Samples      int64 `json:"samples"`
	PowerSamples int64 `json:"power_samples"`
	Rejected     int64 `json:"rejected"`

	MAE3hK      float64 `json:"mae_3h_k"`
	Evaluations int64   `json:"evaluations"`
	WasTrusted  bool    `json:"was_trusted"`

	// The same three-hour error over a week, beside the error of assuming
	// the temperature stays where the stretch began, and the share of those
	// stretches in which heat input varied.
	SkillMAE3hK       float64 `json:"skill_mae_3h_k"`
	PersistenceMAE3hK float64 `json:"persistence_mae_3h_k"`
	HeatVariedShare   float64 `json:"heat_varied_share"`

	// PowerSource names where heat input came from. A twin fitted to one
	// source means nothing against another, so a change starts it over.
	PowerSource string `json:"power_source,omitempty"`

	Pending []pending `json:"pending,omitempty"`
}

// New returns a twin at its prior: τ = 50 h, 20 kWh/K, no unexplained gain.
// The covariance is wide, so a week of data outweighs the prior.
func New() *Twin {
	t := &Twin{Theta: [3]float64{1.0 / 50, 1.0 / 20, 0}}
	t.P[0][0], t.P[1][1], t.P[2][2] = 0.01, 0.01, 0.25
	return t
}

// Observe learns from one window. It returns false, and learns nothing, for
// a window no physical zone produces: a sensor glitch, an open window, a
// missing input.
func (t *Twin) Observe(s Sample) bool {
	if !plausible(s) {
		t.Rejected++
		t.Pending = nil
		return false
	}
	if !s.Contiguous {
		t.Pending = nil
	}
	t.Pending = append(t.Pending, pending{
		TInStartC: s.TInStartC, TInEndC: s.TInEndC, TOutC: s.TOutC,
		PowerKW: s.PowerKW, DurationH: s.durationH(), Theta: t.Theta,
	})
	if len(t.Pending) >= EvalHorizonWindows {
		t.evaluate()
		t.Pending = t.Pending[1:]
	}

	x := regressor(s.TInStartC, s.TOutC, s.PowerKW)
	y := (s.TInEndC - s.TInStartC) / s.durationH()
	t.update(x, y)
	t.Samples++
	if s.PowerKW > 0 {
		t.PowerSamples++
	}
	// Records a first trust here, not only when someone reads the status:
	// a twin nobody looked at must still be able to drift.
	_ = t.Status()
	return true
}

func plausible(s Sample) bool {
	for _, v := range []float64{s.TInStartC, s.TInEndC, s.TOutC, s.PowerKW} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	switch {
	case s.TInStartC < -10 || s.TInStartC > 45 || s.TInEndC < -10 || s.TInEndC > 45:
		return false
	case s.TOutC < -50 || s.TOutC > 50:
		return false
	case s.PowerKW < 0 || s.PowerKW > 50:
		return false
	case s.DurationH < 0 || s.DurationH > 2*windowH:
		return false
	case math.Abs(s.TInEndC-s.TInStartC)/s.durationH() > 3:
		// Faster than any heated building: a window, a sensor in the sun, a
		// sensor moved.
		return false
	}
	return true
}

func regressor(tInC, tOutC, powerKW float64) [3]float64 {
	return [3]float64{tOutC - tInC, powerKW, 1}
}

// update is one recursive-least-squares step with forgetting.
func (t *Twin) update(x [3]float64, y float64) {
	var px [3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			px[i] += t.P[i][j] * x[j]
		}
	}
	denom := lambda
	for i := 0; i < 3; i++ {
		denom += x[i] * px[i]
	}
	if denom <= 0 || math.IsNaN(denom) || math.IsInf(denom, 0) {
		return
	}
	var k [3]float64
	for i := 0; i < 3; i++ {
		k[i] = px[i] / denom
	}
	errY := y - dot(t.Theta, x)
	next := t.Theta
	for i := 0; i < 3; i++ {
		next[i] += k[i] * errY
	}
	var nextP [3][3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			nextP[i][j] = (t.P[i][j] - k[i]*px[j]) / lambda
		}
	}
	// Keep P symmetric and bounded; an unbounded covariance is how RLS with
	// forgetting blows up during a long stretch of identical inputs.
	for i := 0; i < 3; i++ {
		for j := i + 1; j < 3; j++ {
			m := (nextP[i][j] + nextP[j][i]) / 2
			nextP[i][j], nextP[j][i] = m, m
		}
		if nextP[i][i] > 1e3 {
			return
		}
	}
	if !finiteVec(next) {
		return
	}
	t.Theta, t.P = next, nextP
}

// evaluate simulates the oldest pending window forward to the newest one's
// end, from the parameters the twin had when the stretch began.
func (t *Twin) evaluate() {
	first := t.Pending[0]
	temp := first.TInStartC
	for i, w := range t.Pending {
		if i > 0 {
			// The gap between one window's end and the next one's start
			// is the zone drifting with the inputs of the window before.
			prev := t.Pending[i-1]
			if gapH := windowH - prev.DurationH; gapH > 0 {
				temp += gapH * dot(first.Theta, regressor(temp, prev.TOutC, prev.PowerKW))
			}
		}
		temp += w.DurationH * dot(first.Theta, regressor(temp, w.TOutC, w.PowerKW))
	}
	measured := t.Pending[len(t.Pending)-1].TInEndC
	errK := math.Abs(temp - measured)
	if math.IsNaN(errK) || math.IsInf(errK, 0) {
		return
	}
	persistenceK := math.Abs(first.TInStartC - measured)
	minKW, maxKW := first.PowerKW, first.PowerKW
	for _, w := range t.Pending {
		minKW, maxKW = math.Min(minKW, w.PowerKW), math.Max(maxKW, w.PowerKW)
	}
	varied := 0.0
	if maxKW >= heatVariedMinKW && minKW <= heatVariedRatio*maxKW {
		varied = 1
	}
	t.Evaluations++
	week := math.Min(float64(t.Evaluations), skillMemory)
	t.SkillMAE3hK += (errK - t.SkillMAE3hK) / week
	t.PersistenceMAE3hK += (persistenceK - t.PersistenceMAE3hK) / week
	t.HeatVariedShare += (varied - t.HeatVariedShare) / week
	// A running mean at first, then about half a day of memory: long
	// enough that one odd evening does not cost trust, short enough that a
	// house that changed shows as drifting the same day.
	n := float64(t.Evaluations)
	if n > maeMemory {
		n = maeMemory
	}
	t.MAE3hK += (errK - t.MAE3hK) / n
}

// Params is the twin read as physics.
type Params struct {
	TauH            *float64 `json:"tau_h,omitempty"`
	CapacityKWhPerK *float64 `json:"capacity_kwh_per_k,omitempty"`
	UAWPerK         *float64 `json:"ua_w_per_k,omitempty"`
	OtherGainKPerH  float64  `json:"other_gain_k_per_h"`
}

// Params derives physical quantities. A non-positive coefficient has no
// physical reading and is left out rather than shown as nonsense.
func (t *Twin) Params() Params {
	a, b, c := t.Theta[0], t.Theta[1], t.Theta[2]
	p := Params{OtherGainKPerH: c}
	if a > 0 {
		tau := 1 / a
		p.TauH = &tau
	}
	if b > 0 && t.PowerSamples > 0 {
		cap := 1 / b
		p.CapacityKWhPerK = &cap
		if a > 0 {
			ua := 1000 * a / b
			p.UAWPerK = &ua
		}
	}
	return p
}

func (t *Twin) physical() bool {
	p := t.Params()
	return p.TauH != nil && *p.TauH >= 1 && *p.TauH <= 2000 &&
		p.CapacityKWhPerK != nil && *p.CapacityKWhPerK >= 1 && *p.CapacityKWhPerK <= 500
}

// skilled is whether the twin has shown it knows something: over the last
// week it beat "no change", in stretches where heat input varied.
func (t *Twin) skilled() bool {
	return t.SkillMAE3hK <= skillVsPersistence*t.PersistenceMAE3hK &&
		t.HeatVariedShare >= minHeatVariedShare
}

// Status reports the twin's standing and records a first trust, so later
// error above the drift limit reads as drifting rather than as learning.
func (t *Twin) Status() Status {
	if t.PowerSamples == 0 {
		return StatusNoHeatInput
	}
	if t.Samples < minSamplesForTrust || t.Evaluations < minEvaluationsForTrust || !t.physical() {
		if t.WasTrusted && t.Evaluations >= minEvaluationsForTrust {
			return StatusDrifting
		}
		return StatusLearning
	}
	limit := trustMAEK
	if t.WasTrusted {
		limit = driftMAEK
	}
	switch {
	case t.MAE3hK > limit && t.WasTrusted:
		return StatusDrifting
	case t.MAE3hK > limit:
		return StatusLearning
	case !t.skilled():
		return StatusUnproven
	}
	t.WasTrusted = true
	return StatusTrusted
}

// FeatureHash fingerprints the model's form, so stored state from a
// different form cold starts instead of restoring meaningless coefficients.
func FeatureHash() string {
	var probe []float64
	for _, v := range [][3]float64{{20, -5, 2}, {21.5, 10, 0}, {18, 0, 4.5}} {
		x := regressor(v[0], v[1], v[2])
		probe = append(probe, x[:]...)
	}
	probe = append(probe, windowH, lambda, EvalHorizonWindows, skillMemory)
	return modelstate.Fingerprint("thermaltwin: dT/dt K/h = a(Tout-Tin) + b*P_kW + c; heat input hp_power_w; judged against persistence", probe)
}

func dot(a, b [3]float64) float64 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }

func finiteVec(v [3]float64) bool {
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}
