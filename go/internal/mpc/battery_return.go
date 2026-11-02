package mpc

import "math"

// A Swedish household gets back the energy tax on grid energy it stored in
// the battery and later sent back to the grid (11 kap. 13 § lagen
// (1994:1776) om skatt på energi). Losses are not refunded, nor is the VAT.
// The plan counts the refund in the order the History figure does (package
// batteryreturn), slot by slot rather than hour by hour:
//
//   - In each slot the battery took from the grid only what solar could not
//     have supplied, and gave to the grid only what solar could not have
//     exported.
//   - Other energy comes in whole, then the export leaves and takes other
//     energy first, then grid energy comes in after the round-trip loss,
//     with the loss kept as other energy, then the house takes its share and
//     takes grid energy first.
//   - The account holds what the battery holds: what does not fit sheds grid
//     energy first. History caps at the battery's size instead, so the plan
//     sheds grid energy sooner.
//
// Energies are in Wh the battery can give back (AC out). The Core DP
// carries the grid part of what the battery holds as a share, a second
// state beside the SoC.

// batteryReturnShareLevels is the Core DP's grid-share grid: 0, ½, 1. On a
// month of real prices with solar, 3, 5, 7 and 11 levels gave the same
// plans' value to within 0.3 kr.
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

// returnStep is one slot as the refund counts it, in Wh the battery can give
// back: what the house and the grid take from the battery, the grid and
// other energy it takes in, and how much it gains; then, for a given start,
// what it holds before and after. Only the start and the grid share vary,
// so the Core DP builds the flows once per slot and action.
type returnStep struct {
	homeWh, toGridWh    float64
	gridInWh, otherInWh float64
	gainWh              float64

	heldWh, afterWh, invAfter float64
}

// newReturnFlows builds a slot's flows.
func newReturnFlows(slot Slot, batteryW, gridW, dtH, etaC, etaD float64) returnStep {
	toBatteryW, toGridW := returnFlowsW(slot, batteryW, gridW)
	var s returnStep
	switch {
	case batteryW < 0:
		s.toGridWh = toGridW * dtH
		s.homeWh = -batteryW*dtH - s.toGridWh
		s.gainWh = batteryW * dtH
	case batteryW > 0:
		s.gridInWh = toBatteryW * dtH * etaC * etaD
		// Other energy and the grid energy's loss, whole.
		s.otherInWh = batteryW*dtH - s.gridInWh
		s.gainWh = batteryW * dtH * etaC * etaD
	}
	return s
}

// from starts the step with storedWh in the cells.
func (s returnStep) from(storedWh, etaD float64) returnStep {
	s.heldWh = max(storedWh, 0) * etaD
	s.afterWh = max(s.heldWh+s.gainWh, 0)
	s.invAfter = 0
	if s.afterWh > 0 {
		s.invAfter = 1 / s.afterWh
	}
	return s
}

// apply moves a grid share across the step in History's order: other
// energy in, the export out (other energy first), grid energy in, the house
// (grid energy first), then what does not fit sheds grid energy. It returns
// the share after the step and the grid energy sent back, in Wh.
func (s *returnStep) apply(share float64) (float64, float64) {
	grid := share * s.heldWh
	other := s.heldWh - grid + s.otherInWh
	fromOther := min(other, s.toGridWh)
	returned := min(grid, s.toGridWh-fromOther)
	grid += s.gridInWh - returned
	fromGrid := min(grid, s.homeWh)
	grid -= fromGrid
	other = max(other-fromOther-(s.homeWh-fromGrid), 0)
	grid -= min(grid, max(grid+other-s.afterWh, 0))
	return max(grid*s.invAfter, 0), returned
}

// returnMixStep moves the grid share across one slot. storedWh is the energy
// in the cells at the slot's start and share the grid part of it. It returns
// the share at the slot's end and the grid energy sent back, in AC Wh.
func returnMixStep(slot Slot, batteryW, gridW, storedWh, share, dtH, etaC, etaD float64) (float64, float64) {
	step := newReturnFlows(slot, batteryW, gridW, dtH, etaC, etaD).from(storedWh, etaD)
	return step.apply(share)
}

// annotateBatteryReturn sets each action's returned grid energy and the
// plan's refund. It reads only executed power and SoC, so it prices a
// worker's plan and Core's own the same way.
func annotateBatteryReturn(plan *Plan, slots []Slot, p Params) {
	plan.BatteryReturnOre = walkBatteryReturn(plan.Actions, slots, p, func(i int, wh float64) {
		plan.Actions[i].BatteryReturnWh = wh
	})
}

// walkBatteryReturn replays the account over the actions, hands each
// action's returned grid energy (AC Wh) to each, and returns the refund in
// öre.
func walkBatteryReturn(actions []Action, slots []Slot, p Params, each func(i int, wh float64)) float64 {
	credit := p.batteryReturnCredit()
	etaC, etaD := p.ChargeEfficiency, p.DischargeEfficiency
	if etaC <= 0 {
		etaC = 0.95
	}
	if etaD <= 0 {
		etaD = 0.95
	}
	share, soc := p.initialGridShare(), p.InitialSoC
	var returnedWh float64
	for i, a := range actions {
		var wh float64
		if credit > 0 && i < len(slots) {
			share, wh = returnMixStep(slots[i], a.BatteryW, a.GridW,
				soc*p.CapacityWh, share, slots[i].DurationHours(), etaC, etaD)
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
// credit per kWh; the grid and other energy History held after the last
// hour it counted, in Wh the battery can give back; and what the battery
// took in and gave out since, in AC Wh. A zero value counts no refund.
type BatteryReturnState struct {
	OreKwh            float64
	GridWh, OtherWh   float64
	ChargedSinceWh    float64
	DischargedSinceWh float64
}

// startShare is the grid share of the storedWh the cells hold now. It counts
// low: what the battery gave out since History's count took grid energy,
// what it took in counts as other energy, and History's other energy is all
// still there.
func (b BatteryReturnState) startShare(storedWh, etaD float64) float64 {
	if etaD <= 0 {
		etaD = 0.95
	}
	held := storedWh * etaD
	if !(held > 0) {
		return 0
	}
	grid := min(b.GridWh-max(b.DischargedSinceWh, 0), held-max(b.OtherWh, 0)-max(b.ChargedSinceWh, 0))
	return min(max(grid/held, 0), 1)
}
