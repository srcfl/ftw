package mpc

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// coreReservePlan makes prompt progress toward an EV deadline when the worker
// is unavailable. It uses exact energy, legal on-power and idle/charge pulses;
// optional home-battery trading must not consume the car's available headroom.
// The service still validates the complete plan before publication.
func coreReservePlan(ctx context.Context, slots []Slot, p Params) Plan {
	loads := p.activeLoadpoints()
	if len(loads) != 1 || loads[0].TargetSoC <= 0 {
		plan, _ := OptimizeContext(ctx, slots, p)
		return plan
	}
	lp := loads[0]
	plan := Plan{GeneratedAtMs: time.Now().UnixMilli(), Mode: p.Mode, HorizonSlots: len(slots), CapacityWh: p.CapacityWh, InitialSoC: p.InitialSoC}
	if coreDPModelError(p) != nil {
		return plan
	}
	eff := lp.ChargeEfficiency
	if eff <= 0 {
		eff = .9
	}
	chargeEff, dischargeEff := p.ChargeEfficiency, p.DischargeEfficiency
	if chargeEff <= 0 {
		chargeEff = .95
	}
	if dischargeEff <= 0 {
		dischargeEff = .95
	}
	energy, evEnergy := p.InitialSoC*p.CapacityWh, lp.InitialSoC*lp.CapacityWh
	ceiling := lp.SoCMax
	if ceiling <= lp.SoCMin {
		ceiling = 1
	}
	target := min(lp.TargetSoC, ceiling) * lp.CapacityWh
	steps := lp.normalizedSteps()
	for i, slot := range slots {
		if ctx.Err() != nil {
			return Plan{}
		}
		hours := slot.DurationHours()
		if hours <= 0 {
			return Plan{}
		}
		minBatteryW := -min(p.MaxDischargeW, max(0, energy-p.SoCMin*p.CapacityWh)*dischargeEff/hours)
		maxBatteryW := min(p.MaxChargeW, max(0, p.SoCMax*p.CapacityWh-energy)/chargeEff/hours)
		house := slot.LoadW + slot.PVW
		chooseBattery := func(peak float64) (float64, bool) {
			low, high := minBatteryW, maxBatteryW
			if slot.Limits.MaxExportW > 0 {
				low = max(low, -slot.Limits.MaxExportW-house)
			}
			if slot.Limits.MaxImportW > 0 {
				high = min(high, slot.Limits.MaxImportW-house-peak)
			}
			if peak > 0 && lp.blocksBatteryToEV() {
				low = max(low, -max(0, house))
			}
			if low > high {
				return 0, false
			}
			b := min(max(0, low), high)
			if !modeAllows(p.Mode, house, house+b, b) || !modeAllows(p.Mode, house+peak, house+peak+b, b) {
				return 0, false
			}
			return b, true
		}
		peak, battery := 0.0, 0.0
		battery, ok := chooseBattery(0)
		if !ok {
			return Plan{}
		}
		need := max(0, target-evEnergy)
		if need > 1e-6 && i <= lp.TargetSlotIdx {
			for j := len(steps) - 1; j > 0; j-- {
				step := steps[j]
				if lp.SurplusOnly && surplusOnlyExceedsHousePV(step, slot.LoadW, slot.PVW) {
					continue
				}
				if b, ok := chooseBattery(step); ok {
					peak, battery = step, b
					break
				}
			}
		}
		average := min(peak, need/(eff*hours))
		evEnergy += average * eff * hours
		energy += loadpoint.BatteryEnergyDeltaWh(battery, hours, chargeEff, dischargeEff)
		a := Action{ExecutionStartMs: slot.ExecutionStartMs, SlotStartMs: slot.StartMs, SlotLenMin: slot.LenMin, PriceOre: slot.PriceOre, SpotOre: slot.SpotOre, PVW: slot.PVW, LoadW: slot.LoadW, BatteryW: battery, GridW: house + battery + average, SoC: energy / p.CapacityWh,
			LoadpointW: average, LoadpointSoC: evEnergy / lp.CapacityWh, LoadpointPowerW: map[string]float64{lp.ID: average}, LoadpointSoCByID: map[string]float64{lp.ID: evEnergy / lp.CapacityWh}, LoadpointMaxPowerW: map[string]float64{lp.ID: peak}, Reason: "EV deadline reserve"}
		a.CostOre = reserveSlotCost(slot, p, a)
		plan.TotalCostOre += a.CostOre
		plan.Actions = append(plan.Actions, a)
	}
	plan.LoadpointShortfallWh = map[string]float64{lp.ID: max(0, target-evEnergy)}
	plan.Solver = &SolverInfo{Engine: "core", Backend: "ev_reserve", Status: "feasible"}
	return plan
}

func setCoreReserveSolver(plan *Plan, p Params, elapsed float64) {
	if plan.Solver == nil {
		plan.Solver = coreSolverInfo(p, elapsed)
	} else {
		plan.Solver.SolveMs = elapsed
	}
}

// Import and export have different prices. Price the two real pulse states,
// not their average grid power when a partial charge crosses zero grid flow.
func reserveSlotCost(slot Slot, p Params, a Action) float64 {
	if len(a.LoadpointMaxPowerW) == 0 {
		return SlotGridCostOre(slot, a.GridW*slot.DurationHours()/1000, p)
	}
	cuts := []float64{0, 1}
	totalAverage := 0.0
	for id, peak := range a.LoadpointMaxPowerW {
		average := a.LoadpointPowerW[id]
		totalAverage += average
		if peak > 0 && average > 0 && average < peak {
			cuts = append(cuts, average/peak)
		}
	}
	slices.Sort(cuts)
	cost := 0.0
	for i := 1; i < len(cuts); i++ {
		midpoint := (cuts[i-1] + cuts[i]) / 2
		grid := a.GridW - totalAverage
		for id, peak := range a.LoadpointMaxPowerW {
			if peak > 0 && a.LoadpointPowerW[id]/peak > midpoint {
				grid += peak
			}
		}
		cost += (cuts[i] - cuts[i-1]) * SlotGridCostOre(slot, grid*slot.DurationHours()/1000, p)
	}
	return cost
}

// Check the minimum and maximum simultaneous EV draw. Full-interval charges
// never contribute an off state; partial charges may start or stop together.
// Both endpoints must respect the site limit and the battery/PV policies.
func validateEVPulse(slot Slot, p Params, a Action, pvW float64) error {
	if len(a.LoadpointMaxPowerW) == 0 {
		return nil
	}
	loads := p.activeLoadpoints()
	if len(loads) != len(a.LoadpointMaxPowerW) {
		return fmt.Errorf("EV pulse must retain every loadpoint")
	}
	minEV, maxEV, solarPeak := 0.0, 0.0, 0.0
	for i, lp := range loads {
		peak, ok := a.LoadpointMaxPowerW[lp.ID]
		average, hasAverage := a.LoadpointPowerW[lp.ID]
		if !ok || !hasAverage || !finite(peak) || !finite(average) || average < 0 || peak < 0 {
			return fmt.Errorf("invalid EV pulse identity or power")
		}
		if average <= 1e-7 {
			average = 0
		}
		if peak <= 1e-7 {
			peak = 0
		}
		if peak < average || (average == 0 && peak != 0) {
			return fmt.Errorf("invalid EV pulse identity or power")
		}
		if i == 0 && math.Abs(a.LoadpointW-average) > 1e-6 {
			return fmt.Errorf("EV pulse average disagrees with loadpoint allocation")
		}
		maxEV += peak
		if average == peak {
			minEV += peak
		}
		if lp.SurplusOnly {
			solarPeak += peak
		}
		if peak > 0 && lp.blocksBatteryToEV() && loadpoint.BatteryDischargeFeedsEV(a.BatteryW, peak, slot.LoadW, pvW) {
			return fmt.Errorf("battery discharge feeds EV pulse")
		}
	}
	if surplusOnlyExceedsHousePV(solarPeak, slot.LoadW, pvW) {
		return fmt.Errorf("EV pulses exceed PV surplus")
	}
	for _, power := range []float64{minEV, maxEV} {
		baseline := slot.LoadW + pvW + power
		grid := baseline + a.BatteryW
		if (slot.Limits.MaxImportW > 0 && grid > slot.Limits.MaxImportW+solverGridLimitToleranceW) || (slot.Limits.MaxExportW > 0 && grid < -slot.Limits.MaxExportW-solverGridLimitToleranceW) {
			return fmt.Errorf("EV pulse violates instantaneous grid limit")
		}
		if !modeAllows(p.Mode, baseline, grid, a.BatteryW) {
			return fmt.Errorf("EV pulse violates mode")
		}
		if power > 0 && a.BatteryW < 0 && grid < -50 {
			return fmt.Errorf("EV pulse charges during battery export")
		}
	}
	return nil
}
