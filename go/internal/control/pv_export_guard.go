package control

import (
	"math"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// reactivePVCurtailFloorW is the smallest cap the live export guard may send.
// The current driver contract cannot distinguish an active 0 W cap from a
// release, and Ferroamp's MQTT path treats a literal zero as a sticky stop.
// Keep this strictly above curtailMinPerDriverW until the contract carries an
// explicit active bit and each driver declares its safe minimum.
const reactivePVCurtailFloorW = 2.0

type pvExportGuardDecision struct {
	active    bool
	caps      map[string]float64
	exportW   float64
	ceilingW  float64
	overageW  float64
	residualW float64
}

// computePVExportGuard reduces online, curtail-capable PV when the fresh site
// meter shows solar-attributable export above the same effective ceiling used
// by the battery fuse guard. main.go calls ComputePVCurtail only after the
// shared site-meter freshness gate has passed.
//
// This is deliberately a partial guard. It never sends an ambiguous zero-watt
// command. Any reduction below the safe positive floor, or export from PV that
// cannot be curtailed, remains in residualW so the breach is not hidden.
func computePVExportGuard(state *State, store *telemetry.Store) pvExportGuardDecision {
	var out pvExportGuardDecision
	if state == nil || store == nil {
		return out
	}

	fuseMaxW := state.siteFuseMaxW()
	if fuseMaxW <= 0 {
		return out
	}
	out.ceilingW = state.effectiveExportCeilingW(fuseMaxW)
	out.exportW = solarSurplusW(state, store)
	if out.exportW <= out.ceilingW {
		return out
	}
	out.active = true
	out.overageW = out.exportW - out.ceilingW

	type candidate struct {
		driver      string
		generationW float64
		reducibleW  float64
	}
	var candidates []candidate
	var totalReducibleW float64
	for _, reading := range store.ReadingsByType(telemetry.DerPV) {
		health := store.DriverHealth(reading.Driver)
		if health == nil || !health.IsOnline() || !state.SupportsPVCurtail[reading.Driver] {
			continue
		}
		generationW := -reading.SmoothedW
		reducibleW := generationW - reactivePVCurtailFloorW
		if reducibleW <= 0 {
			continue
		}
		candidates = append(candidates, candidate{
			driver:      reading.Driver,
			generationW: generationW,
			reducibleW:  reducibleW,
		})
		totalReducibleW += reducibleW
	}

	reductionW := math.Min(out.overageW, totalReducibleW)
	if reductionW <= 0 || totalReducibleW <= 0 {
		return out
	}

	out.caps = make(map[string]float64, len(candidates))
	for _, c := range candidates {
		shareW := reductionW * (c.reducibleW / totalReducibleW)
		limitW := c.generationW - shareW
		if limitW < reactivePVCurtailFloorW {
			limitW = reactivePVCurtailFloorW
		}
		out.caps[c.driver] = limitW
	}
	return out
}

// pvExportResidualAfterCaps predicts the solar overage left after every safe
// positive cap selected for this tick, including a planner or manual cap that
// was already tighter than the live guard. It does not claim a driver applied
// the command; command refusal remains the actuation tracker's concern.
func pvExportResidualAfterCaps(
	overageW float64,
	state *State,
	store *telemetry.Store,
	caps map[string]float64,
) float64 {
	if overageW <= 0 || state == nil || store == nil {
		return 0
	}
	var reductionW float64
	for _, reading := range store.ReadingsByType(telemetry.DerPV) {
		health := store.DriverHealth(reading.Driver)
		if health == nil || !health.IsOnline() || !state.SupportsPVCurtail[reading.Driver] {
			continue
		}
		limitW, ok := caps[reading.Driver]
		if !ok || limitW <= curtailMinPerDriverW {
			continue
		}
		generationW := -reading.SmoothedW
		if generationW > limitW {
			reductionW += generationW - limitW
		}
	}
	residualW := overageW - reductionW
	if residualW < 0 {
		return 0
	}
	return residualW
}
