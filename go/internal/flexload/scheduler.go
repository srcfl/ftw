// Package flexload schedules price-responsive flexible loads — thermostats
// (thermal "batteries") and deferrable on/off loads (smart plugs) — against
// the same price and PV forecasts the battery MPC consumes.
//
// Why a separate layer instead of new dimensions in the battery DP: the
// battery/EV DP is O(N·S·A·E_L·E_A) and already ~100M state evaluations.
// Adding a thermal SoC dimension and an interruptible-load dimension would
// multiply that into the billions (curse of dimensionality) and put the
// safety-critical battery loop at risk. Flexible loads couple to the rest
// of the system only through the grid-power balance and the price signal,
// so they decompose cleanly: the battery DP plans first, and the flex-load
// scheduler optimizes each device against the resulting price curve (with
// an optional PV-surplus credit). This is the standard HEMS decomposition.
//
// The scheduler is pure and deterministic — it takes forecasts + device
// specs and returns schedules. A Service (service.go) wires it to live
// forecasts and dispatches the schedules to Matter drivers.
package flexload

import (
	"math"
	"sort"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/thermalmodel"
)

// PriceSlot is one horizon step the scheduler optimizes over. PV surplus is
// the expected available solar after the rest of the house is served (≥0);
// leave it 0 if unknown.
type PriceSlot struct {
	StartMs    int64
	LenMin     int
	PriceOre   float64 // consumer total import price öre/kWh
	PVSurplusW float64 // expected PV surplus available this slot (W, ≥0)
}

func (s PriceSlot) hours() float64 { return float64(s.LenMin) / 60.0 }

// ---- Thermal (thermostat) scheduling ----

// ThermalSpec describes one thermostat zone to schedule.
//
// Heating kind matters for power accounting. A direct-electric zone
// (electric radiator, resistive floor heating) converts electricity to
// heat 1:1, so COP=1 and the metered electrical watts equal the thermal
// watts delivered. A hydronic zone (a thermostatic radiator valve on a
// water loop fed by a heat pump) delivers thermal watts the EMS pays for
// at electrical = thermal/COP, because the heat pump multiplies electricity
// into heat (COP≈3). MaxHeatW is always the zone's *thermal* output cap;
// EstHeatW in the resulting schedule is the *electrical* draw (thermal/COP)
// so the grid/fuse accounting is correct for both kinds.
type ThermalSpec struct {
	DriverName string             // Matter driver to command
	Model      thermalmodel.Model // learned RC dynamics for the zone
	CurrentC   float64            // measured indoor temperature now
	MinC       float64            // comfort floor (hard constraint)
	MaxC       float64            // comfort ceiling (pre-heat target)
	MaxHeatW   float64            // zone thermal output cap (W)
	COP        float64            // electrical→thermal multiplier (1 = direct electric, ~3 = heat pump). ≤0 treated as 1.
	// Outdoor returns the forecast outdoor temperature (°C) for a slot
	// start. Must be non-nil.
	Outdoor func(slotStartMs int64) float64
	// PreHeatFraction is the share of the horizon's cheapest slots in
	// which to actively bank heat toward MaxC. 0 falls back to 0.33.
	PreHeatFraction float64
}

// ThermalSetpoint is the per-slot directive for a thermostat.
type ThermalSetpoint struct {
	StartMs  int64
	TargetC  float64 // setpoint to write to the thermostat this slot
	EstHeatW float64 // estimated ELECTRICAL draw (thermal/COP) for grid/fuse accounting
	PreHeat  bool    // true when banking heat in a cheap/PV slot
}

// ThermalSchedule is the full plan for one zone.
type ThermalSchedule struct {
	DriverName string
	Setpoints  []ThermalSetpoint
}

// PlanThermal produces a comfort-respecting, price-aware setpoint schedule.
//
// Strategy: pre-heat toward MaxC in the cheapest PreHeatFraction of slots
// (and whenever PV surplus covers the heater), otherwise coast toward MinC
// — but a forward simulation of the RC model guarantees the predicted
// indoor temperature never crosses below MinC: any slot that would breach
// the floor is forced to heat enough to hold it. The thermostat's own
// controller does the closed-loop work; we only choose its setpoint.
func PlanThermal(slots []PriceSlot, spec ThermalSpec) ThermalSchedule {
	out := ThermalSchedule{DriverName: spec.DriverName}
	if len(slots) == 0 || spec.Outdoor == nil {
		return out
	}
	minC, maxC := spec.MinC, spec.MaxC
	if maxC < minC {
		minC, maxC = maxC, minC
	}
	frac := spec.PreHeatFraction
	if frac <= 0 || frac > 1 {
		frac = 0.33
	}
	cop := spec.COP
	if cop <= 0 {
		cop = 1.0
	}
	// PV "covers" the load when the surplus meets the *electrical* draw.
	pvCoverW := 0.5 * spec.MaxHeatW / cop
	// Price threshold = the frac-th percentile of horizon prices. Slots at
	// or below it are "cheap" → pre-heat.
	threshold := priceQuantile(slots, frac)

	indoor := spec.CurrentC
	out.Setpoints = make([]ThermalSetpoint, 0, len(slots))
	for _, sl := range slots {
		outdoor := spec.Outdoor(sl.StartMs)
		pvCovers := sl.PVSurplusW > 0 && sl.PVSurplusW >= pvCoverW
		preHeat := sl.PriceOre <= threshold || pvCovers

		target := minC
		if preHeat {
			target = maxC
		}

		// Comfort-floor guard: if coasting (no heat) would let the zone
		// fall below MinC by the end of this slot, raise the target to at
		// least MinC so the thermostat heats to hold it.
		dt := sl.hours() * 3600.0
		coastTemp := spec.Model.PredictNext(indoor, outdoor, 0, dt)
		if coastTemp < minC && target < minC {
			target = minC
		}

		// Estimate the THERMAL power delivered to the zone this slot to
		// reach/hold target. If we're pulling the temperature UP toward
		// target, assume near-full output; if just holding, use the
		// steady-state hold power.
		var thermalW float64
		if target > indoor+0.1 {
			thermalW = spec.MaxHeatW
		} else {
			thermalW = spec.Model.HeatToHoldW(target, outdoor)
		}
		if thermalW > spec.MaxHeatW {
			thermalW = spec.MaxHeatW
		}
		if thermalW < 0 {
			thermalW = 0
		}

		out.Setpoints = append(out.Setpoints, ThermalSetpoint{
			StartMs:  sl.StartMs,
			TargetC:  target,
			EstHeatW: thermalW / cop, // electrical draw (= thermal for direct electric)
			PreHeat:  preHeat,
		})

		// Roll the zone temperature forward using the THERMAL power so the
		// next slot's decision sees a self-consistent state.
		indoor = spec.Model.PredictNext(indoor, outdoor, thermalW, dt)
		// Clamp to the band the thermostat would itself enforce.
		if indoor > maxC {
			indoor = maxC
		}
	}
	return out
}

// ---- Simple valuation (MPC-independent) ----
//
// The simple controller is a standalone, interpretable alternative to the
// horizon scheduler for operators who don't run the MPC (or want a
// degraded-mode fallback). It needs only: the current indoor temperature,
// a target (comfort) temperature, the current price vs. a threshold, and
// the learned thermal model. The rule is "block heating during expensive
// periods when the building's own thermal inertia will keep the target
// satisfied for the block horizon; otherwise heat." No forecast required
// when a fixed price threshold is given.

// SimpleSpec is the input to a single simple-mode evaluation.
type SimpleSpec struct {
	Model          thermalmodel.Model
	CurrentC       float64       // measured indoor temp
	TargetC        float64       // comfort target (the temp we must keep)
	MinC           float64       // hard floor (never block below this)
	Outdoor        float64       // current outdoor temp
	PriceNow       float64       // current price (öre/kWh)
	PriceThreshold float64       // "expensive" cutoff (öre/kWh)
	BlockHorizon   time.Duration // target must stay satisfied this long to allow a block
	MaxHeatW       float64       // zone thermal output cap
	COP            float64       // electrical/thermal ratio (≤0 → 1)
}

// SimpleDecision is the output: whether to heat and to what setpoint.
type SimpleDecision struct {
	Heat       bool    // true = heat toward TargetC; false = block (coast)
	SetpointC  float64 // setpoint to write (TargetC if heating, MinC if blocking)
	EstHeatW   float64 // estimated electrical draw if heating (thermal/COP), else 0
	CoastHours float64 // estimated hours of coast before temp falls to TargetC
	Reason     string
}

// EvaluateSimple applies the block/heat rule for one zone.
func EvaluateSimple(spec SimpleSpec) SimpleDecision {
	cop := spec.COP
	if cop <= 0 {
		cop = 1
	}
	// Hard floor always wins.
	if spec.CurrentC <= spec.MinC {
		return SimpleDecision{
			Heat: true, SetpointC: spec.TargetC,
			EstHeatW: spec.MaxHeatW / cop,
			Reason:   "at/below comfort floor",
		}
	}

	coast := coastHoursToTarget(spec.Model, spec.CurrentC, spec.TargetC, spec.Outdoor, 24*time.Hour)
	expensive := spec.PriceThreshold > 0 && spec.PriceNow > spec.PriceThreshold
	bufferEnough := coast >= spec.BlockHorizon.Hours()

	if expensive && bufferEnough {
		return SimpleDecision{
			Heat: false, SetpointC: spec.MinC,
			EstHeatW:   0,
			CoastHours: coast,
			Reason:     "expensive + thermal buffer covers block horizon",
		}
	}
	// Otherwise heat to keep the target.
	holdThermal := spec.Model.HeatToHoldW(spec.TargetC, spec.Outdoor)
	if holdThermal > spec.MaxHeatW {
		holdThermal = spec.MaxHeatW
	}
	reason := "maintaining target"
	if expensive {
		reason = "expensive but buffer insufficient — heating to protect target"
	}
	return SimpleDecision{
		Heat: true, SetpointC: spec.TargetC,
		EstHeatW:   holdThermal / cop,
		CoastHours: coast,
		Reason:     reason,
	}
}

// coastHoursToTarget rolls the RC model forward with no heating until the
// indoor temp decays to targetC, returning the elapsed hours (capped).
func coastHoursToTarget(m thermalmodel.Model, indoorC, targetC, outdoorC float64, cap time.Duration) float64 {
	if indoorC <= targetC {
		return 0
	}
	const step = 300.0 // 5-min integration step (s)
	maxS := cap.Seconds()
	t := 0.0
	temp := indoorC
	for t < maxS {
		temp = m.PredictNext(temp, outdoorC, 0, step)
		t += step
		if temp <= targetC {
			break
		}
	}
	return t / 3600.0
}

// ArbitrateSimple resolves competition between simple-mode zones under a
// shared electrical power budget (e.g. the site fuse headroom reserved for
// heating). When the sum of heating draws would exceed budgetW, it blocks
// the highest-power zones *that can afford it* (coast ≥ their block
// horizon) first — realising "block the higher-consumption load when its
// target will still be satisfied" rather than tripping the breaker or
// blocking a zone that actually needs the heat. Zones at their comfort
// floor are never blocked. budgetW ≤ 0 disables arbitration.
func ArbitrateSimple(decisions []SimpleDecision, specs []SimpleSpec, budgetW float64) {
	if budgetW <= 0 || len(decisions) != len(specs) {
		return
	}
	var total float64
	for _, d := range decisions {
		if d.Heat {
			total += d.EstHeatW
		}
	}
	if total <= budgetW {
		return
	}
	// Candidates we may block: currently heating, above the floor, and with
	// enough coast buffer to ride out a block.
	type cand struct{ idx int; powerW, coast float64 }
	var cands []cand
	for i, d := range decisions {
		if !d.Heat {
			continue
		}
		s := specs[i]
		if s.CurrentC <= s.MinC {
			continue // protect the floor
		}
		if d.CoastHours < s.BlockHorizon.Hours() {
			continue // needs the heat now
		}
		cands = append(cands, cand{idx: i, powerW: d.EstHeatW, coast: d.CoastHours})
	}
	// Block highest-power (then longest-coast) first until under budget.
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].powerW != cands[b].powerW {
			return cands[a].powerW > cands[b].powerW
		}
		return cands[a].coast > cands[b].coast
	})
	for _, c := range cands {
		if total <= budgetW {
			break
		}
		d := &decisions[c.idx]
		total -= d.EstHeatW
		d.Heat = false
		d.SetpointC = specs[c.idx].MinC
		d.EstHeatW = 0
		d.Reason = "deferred under shared power budget (target still satisfiable)"
	}
}

// ---- Deferrable (smart-plug) scheduling ----

// DeferrableSpec describes a load that must run for a given energy budget
// somewhere inside a window, but can be interrupted freely (no minimum run
// length) — e.g. a water heater on a smart plug, a dehumidifier, a pool
// pump. The scheduler picks the cheapest eligible slots.
type DeferrableSpec struct {
	DriverName string
	EnergyWh   float64 // energy still needed over the window
	PowerW     float64 // power drawn while running
	EarliestMs int64   // don't run before this (0 = no lower bound)
	DeadlineMs int64   // must finish by this (0 = end of horizon)
	// PreferPV biases selection toward slots with PV surplus by crediting
	// the surplus against the slot price.
	PreferPV bool
}

// DeferrableSlot is the per-slot directive for a deferrable load.
type DeferrableSlot struct {
	StartMs int64
	On      bool
	EstW    float64 // PowerW when On, else 0
}

// DeferrableSchedule is the full plan for one deferrable load.
type DeferrableSchedule struct {
	DriverName string
	Slots      []DeferrableSlot
	// ScheduledWh is the energy the plan actually places (may be less than
	// requested if the window can't hold it).
	ScheduledWh float64
}

// PlanDeferrable selects the cheapest eligible slots to meet the energy
// budget. Eligibility is the [EarliestMs, DeadlineMs] window; among
// eligible slots, the cheapest (by PV-credited price) are turned on until
// the energy budget is met.
func PlanDeferrable(slots []PriceSlot, spec DeferrableSpec) DeferrableSchedule {
	out := DeferrableSchedule{DriverName: spec.DriverName}
	if len(slots) == 0 || spec.PowerW <= 0 || spec.EnergyWh <= 0 {
		// Still emit an all-off schedule so the dispatcher can turn it off.
		out.Slots = allOff(slots)
		return out
	}

	type cand struct {
		idx      int
		effPrice float64
	}
	var cands []cand
	for i, sl := range slots {
		if spec.EarliestMs != 0 && sl.StartMs < spec.EarliestMs {
			continue
		}
		if spec.DeadlineMs != 0 && sl.StartMs >= spec.DeadlineMs {
			continue
		}
		eff := sl.PriceOre
		if spec.PreferPV && sl.PVSurplusW > 0 {
			// Credit PV surplus that covers (part of) the load. A slot whose
			// surplus fully covers PowerW is treated as ~free.
			coverage := sl.PVSurplusW / spec.PowerW
			if coverage > 1 {
				coverage = 1
			}
			eff = sl.PriceOre * (1 - coverage)
		}
		cands = append(cands, cand{idx: i, effPrice: eff})
	}
	// Cheapest first; tie-break on earlier slot so we finish sooner.
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].effPrice != cands[b].effPrice {
			return cands[a].effPrice < cands[b].effPrice
		}
		return cands[a].idx < cands[b].idx
	})

	onIdx := make(map[int]bool)
	remaining := spec.EnergyWh
	var scheduled float64
	for _, c := range cands {
		if remaining <= 0 {
			break
		}
		sl := slots[c.idx]
		slotWh := spec.PowerW * sl.hours()
		onIdx[c.idx] = true
		scheduled += slotWh
		remaining -= slotWh
	}
	out.ScheduledWh = scheduled

	out.Slots = make([]DeferrableSlot, len(slots))
	for i, sl := range slots {
		on := onIdx[i]
		w := 0.0
		if on {
			w = spec.PowerW
		}
		out.Slots[i] = DeferrableSlot{StartMs: sl.StartMs, On: on, EstW: w}
	}
	return out
}

// ---- helpers ----

func allOff(slots []PriceSlot) []DeferrableSlot {
	out := make([]DeferrableSlot, len(slots))
	for i, sl := range slots {
		out[i] = DeferrableSlot{StartMs: sl.StartMs, On: false}
	}
	return out
}

// priceQuantile returns the q-quantile (0..1) of the slot prices.
func priceQuantile(slots []PriceSlot, q float64) float64 {
	if len(slots) == 0 {
		return 0
	}
	prices := make([]float64, len(slots))
	for i, s := range slots {
		prices[i] = s.PriceOre
	}
	sort.Float64s(prices)
	if q <= 0 {
		return prices[0]
	}
	if q >= 1 {
		return prices[len(prices)-1]
	}
	pos := q * float64(len(prices)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return prices[lo]
	}
	frac := pos - float64(lo)
	return prices[lo]*(1-frac) + prices[hi]*frac
}
