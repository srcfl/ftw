package mpc

import "math"

// A Swedish household gets back the energy tax on grid energy it stored in
// the battery and later sent back to the grid (11 kap. 13 § lagen
// (1994:1776) om skatt på energi). Losses are not refunded, nor is the VAT.
// The plan counts the refund as the History figure does (package
// batteryreturn), slot by slot rather than hour by hour:
//
//   - In each slot the battery took from the grid only what solar could not
//     have supplied, and gave to the grid only what solar could not have
//     exported.
//   - The battery holds a mix of grid and other energy. Each kWh it gives
//     to the grid carries the grid share it held. Grid energy enters the mix
//     after the charge loss and leaves it before the discharge loss.
//
// The Core DP carries the grid share as a second state beside the SoC.

// batteryReturnShareLevels is the Core DP's grid-share grid: 0, ½, 1. A
// fixed plan's refund is linear in the share, so few levels are enough: on
// a month of real prices 3, 5 and 21 levels gave the same plans' value to
// within 0.1 kr.
const batteryReturnShareLevels = 3

// batteryReturnCredit is the öre per kWh of returned grid energy the plan
// counts on, or 0. Only active arbitrage sends battery energy to the grid.
func (p Params) batteryReturnCredit() float64 {
	if p.Mode != ModeArbitrage || !(p.BatteryReturnOreKwh > 0) || math.IsInf(p.BatteryReturnOreKwh, 0) {
		return 0
	}
	return p.BatteryReturnOreKwh
}

// batteryReturnShareLevels is how many grid-share levels the Core DP keeps:
// one when the plan counts no refund. A plugged-in car already takes the
// DP near its time budget, so with one the DP leaves the refund out; the
// published plan still reports what it earns.
func (p Params) batteryReturnShareLevels() int {
	if p.batteryReturnCredit() > 0 && !p.Loadpoint.active() {
		return batteryReturnShareLevels
	}
	return 1
}

// initialGridShare is the start share, held to 0..1.
func (p Params) initialGridShare() float64 {
	if !(p.BatteryReturnGridShare > 0) {
		return 0
	}
	return min(p.BatteryReturnGridShare, 1)
}

// returnFlowsW splits one slot as the refund counts it: the battery's charge
// from the grid and its discharge to the grid, in AC watts.
func returnFlowsW(slot Slot, batteryW, gridW float64) (gridToBatteryW, batteryToGridW float64) {
	solarW := max(0, -slot.PVW)
	switch {
	case batteryW > 0:
		return max(0, min(max(0, gridW), batteryW-solarW)), 0
	case batteryW < 0:
		return 0, max(0, min(-batteryW, -gridW-solarW))
	}
	return 0, 0
}

// returnMixStep moves the grid share across one slot. storedWh is the energy
// in the cells at the slot's start and share the grid part of it. It returns
// the share at the slot's end and the grid energy sent back, in AC Wh.
func returnMixStep(slot Slot, batteryW, gridW, storedWh, share, dtH, etaC float64) (float64, float64) {
	toBatteryW, toGridW := returnFlowsW(slot, batteryW, gridW)
	if toGridW > 0 {
		// A discharge leaves the mix as it was.
		return share, share * toGridW * dtH
	}
	if batteryW > 0 {
		if after := storedWh + batteryW*dtH*etaC; after > 0 {
			share = (share*max(storedWh, 0) + toBatteryW*dtH*etaC) / after
		}
	}
	return min(max(share, 0), 1), 0
}

// annotateBatteryReturn sets each action's returned grid energy and the
// plan's refund. It reads only executed power and SoC, so it prices a
// worker's plan and Core's own the same way.
func annotateBatteryReturn(plan *Plan, slots []Slot, p Params) {
	plan.BatteryReturnOre = walkBatteryReturn(plan.Actions, slots, p, func(i int, wh float64) {
		plan.Actions[i].BatteryReturnWh = wh
	})
}

// walkBatteryReturn replays the mix over the actions, hands each action's
// returned grid energy (AC Wh) to each, and returns the refund in öre.
func walkBatteryReturn(actions []Action, slots []Slot, p Params, each func(i int, wh float64)) float64 {
	credit := p.batteryReturnCredit()
	etaC := p.ChargeEfficiency
	if etaC <= 0 {
		etaC = 0.95
	}
	share, soc := p.initialGridShare(), p.InitialSoC
	var returnedWh float64
	for i, a := range actions {
		var wh float64
		if credit > 0 && i < len(slots) {
			share, wh = returnMixStep(slots[i], a.BatteryW, a.GridW,
				soc*p.CapacityWh, share, slots[i].DurationHours(), etaC)
			returnedWh += wh
		}
		if each != nil {
			each(i, wh)
		}
		soc = a.SoC
	}
	return credit * returnedWh / 1000
}

// BatteryReturnState is what Core knows of the refund when it plans: the
// credit per kWh, the grid share of what the battery held when History last
// counted it, and the AC Wh the battery took in since. A zero value counts
// no refund.
type BatteryReturnState struct {
	OreKwh         float64
	GridShare      float64
	ChargedSinceWh float64
}

// startShare is the grid share of the storedWh the cells hold now. Energy
// that came in after History's count counts as other energy; a discharge
// since then took grid energy in proportion.
func (b BatteryReturnState) startShare(storedWh, etaC float64) float64 {
	share := min(max(b.GridShare, 0), 1)
	if !(share > 0) || !(storedWh > 0) {
		return 0
	}
	if etaC <= 0 {
		etaC = 0.95
	}
	counted := max(storedWh-max(b.ChargedSinceWh, 0)*etaC, 0)
	return share * counted / storedWh
}
