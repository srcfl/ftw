package control

import (
	"encoding/json"
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// fuseHeatTau is the time constant of the main fuse's heat model.
const fuseHeatTau = 60 * time.Second

// fuseInstantFactor is the phase current, as a multiple of the fuse rating,
// above which the EV is cut at once with no heat budget. It stays below the
// 1.25 × In that a gG fuse of 16–63 A carries for an hour.
const fuseInstantFactor = 1.2

// fuseHeat models each phase of the main fuse as a first-order low-pass on
// I², fed only by the site meter's phase currents. A breaker and a fuse both
// trip on heat, not on one reading. FTW does not know which one a site has,
// so the EV hard limit holds the filtered current under the working limit,
// below both types' one-hour ratings (a miniature circuit breaker does not
// trip at 1.13 × In, a gG fuse does not blow at 1.25 × In), and gives no
// budget above fuseInstantFactor.
//
// A reading stands for the time until the next one. A meter gap counts at
// the last current seen: the reading after it neither fills the gap with its
// own current nor erases an overload from before it. A Core restart starts
// the model at the first reading's current, with no memory of the minutes
// before.
type fuseHeat struct {
	set  bool
	at   time.Time  // the last reading
	amps [3]float64 // its phase currents, held until the next
	sq   [3]float64 // filtered I² per phase up to at, A²
}

// step folds the time since the last reading into the model at that
// reading's currents, then holds the new ones.
func (h *fuseHeat) step(at time.Time, amps []float64) {
	if !h.set {
		for i, a := range amps {
			h.sq[i] = a * a
		}
	} else {
		dt := at.Sub(h.at)
		if dt <= 0 {
			return
		}
		k := -math.Expm1(-dt.Seconds() / fuseHeatTau.Seconds())
		for i, a := range h.amps {
			h.sq[i] += (a*a - h.sq[i]) * k
		}
	}
	h.set, h.at, h.amps = true, at, [3]float64{}
	copy(h.amps[:], amps)
}

// worstA is the highest filtered phase current.
func (h *fuseHeat) worstA() float64 {
	return math.Sqrt(max(h.sq[0], h.sq[1], h.sq[2]))
}

// stepFuseHeat folds each new site reading into the model once, at amps,
// whichever decision reads it first.
func (s *State) stepFuseHeat(meter *telemetry.DerReading, amps []float64) {
	if s.fuseHeat.set && !meter.UpdatedAt.After(s.fuseHeat.at) {
		return
	}
	s.fuseHeat.step(meter.UpdatedAt, amps)
}

// phaseCurrentsA reads the site's configured phases with SitePhaseCurrents.
// valid is also false when the fuse is not described.
func phaseCurrentsA(meter *telemetry.DerReading, state *State) (amps []float64, present, valid bool) {
	if !finiteFuseValue(state.SiteFuseAmps) || !finiteFuseValue(state.SiteFuseVoltage) || state.SiteFuseAmps <= 0 || state.SiteFuseVoltage <= 0 {
		_, present, _ = SitePhaseCurrents(meter, 3)
		return nil, present, false
	}
	return SitePhaseCurrents(meter, state.SiteFusePhases)
}

// SitePhaseCurrents reads a site reading's current on each of the first
// phases, import positive, as fuse protection reads it. A phase's direction
// is its power's sign in the same reading: P1 and many meters report current
// as a magnitude. Without that phase's power the current counts as import,
// the safe side. present is false when the reading has no phase currents;
// valid is false when any of the phases is missing or unreadable.
func SitePhaseCurrents(meter *telemetry.DerReading, phases int) (amps []float64, present, valid bool) {
	if meter == nil {
		return nil, false, false
	}
	// DER data uses snake case; separate fields retain absent-vs-zero.
	var data struct {
		L1  *float64 `json:"l1_a"`
		L2  *float64 `json:"l2_a"`
		L3  *float64 `json:"l3_a"`
		L1W *float64 `json:"l1_w"`
		L2W *float64 `json:"l2_w"`
		L3W *float64 `json:"l3_w"`
	}
	if json.Unmarshal(meter.Data, &data) != nil || (data.L1 == nil && data.L2 == nil && data.L3 == nil) {
		return nil, false, false
	}
	if phases < 1 || phases > 3 {
		return nil, true, false
	}
	powers := []*float64{data.L1W, data.L2W, data.L3W}
	for i, a := range []*float64{data.L1, data.L2, data.L3}[:phases] {
		if a == nil || !finiteFuseValue(*a) {
			return nil, true, false
		}
		signed := math.Abs(*a)
		if w := powers[i]; w != nil && finiteFuseValue(*w) && *w < 0 {
			signed = -signed
		}
		amps = append(amps, signed)
	}
	return amps, true, true
}
