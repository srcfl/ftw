package control

import (
	"encoding/json"
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// phaseImbalanceA is how far the worst phase sits above the mean phase
// current of one meter sample. The aggregate fuse budget assumes balanced
// phases, but the breaker, and a charger's own load balancer, act on the
// worst phase. A three-phase battery or charger adds the same current to
// every phase, so this gap is import the aggregate budget cannot use.
//
// Meters report phase magnitudes, so the mean uses |aggregate|. Phases
// flowing in opposite directions make the gap look smaller than it is; the
// per-phase fuse guards still act on a real overage. A single-phase battery
// or charger is outside this model, as it is for perPhaseReliefW.
func phaseImbalanceA(r *telemetry.DerReading, state *State) (float64, bool) {
	if state == nil || state.SiteFuseAmps <= 0 || state.SiteFuseVoltage <= 0 || state.SiteFusePhases <= 1 {
		return 0, false
	}
	if r == nil || len(r.Data) == 0 {
		return 0, false
	}
	var d struct {
		L1A *float64 `json:"l1_a"`
		L2A *float64 `json:"l2_a"`
		L3A *float64 `json:"l3_a"`
	}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return 0, false
	}
	phaseAmps := [...]*float64{d.L1A, d.L2A, d.L3A}
	phases := min(state.SiteFusePhases, len(phaseAmps))
	worst := 0.0
	for i := 0; i < phases; i++ {
		p := phaseAmps[i]
		if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
			return 0, false
		}
		worst = math.Max(worst, math.Abs(*p))
	}
	if math.IsNaN(r.RawW) || math.IsInf(r.RawW, 0) {
		return 0, false
	}
	mean := math.Abs(r.RawW) / (state.SiteFuseVoltage * float64(phases))
	return math.Max(0, worst-mean), true
}

// phaseImportCeilingW is the import ceiling that keeps the worst phase
// inside the fuse less its margin. A tariff peak below it still binds, but
// the imbalance is never taken off the peak: that is a billing total, not a
// breaker. Never above effectiveImportCeilingW.
func (s *State) phaseImportCeilingW(fuseMaxW float64, meter *telemetry.DerReading) float64 {
	ceiling := s.effectiveImportCeilingW(fuseMaxW)
	imbalanceA, ok := phaseImbalanceA(meter, s)
	if !ok || imbalanceA <= 0 {
		return ceiling
	}
	phaseBudget := fuseMaxW - s.fuseSafetyMarginW() - imbalanceA*s.SiteFuseVoltage*float64(s.SiteFusePhases)
	return math.Max(0, math.Min(ceiling, phaseBudget))
}

// PhaseImbalanceW is the live phase gap as aggregate watts: import the
// planner must leave unused so the worst phase stays inside the fuse. Zero
// when the meter reports no phase currents or its sample is older than
// maxAge. Caller holds the control mutex.
func PhaseImbalanceW(store *telemetry.Store, state *State, maxAge time.Duration, now time.Time) float64 {
	if store == nil || state == nil || state.SiteMeterDriver == "" {
		return 0
	}
	r := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	if r == nil || now.Sub(r.UpdatedAt) > maxAge {
		return 0
	}
	imbalanceA, ok := phaseImbalanceA(r, state)
	if !ok {
		return 0
	}
	return imbalanceA * state.SiteFuseVoltage * float64(state.SiteFusePhases)
}
