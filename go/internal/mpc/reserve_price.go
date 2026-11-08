package mpc

import (
	"cmp"
	"context"
	"math"
	"slices"
)

// Price the reserve's EV energy without changing its battery trajectory or
// delivered energy. Each slot offers a legal on-power for part of the interval.
// This is a bounded improvement to a feasible reserve, not a global optimum.
func priceReserveCharging(ctx context.Context, slots []Slot, p Params, reserve Plan) (Plan, error) {
	loads := p.activeLoadpoints()
	// This pass prices energy only; retain the reserve when a demand tariff
	// couples costs across slots. The native planner owns those tariff costs.
	if len(loads) != 1 || len(p.DemandCharges) > 0 {
		return reserve, nil
	}
	lp := loads[0]
	eff := lp.ChargeEfficiency
	if eff <= 0 {
		eff = .9
	}
	energyWh := 0.0
	for i, a := range reserve.Actions {
		energyWh += a.LoadpointW * slots[i].DurationHours() * eff
	}
	if energyWh <= 1e-6 {
		return reserve, nil
	}
	type offer struct {
		index           int
		peakW, unitCost float64
	}
	offers := make([]offer, 0, len(slots))
	steps := lp.normalizedSteps()
	for i, slot := range slots {
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		if i > lp.TargetSlotIdx {
			break
		}
		baseGrid := slot.LoadW + slot.PVW + reserve.Actions[i].BatteryW
		for j := len(steps) - 1; j > 0; j-- {
			peak := steps[j]
			// A half-slot probe checks both the idle and charging endpoints.
			probe := Action{
				BatteryW: reserve.Actions[i].BatteryW, GridW: baseGrid + peak/2,
				LoadpointW:         peak / 2,
				LoadpointPowerW:    map[string]float64{lp.ID: peak / 2},
				LoadpointMaxPowerW: map[string]float64{lp.ID: peak},
			}
			if validateEVPulse(slot, p, probe, slot.PVW, true) != nil {
				continue
			}
			hours := slot.DurationHours()
			offCost := SlotGridCostOre(slot, baseGrid*hours/1000, p)
			onCost := SlotGridCostOre(slot, (baseGrid+peak)*hours/1000, p)
			offers = append(offers, offer{i, peak, (onCost - offCost) / (peak * hours * eff)})
			break
		}
	}
	slices.SortStableFunc(offers, func(a, b offer) int { return cmp.Compare(a.unitCost, b.unitCost) })
	priced := reserve
	priced.Actions = append([]Action(nil), reserve.Actions...)
	for i := range priced.Actions {
		a := &priced.Actions[i]
		a.LoadpointW = 0
		a.LoadpointPowerW = map[string]float64{lp.ID: 0}
		a.LoadpointMaxPowerW = map[string]float64{lp.ID: 0}
		a.LoadpointSoCByID = map[string]float64{lp.ID: lp.InitialSoC}
	}
	remaining := energyWh
	_, importCaps := importHourCapsW(slots, p)
	for _, o := range offers {
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		if remaining <= 1e-6 {
			break
		}
		hours := slots[o.index].DurationHours()
		average := min(o.peakW, remaining/(eff*hours))
		if importCaps != nil {
			slot := slots[o.index]
			average = min(average, evAverageRoomW(importCaps[o.index], slot.LoadW+slot.PVW+reserve.Actions[o.index].BatteryW, o.peakW))
			if average <= 1e-6 {
				continue
			}
		}
		a := &priced.Actions[o.index]
		a.LoadpointW = average
		a.LoadpointPowerW[lp.ID] = average
		a.LoadpointMaxPowerW[lp.ID] = o.peakW
		remaining -= average * eff * hours
	}
	if remaining > 1e-6 {
		return reserve, nil
	}
	evEnergy := lp.InitialSoC * lp.CapacityWh
	priced.TotalCostOre = 0
	for i := range priced.Actions {
		a := &priced.Actions[i]
		evEnergy += a.LoadpointW * slots[i].DurationHours() * eff
		a.LoadpointSoC = evEnergy / lp.CapacityWh
		a.LoadpointSoCByID[lp.ID] = a.LoadpointSoC
		a.GridW = slots[i].LoadW + slots[i].PVW + a.BatteryW + a.LoadpointW
		a.CostOre = reserveSlotCost(slots[i], p, *a)
		priced.TotalCostOre += a.CostOre
	}
	// Price improvements never trade away the reserve's physical guarantees.
	// Keep its independently valid plan if allocation or replay cannot match it.
	if math.Abs(evEnergy-lp.InitialSoC*lp.CapacityWh-energyWh) > 1e-6 ||
		priced.TotalCostOre >= reserve.TotalCostOre-1e-6 || ValidatePlan(slots, p, &priced) != nil {
		return reserve, nil
	}
	if reserve.Solver != nil {
		info := *reserve.Solver
		info.PolicyVersion = "core-reserve-prices-v1"
		priced.Solver = &info
	}
	return priced, nil
}
