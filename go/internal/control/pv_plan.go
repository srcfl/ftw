package control

import (
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"math"
	"time"
)

// Aggregate min(PV, cap) requires one verified control domain covering all PV.
// Independent inverter caps need per-source forecasts before they can qualify.
func PlanningPVCurtailment(state *State, store *telemetry.Store, options telemetry.ForecastOptions) mpc.PVCurtailment {
	if state == nil || store == nil || state.PVGenerationLimit == nil {
		return mpc.PVCurtailment{}
	}
	if !store.ForecastMeasurement(state.now(), state.SiteMeterDriver, options).PVValid {
		return mpc.PVCurtailment{}
	}
	expected := map[string]bool{}
	for _, f := range options.ExpectedFlows {
		if f.DerType == telemetry.DerPV {
			expected[f.Driver] = true
		}
	}
	if len(expected) != 1 {
		return mpc.PVCurtailment{}
	}
	for id := range expected {
		proof := state.PVGenerationLimit(id)
		if proof.Driver == id && proof.Valid() && planningPVProofValid(state, store, proof) {
			return proof
		}
	}
	return mpc.PVCurtailment{}
}

func planningPVProofValid(state *State, store *telemetry.Store, proof mpc.PVCurtailment) bool {
	if state == nil || store == nil || !proof.Valid() || state.PVGenerationLimit == nil || !state.SupportsPVCurtail[proof.Driver] {
		return false
	}
	return PVGenerationProofValid(store, state.now(), proof, state.PVGenerationLimit)
}

// PVGenerationProofValid takes a bounded registry lookup and no control-state
// lock. Service uses it after taking one plan/params snapshot, for all consumers.
func PVGenerationProofValid(store *telemetry.Store, now time.Time, proof mpc.PVCurtailment, lookup func(string) mpc.PVCurtailment) bool {
	if store == nil || !proof.Valid() || lookup == nil || lookup(proof.Driver) != proof {
		return false
	}
	sources := store.ReadingsByType(telemetry.DerPV)
	if len(sources) != 1 || sources[0].Driver != proof.Driver {
		return false
	}
	r := sources[0]
	h := store.DriverHealth(proof.Driver)
	return h != nil && h.IsOnline() && !r.UpdatedAt.After(now) && now.Sub(r.UpdatedAt) <= 90*time.Second && !math.IsNaN(r.RawW) && !math.IsInf(r.RawW, 0)
}

// Check the proof before exposing any energy directive from a plan that used
// PV control. Losing the driver/config/generation also revokes battery/EV use.
func PlanningPVDirectiveValid(state *State, store *telemetry.Store, dir SlotDirective) bool {
	if dir.PVCurtailment.Proof == "" {
		return !dir.PVCurtailActive
	}
	return planningPVProofValid(state, store, dir.PVCurtailment)
}

func plannedPVCaps(state *State, store *telemetry.Store, dir SlotDirective, limit float64) (map[string]float64, bool) {
	p := dir.PVCurtailment
	if !dir.PVCurtailActive || !planningPVProofValid(state, store, p) || math.IsNaN(limit) || math.IsInf(limit, 0) || limit < p.MinW || limit > p.MaxW {
		return nil, false
	}
	// The worker proposes whole watts. A stronger manual/protective ceiling may
	// be fractional; rounding down preserves that ceiling and the safe minimum.
	limit = math.Floor(limit + 1e-7)
	if limit < p.MinW {
		return nil, false
	}
	return map[string]float64{p.Driver: limit}, true
}
