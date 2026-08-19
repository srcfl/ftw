package mpc

import "math"

// PVBand returns an uncertainty band around a slot's expected PV power.
//
// Everything here is in the site convention: power into the site is positive,
// so generation is NEGATIVE. That inverts the intuitive reading of the return
// values, which is exactly why this lives in one tested function instead of
// being open-coded at each call site:
//
//   - low  is the numerically smaller (more negative) bound — MORE generation,
//     i.e. the OPTIMISTIC case.
//   - high is the numerically larger bound (closer to zero) — LESS generation,
//     i.e. the PESSIMISTIC case.
//
// The optimizer validates `low <= base <= high <= 0` and rejects the request
// outright if any of that fails, so two clamps are load-bearing: high can never
// cross zero (a spread wider than expected generation degrades to "produces
// nothing", never to positive PV, which would be read as load), and a
// non-positive spread collapses the band onto the base forecast exactly.
//
// Slots with no expected generation return a zero-width band at zero, matching
// how night slots are left untouched.
func PVBand(basePVW, spreadW float64) (low, high float64) {
	// `!(x < 0)` rather than `x >= 0` so NaN falls into this branch too.
	if !(basePVW < 0) || math.IsInf(basePVW, 0) {
		return 0, 0
	}
	if !(spreadW > 0) || math.IsInf(spreadW, 0) {
		return basePVW, basePVW
	}
	generation := -basePVW // > 0
	low = -(generation + spreadW)
	high = -math.Max(0, generation-spreadW)
	return low, high
}
