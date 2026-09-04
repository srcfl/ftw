package mpc

import "github.com/srcfl/ftw/go/internal/loadpoint"

// LoadpointSpec tells the DP how to extend its state space with an EV
// loadpoint. Set `Params.Loadpoint` to a non-nil spec to have the
// optimizer treat charging the EV as a decision variable alongside
// battery action. Leave nil to preserve the legacy battery-only DP.
//
// Design choices:
//
//   - The action set is DISCRETE (AllowedStepsW) rather than continuous.
//     EVs have real disjunctive constraints — a 1-phase charger jumps
//     {0, 1.4, 2.3} kW but not 3.5 kW; a 3-phase jumps to 4.1+ only.
//     LP/MILP would need binary variables; DP just enumerates the
//     allowed levels and the infeasible gap between 1-phase and
//     3-phase minima is handled for free.
//
//   - Only charging is modeled — no V2G. Our current chargers
//     (Easee, Zap) don't discharge to the grid, and including V2G
//     would double the action dimension.
//
//   - Target SoC + deadline are honored via a linearly decaying
//     terminal penalty in the DP (see optimize.go). This is a
//     "lexicographic fallback" analogue: prefer meeting target, but
//     when infeasible, maximize delivered energy instead of
//     returning no plan.
type LoadpointSpec struct {
	ID string // matches loadpoint.Config.ID for dispatch routing

	// Vehicle battery capacity (Wh). Drives SoC% ↔ Wh conversion.
	CapacityWh float64

	// SoC grid. Coarser than battery (11 is typical — EV loads are
	// lumpy anyway).
	Levels int
	SoCMin float64 // usually 0
	SoCMax float64 // usually 1

	// Plan-start conditions.
	InitialSoC float64 // EV SoC at the first slot
	PluggedIn  bool    // when false, Optimize treats the loadpoint as absent

	// User intent. A zero target means no deadline — charge
	// opportunistically based on price/PV surplus only.
	TargetSoC     float64
	TargetSlotIdx int // zero-based slot by whose end the target must be met; ignored when target is zero

	// Electrical constraints. AllowedStepsW MUST include 0 (off) and
	// should enumerate the discrete charger power levels. If empty,
	// defaults to {0, MaxChargeW}.
	MaxChargeW    float64
	AllowedStepsW []float64

	// Charge-side efficiency (AC → battery). Typical 0.90 for a
	// modern 3-phase EV charger. 0 defaults to 0.90.
	ChargeEfficiency float64

	// SurplusOnly forbids EV actions that need grid import or home-battery
	// discharge into the car. The loadpoint may take at most the PV leftover
	// after house load. Site import caused by a simultaneous home-battery
	// grid-charge does not count as the car importing — surplus-only is an
	// EV policy, not a site import ban. Battery discharge still cannot be
	// treated as synthetic PV surplus, even when BatteryCoversEV is on.
	SurplusOnly bool

	// NoBatteryToEV mirrors ctrl.State.BatteryCoversEV inverted: when
	// true (operator's default), the home battery's discharge MUST NOT
	// end up at the EV. Enforced by loadpoint.BatteryDischargeFeedsEV
	// in the DP and in ValidatePlan. The runtime clamp in
	// control/dispatch.go (search CANONICAL "battery may not feed EV")
	// is the same conservation rule, not this helper — dispatch.go is
	// owned by a separate PV-export PR.
	NoBatteryToEV bool
}

func (l *LoadpointSpec) blocksBatteryToEV() bool {
	return l != nil && (l.NoBatteryToEV || l.SurplusOnly)
}

// surplusOnlyExceedsHousePV is the planner name for the site-power
// leftover check. Surplus-only is an EV policy: leftover PV after the
// house, not a ban on site import while the car is charging.
func surplusOnlyExceedsHousePV(evW, loadW, pvW float64) bool {
	return loadpoint.SurplusOnlyExceedsHousePV(evW, loadW, pvW)
}

// normalizedSteps returns a non-nil, 0-included, dedup'd + sorted
// action set. Used internally by the DP.
func (l *LoadpointSpec) normalizedSteps() []float64 {
	if l == nil {
		return nil
	}
	if len(l.AllowedStepsW) == 0 {
		if l.MaxChargeW <= 0 {
			return []float64{0}
		}
		return []float64{0, l.MaxChargeW}
	}
	seen := map[float64]struct{}{0: {}}
	out := []float64{0}
	for _, s := range l.AllowedStepsW {
		if s < 0 {
			continue
		}
		if l.MaxChargeW > 0 && s > l.MaxChargeW {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	// Bubble-insertion sort (few elements, clarity > performance).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// active reports whether the DP should include EV dimensions for
// this spec. Nil or un-plugged = inactive; treat as pure battery.
func (l *LoadpointSpec) active() bool {
	return l != nil && l.PluggedIn && l.CapacityWh > 0 && l.Levels >= 2
}
