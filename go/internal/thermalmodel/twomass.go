package thermalmodel

import (
	"math"
	"time"
)

// TwoMass is a 2R2C thermal model for floor heating, where two very
// different thermal masses matter:
//
//   - the SLAB (concrete floor + screed) is heated directly by the element
//     or the hydronic loop. It's large and slow — it banks heat for hours.
//   - the ROOM AIR is heated by the slab and loses heat to the outdoors.
//     It's lighter and faster.
//
// The single-mass Model lumps these together and so badly underestimates how
// long a floor-heated room stays warm after the element switches off (the
// slab keeps radiating). That matters for the pause/block economics: the
// true coast time is much longer than a 1R1C fit predicts. The 2R2C model
// captures it:
//
//	Cs·dTs/dt = P              − (Ts−Tr)/Rsr           (slab)
//	Cr·dTr/dt = (Ts−Tr)/Rsr    − (Tr−To)/Rro           (room)
//
// Discretised (dt seconds), as two independent linear regressions:
//
//	slab:  (Ts[k+1]−Ts[k])/dt = aRS·(Tr−Ts) + bP·P
//	room:  (Tr[k+1]−Tr[k])/dt = aSR·(Ts−Tr) + aRO·(To−Tr)
//
// where aRS=1/(Rsr·Cs), bP=1/Cs, aSR=1/(Rsr·Cr), aRO=1/(Rro·Cr). Each
// regression is fit by the same RLS used elsewhere. The slab temperature Ts
// comes from a floor probe (common on electric floor thermostats) or the
// hydronic flow temperature as a proxy.
type TwoMass struct {
	// Slab dynamics: y = aRS·(Tr−Ts) + bP·P ; beta = [aRS, bP]
	BetaSlab [2]float64    `json:"beta_slab"`
	PSlab    [2][2]float64 `json:"p_slab"`
	// Room dynamics: y = aSR·(Ts−Tr) + aRO·(To−Tr) ; beta = [aSR, aRO]
	BetaRoom [2]float64    `json:"beta_room"`
	PRoom    [2][2]float64 `json:"p_room"`

	Forgetting float64 `json:"forgetting"`
	Samples    int64   `json:"samples"`
	LastMs     int64   `json:"last_ms"`
	MAESlab    float64 `json:"mae_slab"` // EMA of |Δslab err| per step (°C)
	MAERoom    float64 `json:"mae_room"` // EMA of |Δroom err| per step (°C)
}

// Physical priors.
//
//	slab→room coupling τ ≈ 1 h  ⇒ aSR ≈ 1/3600
//	room→outdoor      τ ≈ 4 h  ⇒ aRO ≈ 1/14400
//	room→slab coupling τ ≈ 2 h ⇒ aRS ≈ 1/7200  (slab is the bigger mass)
//	bP = 1/Cs: a 2 kW element over 15 min lifts a room's slab ~0.18 °C
//	  ⇒ bP ≈ 0.18/(2000·900) ≈ 1.0e-7 °C/(W·s)
const (
	priorASR = 1.0 / 3600.0
	priorARO = 1.0 / 14400.0
	priorARS = 1.0 / 7200.0
	priorBP  = 1.0e-7
)

// NewTwoMass returns a model anchored on the physical priors.
func NewTwoMass() *TwoMass {
	m := &TwoMass{Forgetting: 0.997}
	for i := 0; i < 2; i++ {
		m.PSlab[i][i] = 1.0
		m.PRoom[i][i] = 1.0
	}
	m.BetaSlab = [2]float64{priorARS, priorBP}
	m.BetaRoom = [2]float64{priorASR, priorARO}
	return m
}

// effSlab / effRoom blend learned β toward the priors during warmup and keep
// the coefficients physical (all couplings ≥ 0).
func (m TwoMass) effSlab() (aRS, bP float64) {
	trust := m.trust()
	aRS = trust*m.BetaSlab[0] + (1-trust)*priorARS
	bP = trust*m.BetaSlab[1] + (1-trust)*priorBP
	if aRS < 0 {
		aRS = priorARS
	}
	if bP < 0 {
		bP = 0
	}
	return
}

func (m TwoMass) effRoom() (aSR, aRO float64) {
	trust := m.trust()
	aSR = trust*m.BetaRoom[0] + (1-trust)*priorASR
	aRO = trust*m.BetaRoom[1] + (1-trust)*priorARO
	if aSR < 0 {
		aSR = priorASR
	}
	if aRO <= 0 {
		aRO = priorARO
	}
	return
}

func (m TwoMass) trust() float64 {
	t := float64(m.Samples) / float64(WarmupSamples)
	if t > 1 {
		t = 1
	}
	return t
}

// PredictStep advances both states one step of dtSeconds. heatW is the
// thermal power delivered to the slab.
func (m TwoMass) PredictStep(roomC, slabC, outdoorC, heatW, dtSeconds float64) (nextRoomC, nextSlabC float64) {
	aRS, bP := m.effSlab()
	aSR, aRO := m.effRoom()
	dSlab := aRS*(roomC-slabC) + bP*heatW
	dRoom := aSR*(slabC-roomC) + aRO*(outdoorC-roomC)
	return roomC + dtSeconds*dRoom, slabC + dtSeconds*dSlab
}

// ForecastRoom rolls the model forward over the supplied per-step heat and
// outdoor series, returning the predicted ROOM temperature after each step.
// This is the floor-heating forecast the scheduler reasons over. The series
// must be the same length; dtSeconds is the step.
func (m TwoMass) ForecastRoom(roomC, slabC float64, heatW, outdoorC []float64, dtSeconds float64) []float64 {
	n := len(heatW)
	if len(outdoorC) < n {
		n = len(outdoorC)
	}
	out := make([]float64, 0, n)
	r, s := roomC, slabC
	for i := 0; i < n; i++ {
		r, s = m.PredictStep(r, s, outdoorC[i], heatW[i], dtSeconds)
		out = append(out, r)
	}
	return out
}

// CoastHoursToRoomTarget returns how many hours the ROOM stays at or above
// targetC with no heating, starting from the given room+slab temperatures.
// Unlike the single-mass version, the slab keeps feeding the room as it
// cools, so this is materially longer for floor heating — and far more
// accurate. cap bounds the search.
func (m TwoMass) CoastHoursToRoomTarget(roomC, slabC, targetC, outdoorC float64, cap time.Duration) float64 {
	if roomC <= targetC {
		return 0
	}
	const step = 300.0
	maxS := cap.Seconds()
	t := 0.0
	r, s := roomC, slabC
	for t < maxS {
		r, s = m.PredictStep(r, s, outdoorC, 0, step)
		t += step
		if r <= targetC {
			break
		}
	}
	return t / 3600.0
}

// Update folds one observed transition into both regressions. It needs the
// slab temperature (floor probe or flow temp) at the start and end of the
// step alongside the room temperature.
func (m *TwoMass) Update(roomC, nextRoomC, slabC, nextSlabC, outdoorC, heatW, dtSeconds float64, nowMs int64) bool {
	if dtSeconds <= 0 {
		return false
	}
	// Reject implausible slews (sensor glitches).
	if math.Abs(nextRoomC-roomC)/(dtSeconds/60.0) > 2.0 ||
		math.Abs(nextSlabC-slabC)/(dtSeconds/60.0) > 4.0 {
		return false
	}
	lambda := m.Forgetting
	if lambda <= 0 {
		lambda = 0.997
	}

	// Slab regression: y = aRS·(Tr−Ts) + bP·P
	xs := [2]float64{roomC - slabC, heatW}
	ys := (nextSlabC - slabC) / dtSeconds
	errSlab := rls2(&m.BetaSlab, &m.PSlab, xs, ys, lambda)
	if m.BetaSlab[0] < 0 {
		m.BetaSlab[0] = priorARS
	}
	if m.BetaSlab[1] < 0 {
		m.BetaSlab[1] = 0
	}

	// Room regression: y = aSR·(Ts−Tr) + aRO·(To−Tr)
	xr := [2]float64{slabC - roomC, outdoorC - roomC}
	yr := (nextRoomC - roomC) / dtSeconds
	errRoom := rls2(&m.BetaRoom, &m.PRoom, xr, yr, lambda)
	if m.BetaRoom[0] < 0 {
		m.BetaRoom[0] = priorASR
	}
	if m.BetaRoom[1] <= 0 {
		m.BetaRoom[1] = priorARO
	}

	stepSlabErr := math.Abs(errSlab) * dtSeconds
	stepRoomErr := math.Abs(errRoom) * dtSeconds
	m.MAESlab = ema(m.MAESlab, stepSlabErr)
	m.MAERoom = ema(m.MAERoom, stepRoomErr)
	m.Samples++
	m.LastMs = nowMs
	return true
}

// TauRoomSeconds / TauSlabSeconds expose the learned time constants.
func (m TwoMass) TauRoomSeconds() float64 {
	_, aRO := m.effRoom()
	if aRO <= 0 {
		return 0
	}
	return 1.0 / aRO
}

func (m TwoMass) TauSlabSeconds() float64 {
	aRS, _ := m.effSlab()
	if aRS <= 0 {
		return 0
	}
	return 1.0 / aRS
}

// Quality mirrors Model.Quality: confidence in [0,1] from sample count and
// the room-temperature error band (the quantity the scheduler acts on).
func (m TwoMass) Quality() float64 {
	if m.Samples < 30 {
		return float64(m.Samples) / 30.0 * 0.5
	}
	switch {
	case m.MAERoom <= 0.05:
		return 1.0
	case m.MAERoom >= 0.5:
		return 0.0
	default:
		return 1.0 - (m.MAERoom-0.05)/0.45
	}
}

// rls2 performs one RLS update of a 2-feature regression in place and returns
// the prediction error (y − β·x) in rate space.
func rls2(beta *[2]float64, P *[2][2]float64, x [2]float64, y, lambda float64) float64 {
	var pred float64
	for i := 0; i < 2; i++ {
		pred += beta[i] * x[i]
	}
	e := y - pred
	// Px = P·x
	var Px [2]float64
	for i := 0; i < 2; i++ {
		Px[i] = P[i][0]*x[0] + P[i][1]*x[1]
	}
	denom := lambda + x[0]*Px[0] + x[1]*Px[1]
	if denom < 1e-9 {
		return e
	}
	var K [2]float64
	K[0] = Px[0] / denom
	K[1] = Px[1] / denom
	beta[0] += K[0] * e
	beta[1] += K[1] * e
	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			P[i][j] = (P[i][j] - K[i]*Px[j]) / lambda
		}
	}
	return e
}

func ema(prev, sample float64) float64 {
	if prev == 0 {
		return sample
	}
	return 0.95*prev + 0.05*sample
}
