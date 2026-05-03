// Package mpc is a receding-horizon energy scheduler. It turns forecast
// prices + forecast PV + current SoC into an optimal battery power
// schedule by running dynamic programming over a discretized SoC grid.
//
// We deliberately avoid an LP/QP solver here:
//   - DP is exact over the quantization grid
//   - No dependencies, one file of pure Go
//   - Easy to explain + audit
//   - Fast enough for horizons up to ~100 slots × 50 SoC × 50 actions
//     (~250k state evaluations — under 50ms on any modern CPU)
//
// Site sign convention (same as the rest of the codebase):
//
//	grid_w  > 0 → importing (paying)
//	grid_w  < 0 → exporting
//	pv_w    < 0 → PV generating into site
//	battery > 0 → charging (load on site)
//	battery < 0 → discharging (source on site)
//
// Power balance per slot (from the grid meter's point of view):
//
//	grid_w = load_w + pv_w + battery_w
//
// Battery efficiency: the `battery_w` we command is measured at the AC
// terminals (site-facing). Due to conversion losses, only a fraction
// actually lands in (or comes out of) the cells:
//
//	charging   (battery_w > 0):  ΔSoC_kWh = +battery_w × dt × charge_eff
//	discharging(battery_w < 0):  ΔSoC_kWh = +battery_w × dt / discharge_eff
//
// So a 1000W charge command with 95% efficiency adds 950Wh/h to SoC. A
// 1000W discharge command with 95% efficiency drains ~1053Wh/h from SoC.
// Round-trip = charge_eff × discharge_eff (typically ~0.90).
package mpc

import (
	"math"
	"strings"
	"time"
)

// Mode selects how aggressively the planner uses the battery.
type Mode string

const (
	// ModeSelfConsumption: only use the battery to cover local load or
	// absorb PV surplus. Never import to charge; never export to discharge.
	// Matches the behavior of the base control loop (no planning needed).
	ModeSelfConsumption Mode = "self_consumption"

	// ModeCheapCharge: allow importing to charge when prices are low
	// (the DP decides based on forecast). Still never export battery to
	// grid — discharge stays ≤ local load.
	ModeCheapCharge Mode = "cheap_charge"

	// ModeArbitrage: unrestricted. Charge from grid, discharge to grid —
	// whatever minimizes total cost over the horizon, subject to SoC and
	// power limits.
	ModeArbitrage Mode = "arbitrage"
)

// IdleGateThresholdW is the per-slot average battery power below which the
// plan is treated as "idle this slot" by the planner_self control branch.
// Mirrors the chargeThresh used by reasonFor — a slot averaging less than
// this much battery action is interpreted as the DP declining to
// participate (e.g. saving surplus for a later slot with a richer price/PV
// mix). The control package duplicates this constant (same import-avoidance
// trick as SlotDirective) — keep the two in sync.
const IdleGateThresholdW = 100.0

// Slot is one input time slot for the optimizer.
type Slot struct {
	StartMs  int64
	LenMin   int
	PriceOre float64 // total consumer öre/kWh (incl. grid + VAT) — used for IMPORT cost
	SpotOre  float64 // raw spot öre/kWh — used for EXPORT revenue (before bonus/fee)
	PVW      float64 // negative (site sign). 0 if no forecast.
	LoadW    float64 // positive (site sign). Defaults to a flat baseline.

	// Confidence in [0, 1]. 1.0 = real day-ahead price; < 1.0 = ML-
	// forecasted price where we're less sure of both level and shape.
	// The DP blends low-confidence prices toward the horizon mean so
	// the planner doesn't over-commit to uncertain spikes. Defaults to
	// 1.0 when callers leave it zero.
	Confidence float64

	// Limits caps grid flow for this slot. Zero value = unlimited.
	// See power_limits.go for use cases (peak-tariff capacity, DSO
	// curtailment, service-entrance current limit).
	Limits PowerLimits
}

// Params bounds the optimization. All fields are required.
type Params struct {
	Mode Mode

	// SoC grid
	SoCLevels     int     // e.g. 41 (2.5% steps)
	CapacityWh    float64 // aggregate battery capacity
	SoCMinPct     float64 // e.g. 10
	SoCMaxPct     float64 // e.g. 95
	InitialSoCPct float64

	// Action grid (+charge, −discharge; site sign)
	ActionLevels  int     // odd number preferred so 0 is represented (e.g. 21)
	MaxChargeW    float64 // ≥ 0
	MaxDischargeW float64 // ≥ 0 (magnitude)

	// Efficiency (0..1). Default 0.95 each → ~90% round-trip.
	ChargeEfficiency    float64
	DischargeEfficiency float64

	// Terminal valuation. If > 0, we credit the plan with
	// TerminalSoCPrice × remaining_kwh at the final slot. Prevents the
	// planner from always ending at SoCMin to minimize cost. A good
	// default is the mean price over the horizon.
	TerminalSoCPrice float64

	// Export revenue. Two modes:
	//   - If ExportOrePerKWh > 0, every slot earns this flat rate on
	//     export. Useful for operators with a fixed feed-in tariff.
	//   - If ExportOrePerKWh == 0, each slot earns
	//         slot.SpotOre + ExportBonusOreKwh − ExportFeeOreKwh
	//     i.e. raw spot pricing. The value can go negative when spot
	//     is negative (Sweden + Nordic markets, increasingly common
	//     during midday solar peaks). The DP treats a negative export
	//     ore as a positive cost — exactly right when most retailers
	//     pass the negative spot through to the customer.
	//
	//     ExportFloorOreKwh, if non-nil, clamps the per-slot export
	//     ore at the given floor. Set to a pointer-to-zero for
	//     retailers that cap export at 0 öre (no negative-spot
	//     billing). nil = no clamp (default; matches the physics).
	ExportOrePerKWh    float64
	ExportBonusOreKwh  float64
	ExportFeeOreKwh    float64
	ExportFloorOreKwh  *float64

	// Loadpoint extends the DP state space with one EV charge point.
	// Nil (default) keeps the battery-only optimization path. See
	// LoadpointSpec for the state/action shape.
	Loadpoint *LoadpointSpec
}

// Action is one scheduled battery target.
type Action struct {
	SlotStartMs int64   `json:"slot_start_ms"`
	SlotLenMin  int     `json:"slot_len_min"`
	PriceOre    float64 `json:"price_ore"`
	// SpotOre is the raw wholesale spot price (öre/kWh, ex grid tariff
	// and VAT). Surfaced so the UI can break the price bar into
	// components (spot + grid tariff + VAT) — pedagogical view of
	// where the kr/kWh actually goes. Mirrors Slot.SpotOre.
	SpotOre     float64 `json:"spot_ore"`
	PVW         float64 `json:"pv_w"`
	LoadW       float64 `json:"load_w"`
	BatteryW    float64 `json:"battery_w"`  // decision (site sign, AC terminals)
	GridW       float64 `json:"grid_w"`     // resulting grid power
	SoCPct      float64 `json:"soc_pct"`    // SoC at END of slot
	CostOre     float64 `json:"cost_ore"`   // this slot's cost (öre). Negative = revenue.
	Confidence  float64 `json:"confidence"` // 1.0 real, <1.0 forecasted (UI uses this to style)
	Reason      string  `json:"reason"`     // short human-readable explanation
	EMSMode     string  `json:"ems_mode"`   // effective EMS mode for this slot (set by SlotAt post-processing)

	// PVLimitW is the recommended cap on PV inverter output (W, positive).
	// 0 = no curtailment. Set by post-processing when exporting would
	// cost money (negative export revenue after fees). Consumed by the
	// control loop only when the driver advertises `supports_pv_curtail`.
	PVLimitW float64 `json:"pv_limit_w,omitempty"`

	// LoadpointW is the EV charger power (W, positive = charging) the
	// DP picked for this slot. Zero when no loadpoint was in Params
	// or the DP chose "don't charge" this slot. Per-loadpoint in a
	// multi-LP future; single-value for now.
	LoadpointW float64 `json:"loadpoint_w,omitempty"`

	// LoadpointSoCPct is the EV SoC at END of slot, following the
	// same convention as SoCPct for the home battery.
	LoadpointSoCPct float64 `json:"loadpoint_soc_pct,omitempty"`
}

// Baselines are counter-factual dispatch costs over the same horizon,
// included alongside the plan so the UI can show "savings vs X" numbers
// without re-deriving the cost model client-side. All values in öre.
//
// NoBatteryOre is the cost if the battery didn't exist at all (grid =
// load + pv each slot, priced with the same import/export model the DP
// uses). SelfConsumptionOre comes from re-running Optimize with
// ModeSelfConsumption — so it uses the real efficiency, power, and SoC
// constraints, not a simplified simulation. FlatAvgOre re-prices the
// no-battery flows at horizon-mean prices (import and export each at
// their own mean) — shows the value of *timing* (shifting load to
// cheap hours), separate from the value of having a battery.
// AvgPriceOre is the time-weighted mean import price over the horizon;
// NetKWh is import minus export volume.
type Baselines struct {
	NoBatteryOre       float64 `json:"no_battery_ore"`
	SelfConsumptionOre float64 `json:"self_consumption_ore"`
	FlatAvgOre         float64 `json:"flat_avg_ore"`
	AvgPriceOre        float64 `json:"avg_price_ore"`
	NetKWh             float64 `json:"net_kwh"`
}

// Plan is the output.
type Plan struct {
	GeneratedAtMs int64      `json:"generated_at_ms"`
	Mode          Mode       `json:"mode"`
	HorizonSlots  int        `json:"horizon_slots"`
	CapacityWh    float64    `json:"capacity_wh"`
	InitialSoCPct float64    `json:"initial_soc_pct"`
	TotalCostOre  float64    `json:"total_cost_ore"`
	Actions       []Action   `json:"actions"`
	Baselines     *Baselines `json:"baselines,omitempty"`
}

// SlotGridCostOre returns the öre cost of flowing gridKWh across the
// meter during a slot, using the same import/export model the DP loop
// uses (see Optimize). Positive gridKWh = importing at consumer price;
// negative = exporting, revenue = spot + bonus − fee (or a flat rate
// if p.ExportOrePerKWh > 0). Clamped so a negative export price never
// becomes a reward for not exporting.
//
// Shared between the DP and ComputeBaselines so they agree on the cost
// model exactly — baselines must use the same formula as the plan
// they're compared against, otherwise "savings" are misleading.
func SlotGridCostOre(slot Slot, gridKWh float64, p Params) float64 {
	if gridKWh > 0 {
		return slot.PriceOre * gridKWh
	}
	return -SlotExportPriceOre(slot, p) * (-gridKWh)
}

// SlotExportPriceOre returns the öre/kWh a slot's export earns, using the
// same model SlotGridCostOre uses on the export side: a flat
// ExportOrePerKWh wins if set, otherwise spot + bonus − fee clamped at
// zero. Exported here so baseline / reconstruction code can apply the
// export side of the cost model without duplicating the formula.
func SlotExportPriceOre(slot Slot, p Params) float64 {
	if p.ExportOrePerKWh > 0 {
		return p.ExportOrePerKWh
	}
	v := slot.SpotOre + p.ExportBonusOreKwh - p.ExportFeeOreKwh
	if v < 0 {
		v = 0
	}
	return v
}

// Optimize runs DP and returns the cost-minimizing plan.
//
// Complexity: O(N × S × A) where N = len(slots), S = SoCLevels, A = ActionLevels.
// For a 96-slot (24h × 15m) horizon with 41 SoC × 21 action levels, that's
// ~82k evaluations — well under 10ms.
func Optimize(slots []Slot, p Params) Plan {
	now := time.Now().UnixMilli()
	if len(slots) == 0 || p.CapacityWh <= 0 {
		return Plan{GeneratedAtMs: now, Mode: p.Mode}
	}
	if p.Mode == "" {
		p.Mode = ModeSelfConsumption
	}
	if p.ChargeEfficiency <= 0 {
		p.ChargeEfficiency = 0.95
	}
	if p.DischargeEfficiency <= 0 {
		p.DischargeEfficiency = 0.95
	}
	N := len(slots)
	S := p.SoCLevels
	if S < 3 {
		S = 3
	}
	A := p.ActionLevels
	if A < 3 {
		A = 3
	}

	socStep := (p.SoCMaxPct - p.SoCMinPct) / float64(S-1)
	socAt := func(i int) float64 { return p.SoCMinPct + float64(i)*socStep }

	// Confidence handling: compute the horizon mean (real + forecast)
	// so we can blend low-confidence prices toward it. Default any
	// missing confidence to 1.0 (treat caller-unaware slots as "real").
	var sumPrice float64
	for i := range slots {
		if slots[i].Confidence <= 0 {
			slots[i].Confidence = 1.0
		}
		sumPrice += slots[i].PriceOre
	}
	meanPrice := sumPrice / float64(N)
	// effPrice(slot) = c × raw + (1 − c) × mean. c=1 → raw; c<1 pulls
	// toward horizon mean, dampening arbitrage the DP sees on shaky
	// forecasted slots without hiding them entirely.
	effPrice := func(s Slot) float64 {
		return s.Confidence*s.PriceOre + (1-s.Confidence)*meanPrice
	}
	// slotExportOre: per-slot export revenue (öre/kWh). When
	// Params.ExportOrePerKWh is set, it wins (fixed feed-in tariff).
	// Otherwise each slot earns spot + bonus − fee.
	//
	// The DP's cost formula for an exporting slot is
	//   cost = -slotExportOre(slot) * |gridKWh|
	// so a NEGATIVE return value here is a positive cost — exactly
	// what we want when spot is below zero (you pay to export under
	// most Swedish retail agreements). Earlier code clamped this at
	// zero, which made the DP indifferent between "stand still" and
	// "blast export from battery" during minus-price hours and
	// occasionally tripped the discharge in the latter direction by
	// tie-break. Real incident: 2026-05-02 user switched to arbitrage
	// at spot ≈ −5 öre and watched the battery discharge full power
	// into the grid.
	//
	// If a retailer caps you at zero (no negative-spot billing) set
	// Params.ExportFloorOreKwh to 0 — that re-introduces the clamp at
	// the operator's choice rather than as silent default.
	slotExportOre := func(s Slot) float64 {
		if p.ExportOrePerKWh > 0 {
			return p.ExportOrePerKWh
		}
		v := s.SpotOre + p.ExportBonusOreKwh - p.ExportFeeOreKwh
		// Confidence blend on the same principle as import price.
		mean := meanPrice * 0.7 // rough: spot ≈ 70% of consumer total
		v = s.Confidence*v + (1-s.Confidence)*mean
		if p.ExportFloorOreKwh != nil && v < *p.ExportFloorOreKwh {
			v = *p.ExportFloorOreKwh
		}
		return v
	}

	// Action grid spans −MaxDischargeW … +MaxChargeW. Forcing an odd
	// ActionLevels puts 0 exactly at the midpoint.
	actionAt := func(j int) float64 {
		if A == 1 {
			return 0
		}
		frac := float64(j) / float64(A-1) // 0..1
		return -p.MaxDischargeW + frac*(p.MaxChargeW+p.MaxDischargeW)
	}

	// EV dimensions. When no loadpoint is active, EL=EA=1 and the
	// EV loops degenerate to a single pass that adds zero power /
	// zero SoC — functionally identical to the legacy battery-only
	// DP. Keeps one code path.
	lp := p.Loadpoint
	evActive := lp.active()
	EL := 1
	EA := 1
	var evSteps []float64
	var evSocStep float64
	var evChargeEff float64 = 0.9
	if evActive {
		EL = lp.Levels
		evSteps = lp.normalizedSteps()
		EA = len(evSteps)
		if lp.MaxPct <= lp.MinPct {
			lp.MaxPct = 100
			lp.MinPct = 0
		}
		evSocStep = (lp.MaxPct - lp.MinPct) / float64(EL-1)
		if lp.ChargeEfficiency > 0 {
			evChargeEff = lp.ChargeEfficiency
		}
	}
	evSocAt := func(e int) float64 {
		if !evActive {
			return 0
		}
		return lp.MinPct + float64(e)*evSocStep
	}
	evActionW := func(ea int) float64 {
		if !evActive {
			return 0
		}
		return evSteps[ea]
	}

	// V[t][s][e] = minimum expected cost from slot t onward, starting
	// from battery SoC index s and EV SoC index e. Backward-filled.
	V := make([][][]float64, N+1)
	Policy := make([][][]int, N) // encodes (battActionIdx * EA + evActionIdx)
	for t := 0; t <= N; t++ {
		V[t] = make([][]float64, S)
		for si := 0; si < S; si++ {
			V[t][si] = make([]float64, EL)
		}
		if t < N {
			Policy[t] = make([][]int, S)
			for si := 0; si < S; si++ {
				Policy[t][si] = make([]int, EL)
			}
		}
	}

	// Deadline slot index for the mid-horizon EV target penalty. A
	// target beyond horizon end gets clamped to the last slot so
	// the DP still "sees" it (rather than silently ignoring). A
	// target of -1 means no deadline — opportunistic charging only.
	deadlineSlot := -1
	if evActive && lp.TargetSoCPct > 0 {
		deadlineSlot = lp.TargetSlotIdx
		if deadlineSlot < 0 {
			deadlineSlot = -1
		} else if deadlineSlot >= N {
			deadlineSlot = N - 1
		}
	}

	// Terminal values. Battery SoC credits stored energy. EV SoC
	// carries no terminal cost — the deadline-slot penalty below
	// handles target enforcement and avoids double-counting.
	for si := 0; si < S; si++ {
		battKwh := p.CapacityWh * socAt(si) / 100.0 / 1000.0
		battCredit := -p.TerminalSoCPrice * battKwh
		for ei := 0; ei < EL; ei++ {
			V[N][si][ei] = battCredit
		}
	}

	// Backwards induction.
	for t := N - 1; t >= 0; t-- {
		slot := slots[t]
		dtH := float64(slot.LenMin) / 60.0
		for si := 0; si < S; si++ {
			soc := socAt(si)
			for ei := 0; ei < EL; ei++ {
				evSoc := evSocAt(ei)
				bestV := math.Inf(1)
				bestPolicy := 0
				for ba := 0; ba < A; ba++ {
					battW := actionAt(ba)

					// Battery SoC transition (independent of EV).
					var dBattWh float64
					if battW >= 0 {
						dBattWh = +battW * dtH * p.ChargeEfficiency
					} else {
						dBattWh = +battW * dtH / p.DischargeEfficiency
					}
					battSoc2 := soc + dBattWh/p.CapacityWh*100.0
					if battSoc2 < p.SoCMinPct-1e-9 || battSoc2 > p.SoCMaxPct+1e-9 {
						continue
					}

					for ea := 0; ea < EA; ea++ {
						evW := evActionW(ea)
						// EV SoC transition: only charging, so non-
						// negative by construction. Skip actions
						// that would overshoot max (realistic
						// chargers taper, but we approximate).
						var evSoc2 float64
						if evActive {
							dEvWh := evW * dtH * evChargeEff
							evSoc2 = evSoc + dEvWh/lp.CapacityWh*100.0
							if evSoc2 > lp.MaxPct+1e-9 {
								continue
							}
						}
						// EV appears as a site load (+ site-signed).
						// GridW = load + PV + battery + EV.
						gridW := slot.LoadW + slot.PVW + battW + evW

						// Surplus-only EV: forbid any non-zero EV
						// action that turns the site into a net
						// importer. evW = 0 is always feasible (the
						// constraint short-circuits), so the DP
						// degrades gracefully on low-PV days — the
						// deadline shortfall penalty then makes the
						// "miss target" outcome expensive but legal.
						// 50 W epsilon absorbs floating-point dither
						// from the discretized PV/load grid so the
						// constraint isn't artificially tight against
						// an action that's effectively zero net.
						if evActive && lp.SurplusOnly && evW > 0 && gridW > 50 {
							continue
						}

						// Mode-based feasibility. Baseline includes
						// EV so the mode check asks "is the extra
						// battery action pulling the grid further
						// into import/export than baseline?".
						baseGridW := slot.LoadW + slot.PVW + evW
						if !modeAllows(p.Mode, baseGridW, gridW, battW) {
							continue
						}

						// Per-slot power limits.
						if !slot.Limits.allowsImport(gridW) || !slot.Limits.allowsExport(gridW) {
							continue
						}

						gridKWh := gridW * dtH / 1000.0
						var cost float64
						if gridKWh > 0 {
							cost = effPrice(slot) * gridKWh
						} else {
							cost = -slotExportOre(slot) * (-gridKWh)
						}

						// Strict self-consumption bias. When the mode
						// is self_consumption we add a penalty equal
						// to 2× effPrice per kWh of HOUSE-driven grid
						// import — so the total house-import cost the
						// DP sees is tripled. That makes discharge
						// strictly cheaper than idle whenever the
						// battery can physically supply house load.
						//
						// The penalty applies only to the house
						// portion of the import (gridW minus EV). EV
						// charging has its own deadline-shortfall
						// penalty (below, 4× meanPrice per kWh of
						// shortfall); applying the SC bias on top
						// would let the DP prefer missing an EV
						// deadline over charging it whenever the
						// slot price exceeded ~4/3 of the horizon
						// mean — Codex P1 on PR #122.
						//
						// Rationale: operator picked self_consumption
						// because they want "use my battery before
						// grid." Pure cost-minimisation over a long
						// horizon with a high horizon-mean will
						// sometimes prefer importing today to
						// preserve SoC for tomorrow's peak — that's
						// arbitrage behaviour, not self-consumption.
						// The bias extends all the way down to
						// SoCMinPct: the operator's configured floor
						// IS the reserve, no implicit extra buffer on
						// top of it (#157). The hard floor is still
						// enforced by the SoC-transition feasibility
						// check above (line 357), so the bias can only
						// push discharge *toward* the floor, never
						// past it.
						if p.Mode == ModeSelfConsumption {
							houseGridW := slot.LoadW + slot.PVW + battW
							if houseGridW > 0 {
								houseKWh := houseGridW * dtH / 1000.0
								cost += 2.0 * effPrice(slot) * houseKWh
							}
						}

						// Deadline slot: if this slot is the EV's
						// deadline AND target isn't met with this
						// action's evSoc2, add a shortfall penalty.
						// Penalty factor 4×meanPrice makes missing
						// the target more expensive than the
						// aggressive-slot charge cost a DP might
						// otherwise prefer, so it commits when
						// feasible. Lexicographic behaviour:
						// infeasible targets degrade gracefully —
						// DP maximizes delivered energy since less
						// shortfall = less penalty.
						if deadlineSlot == t {
							short := lp.TargetSoCPct - evSoc2
							if short > 0 {
								missedKwh := lp.CapacityWh * short / 100.0 / 1000.0
								cost += missedKwh * meanPrice * 4.0
							}
						}

						// Battery SoC interpolation indices.
						fIdx := (battSoc2 - p.SoCMinPct) / socStep
						lo := int(math.Floor(fIdx))
						hi := lo + 1
						if lo < 0 {
							lo, hi = 0, 0
						}
						if hi >= S {
							lo, hi = S-1, S-1
						}
						frac := fIdx - float64(lo)

						// EV SoC interpolation indices. Discrete
						// charger steps often produce fractional
						// bucket advances (e.g. 11 kW for 15 min →
						// 4.1 % at 10 % resolution). Rounding to
						// nearest kills incremental progress and
						// makes the DP blind to "almost-a-full-
						// bucket" moves. Bilinear lookup preserves
						// fractional progress.
						eLo, eHi := 0, 0
						eFrac := 0.0
						if evActive {
							f := (evSoc2 - lp.MinPct) / evSocStep
							eLo = int(math.Floor(f))
							eHi = eLo + 1
							if eLo < 0 {
								eLo, eHi = 0, 0
							}
							if eHi >= EL {
								eLo, eHi = EL-1, EL-1
							}
							eFrac = f - float64(eLo)
						}
						vNext := (1-frac)*(1-eFrac)*V[t+1][lo][eLo] +
							(1-frac)*eFrac*V[t+1][lo][eHi] +
							frac*(1-eFrac)*V[t+1][hi][eLo] +
							frac*eFrac*V[t+1][hi][eHi]
						total := cost + vNext
						if total < bestV {
							bestV = total
							bestPolicy = ba*EA + ea
						}
					}
				}
				// If every action at this (slot, soc, ev_soc) state
				// was rejected (mode + PowerLimits combined out of
				// feasibility), bestV stays +Inf and bestPolicy
				// defaults to 0 — which encodes (battery action 0,
				// EV action 0) = "full discharge, EV off", NOT
				// "idle". A forward-sim that reaches this state
				// would pick the worst possible action. Fall back
				// to closest-to-idle: battery action at the middle
				// of the grid (≈0 W when ActionLevels is odd) and
				// EV off. The +Inf V propagates upstream so the
				// DP avoids routing through this infeasible region
				// when a legal path exists.
				if math.IsInf(bestV, 1) {
					bestPolicy = ((A - 1) / 2) * EA
				}
				V[t][si][ei] = bestV
				Policy[t][si][ei] = bestPolicy
			}
		}
	}

	// Forward simulate using the policy.
	plan := Plan{
		GeneratedAtMs: now,
		Mode:          p.Mode,
		HorizonSlots:  N,
		CapacityWh:    p.CapacityWh,
		InitialSoCPct: p.InitialSoCPct,
		Actions:       make([]Action, 0, N),
	}
	fIdx := (p.InitialSoCPct - p.SoCMinPct) / socStep
	si := int(math.Round(fIdx))
	if si < 0 {
		si = 0
	}
	if si >= S {
		si = S - 1
	}
	soc := socAt(si)
	// Initial EV SoC index.
	ei := 0
	var evSoc float64
	if evActive {
		f := (lp.InitialSoCPct - lp.MinPct) / evSocStep
		ei = int(math.Round(f))
		if ei < 0 {
			ei = 0
		}
		if ei >= EL {
			ei = EL - 1
		}
		evSoc = evSocAt(ei)
	}
	var totalCost float64
	for t := 0; t < N; t++ {
		slot := slots[t]
		dtH := float64(slot.LenMin) / 60.0
		pol := Policy[t][si][ei]
		ba := pol / EA
		ea := pol % EA
		actW := actionAt(ba)
		evW := evActionW(ea)
		// Battery SoC transition.
		var dSoCWh float64
		if actW >= 0 {
			dSoCWh = +actW * dtH * p.ChargeEfficiency
		} else {
			dSoCWh = +actW * dtH / p.DischargeEfficiency
		}
		soc2 := soc + dSoCWh/p.CapacityWh*100.0
		if soc2 < p.SoCMinPct {
			soc2 = p.SoCMinPct
		}
		if soc2 > p.SoCMaxPct {
			soc2 = p.SoCMaxPct
		}
		// EV SoC transition (no-op when !evActive since evW = 0).
		var evSoc2 float64
		if evActive {
			dEvWh := evW * dtH * evChargeEff
			evSoc2 = evSoc + dEvWh/lp.CapacityWh*100.0
			if evSoc2 > lp.MaxPct {
				evSoc2 = lp.MaxPct
			}
		}
		gridW := slot.LoadW + slot.PVW + actW + evW
		gridKWh := gridW * dtH / 1000.0
		// Report the ACTUAL expected cost using the raw (un-blended)
		// prices so the UI summary reflects "what we'd actually pay
		// if prices hold". Blending is a decision lens only.
		var cost float64
		if gridKWh > 0 {
			cost = slot.PriceOre * gridKWh
		} else {
			rawExport := p.ExportOrePerKWh
			if rawExport <= 0 {
				rawExport = slot.SpotOre + p.ExportBonusOreKwh - p.ExportFeeOreKwh
				if rawExport < 0 {
					rawExport = 0
				}
			}
			cost = -rawExport * (-gridKWh)
		}
		totalCost += cost
		a := Action{
			SlotStartMs: slot.StartMs,
			SlotLenMin:  slot.LenMin,
			PriceOre:    slot.PriceOre,
			SpotOre:     slot.SpotOre,
			Confidence:  slot.Confidence,
			PVW:         slot.PVW,
			LoadW:       slot.LoadW,
			BatteryW:    actW,
			GridW:       gridW,
			SoCPct:      soc2,
			CostOre:     cost,
			Reason:      reasonFor(slot, actW, gridW, meanPrice),
		}
		if evActive {
			a.LoadpointW = evW
			a.LoadpointSoCPct = evSoc2
		}
		plan.Actions = append(plan.Actions, a)
		soc = soc2
		fIdx = (soc - p.SoCMinPct) / socStep
		si = int(math.Round(fIdx))
		if si < 0 {
			si = 0
		}
		if si >= S {
			si = S - 1
		}
		if evActive {
			evSoc = evSoc2
			f := (evSoc - lp.MinPct) / evSocStep
			ei = int(math.Round(f))
			if ei < 0 {
				ei = 0
			}
			if ei >= EL {
				ei = EL - 1
			}
		}
	}
	plan.TotalCostOre = totalCost
	annotateCurtailment(&plan, p.ExportOrePerKWh)
	return plan
}

// annotateCurtailment walks the plan and flags slots where curtailing
// PV would avoid a net-negative export event. Triggered when:
//
//   - the slot is exporting (grid_w < 0)
//   - AND export revenue is non-positive (fee ≥ revenue, or negative spot)
//   - AND the battery can't absorb more (already charging at max)
//
// In that case exporting PV costs money with no offsetting benefit.
// Recommended PV limit = load + battery_charge (just cover what the
// site + battery can consume). Driver dispatches this only if it
// advertises PV-curtailment support. The CostOre doesn't change — the
// DP already priced this slot as-is; curtailment is a mitigation
// applied at dispatch time.
func annotateCurtailment(plan *Plan, exportOrePerKWh float64) {
	if exportOrePerKWh > 0 {
		// Positive export price → exporting is always better than
		// curtailing. Nothing to do.
		return
	}
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if a.GridW >= 0 {
			continue // importing, not exporting
		}
		// Slot is exporting. If we can't earn on export, cap PV to
		// what's being consumed locally + stored.
		consumedW := a.LoadW
		if a.BatteryW > 0 {
			consumedW += a.BatteryW // site-sign: + = charging (absorbs PV)
		}
		if consumedW < 0 {
			consumedW = 0
		}
		// Only suggest curtailment if PV actually exceeds local consumption.
		pvAbs := -a.PVW // PV stored site-signed as negative
		if pvAbs > consumedW {
			a.PVLimitW = consumedW
			if a.Reason != "" && !strings.HasSuffix(a.Reason, ")") {
				a.Reason += " · curtail PV"
			} else {
				a.Reason = "curtail PV (negative export) · " + a.Reason
			}
		}
	}
}

// modeAllows enforces the mode's grid-use policy.
//
//	baselineGridW = load + pv  (what grid would see with no battery action)
//	gridW         = baselineGridW + actW  (what grid actually sees)
//	actW          = battery command (+ charge, − discharge)
func modeAllows(m Mode, baselineGridW, gridW, actW float64) bool {
	const eps = 1e-6
	switch m {
	case ModeSelfConsumption:
		// Battery must only move the grid toward zero, never past it:
		//   if baseline > 0 (import): grid must be in [0, baseline]
		//   if baseline < 0 (export): grid must be in [baseline, 0]
		//   if baseline == 0: battery must be 0
		if baselineGridW > eps {
			return gridW >= -eps && gridW <= baselineGridW+eps
		}
		if baselineGridW < -eps {
			return gridW <= eps && gridW >= baselineGridW-eps
		}
		return math.Abs(actW) < eps
	case ModeCheapCharge:
		// Allow charging from grid (any actW ≥ 0), but never discharge past
		// the local load: i.e. gridW must stay ≥ 0 when we'd otherwise be
		// importing, OR ≥ baseline when we'd otherwise be exporting.
		// Simpler rule: no battery-driven export, i.e. gridW ≥ min(0, baseline).
		minGrid := 0.0
		if baselineGridW < 0 {
			minGrid = baselineGridW
		}
		return gridW >= minGrid-eps
	case ModeArbitrage:
		return true
	default:
		return true
	}
}

// reasonFor returns a short human-readable explanation of the planner's
// decision for a single slot. The UI surfaces this on hover so operators
// can see *why* the battery is (dis)charging — explainable AI at the
// level it actually helps: per-decision.
//
// Labels branch on the POST-action gridW, not just the pre-action
// baseline. That matters when the battery action pushes grid from
// "importing a little" into "exporting a lot" — previously labelled
// "cover local load" because baseline was still technically positive,
// which made it impossible for operators to tell a defensive
// discharge from an aggressive export-for-arbitrage.
func reasonFor(s Slot, batteryW, gridW, meanPrice float64) string {
	baseline := s.LoadW + s.PVW // what grid would see with no battery
	const chargeThresh = IdleGateThresholdW
	const gridThresh = 100.0
	priceTag := ""
	if s.Confidence < 1.0 {
		priceTag = " (predicted)"
	}
	// Classify the resulting grid direction — this is what the
	// operator sees on the meter, and what matters for the label.
	gridExports := gridW < -gridThresh
	gridImports := gridW > gridThresh
	priceAbove := s.PriceOre > meanPrice*1.1
	priceBelow := s.PriceOre < meanPrice*0.9

	switch {
	// --- charging branches ---
	case batteryW > chargeThresh && baseline < -chargeThresh && !gridImports:
		// PV-dominant baseline + battery charging + not pulling extra
		// from grid → the battery is absorbing (part of) the PV
		// surplus. Partial absorption (grid still exports some) still
		// counts as "absorb PV surplus" — the operator is seeing the
		// battery act as a sink for solar energy. Only flip to the
		// "charge — import" branches when the battery's appetite
		// exceeds PV output and drags grid into actual import.
		return "absorb PV surplus" + priceTag
	case batteryW > chargeThresh && gridImports && priceBelow:
		return "charge from cheap grid" + priceTag
	case batteryW > chargeThresh && gridImports:
		return "charge — import" + priceTag
	case batteryW > chargeThresh:
		return "charge" + priceTag

	// --- discharging branches ---
	case batteryW < -chargeThresh && gridExports && priceAbove:
		return "discharge — export at peak" + priceTag
	case batteryW < -chargeThresh && gridExports:
		return "discharge — export" + priceTag
	case batteryW < -chargeThresh && priceAbove:
		// Reducing import during a high-price slot — even if it
		// doesn't push grid negative, the motive is peak-shaving.
		return "discharge — price above horizon mean" + priceTag
	case batteryW < -chargeThresh && baseline > chargeThresh:
		return "discharge — cover local load" + priceTag
	case batteryW < -chargeThresh:
		return "discharge" + priceTag

	// --- idle branches ---
	default:
		if gridImports {
			return "idle — import to cover load" + priceTag
		}
		if gridExports {
			return "idle — export PV surplus" + priceTag
		}
		return "idle" + priceTag
	}
}
