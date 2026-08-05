package appproto

import (
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
)

// planFrom projects the planner's output onto the wire.
//
// Two things happen here and nowhere else. The planner's per-slot explanation
// is prose written in English for an operator dashboard; the app needs a
// stable code, because the box has no idea what language anyone reads. And
// staleness becomes explicit: "nothing is scheduled" and "we do not know what
// is scheduled" are different sentences, and only one of them is honest when
// the planner could not run.
func planFrom(p *mpc.Plan, rev uint64, ceilingW *int64, uptimeMs int64, now time.Time) Plan {
	out := Plan{
		Rev:      rev,
		UptimeMs: uptimeMs,
		Slots:    []PlanSlot{},
		CeilingW: ceilingW,
	}

	if p == nil || len(p.Actions) == 0 {
		// No plan at all. Stale rather than empty: an empty schedule would
		// claim the box means to do nothing, which is a claim nobody made.
		out.Stale = true
		return out
	}

	// A plan older than the control loop is willing to act on is not what the
	// box is doing any more, whatever it says.
	out.Stale = now.Sub(time.UnixMilli(p.GeneratedAtMs)) > mpc.MaxPlanAge

	mean := meanPriceOre(p.Actions)
	minSoC := minSoCPct(p.Actions)

	for _, a := range p.Actions {
		price := int64(math.Round(a.PriceOre))
		out.Slots = append(out.Slots, PlanSlot{
			StartMs:    a.SlotStartMs,
			DurationMs: int64(a.SlotLenMin) * int64(time.Minute/time.Millisecond),
			BatteryW:   roundW(a.BatteryW),
			GridW:      roundW(a.GridW),
			PriceMinor: &price,
			Reason:     reasonCode(a, mean, minSoC, ceilingW),
		})
	}

	return out
}

// meanPriceOre is the horizon mean, weighted by slot length. Slots are not
// guaranteed to be the same length, and an unweighted mean would let a stray
// short slot drag the reference the reason codes are judged against.
func meanPriceOre(actions []mpc.Action) float64 {
	var sum, weight float64
	for _, a := range actions {
		w := float64(a.SlotLenMin)
		if w <= 0 {
			w = 1
		}
		sum += a.PriceOre * w
		weight += w
	}
	if weight == 0 {
		return 0
	}
	return sum / weight
}

// minSoCPct is the lowest state of charge the plan intends to reach.
//
// The planner never crosses its own reserve floor, so the deepest point it
// schedules is the floor it is defending. That makes it the honest reference
// for "held back" without this package having to be told the floor separately
// and then drift from it.
func minSoCPct(actions []mpc.Action) float64 {
	min := math.Inf(1)
	for _, a := range actions {
		if a.SoCPct < min {
			min = a.SoCPct
		}
	}
	if math.IsInf(min, 1) {
		return 0
	}
	return min
}

// reasonCode classifies one slot.
//
// Derived from the numbers, not parsed from mpc's English. Parsing the prose
// would break the first time somebody improved a sentence, and the point of a
// stable code is that it survives exactly that.
func reasonCode(a mpc.Action, meanPriceOre, minSoCPct float64, ceilingW *int64) PlanReason {
	const idleGateW = mpc.IdleGateThresholdW
	const gridThreshW = 100.0

	// Site convention: PV is negative while generating, load positive. The
	// baseline is what the meter would read with the battery doing nothing.
	baseline := a.LoadW + a.PVW

	gridExports := a.GridW < -gridThreshW
	gridImports := a.GridW > gridThreshW
	priceAbove := a.PriceOre > meanPriceOre*1.1

	// A ceiling being defended is a peak-shaving story regardless of price,
	// and it outranks the price bands: the ceiling is a hardware limit, the
	// price is an opinion.
	if ceilingW != nil && baseline > float64(*ceilingW) && a.BatteryW < -idleGateW {
		return ReasonPeakShaving
	}

	switch {
	case a.BatteryW > idleGateW:
		if baseline < -idleGateW && !gridImports {
			// The battery is absorbing solar the site would otherwise
			// export. Partial absorption still counts: the operator sees
			// the battery acting as a sink for sunlight.
			return ReasonSolarSurplus
		}
		// Charging off the meter. The planner only buys energy to store it
		// when doing so beats using it later, so the story is the same
		// whether this hour is the cheapest or merely cheap enough.
		return ReasonCheapImport

	case a.BatteryW < -idleGateW:
		if gridExports {
			return ReasonExportPaid
		}
		// Discharging to avoid buying — at a peak if the price says so,
		// and to cover the house otherwise. Both are the same decision
		// seen from different prices.
		return ReasonExpensiveImport

	default:
		// Idle at its deepest planned point during an expensive hour is
		// the planner refusing to spend the reserve, which is a decision
		// worth naming rather than calling nothing.
		if priceAbove && a.SoCPct <= minSoCPct+0.5 {
			return ReasonReserveHeld
		}
		if gridExports {
			return ReasonExportPaid
		}
		return ReasonIdle
	}
}
