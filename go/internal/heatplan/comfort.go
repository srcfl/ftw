package heatplan

import (
	"errors"
	"math"
)

// This file holds the owner's two rules for what heat is worth, beside the
// plan itself. Neither has a default in code: every number in them is the
// owner's, the zero value of each rule changes nothing, and a caller must
// not invent one.
//
//   - PriceComfort: how warm the owner wants a zone depends on what the day
//     costs. A cheap day may buy a warmer floor; a dear day may cost a
//     cooler room. It moves the target before planning, so the plan and its
//     baseline both stand on the comfort that day is worth.
//   - SavingsGate: moving heat in time is only worth a device's writes and
//     the owner's attention when the plans together save enough in a day.
//     Below that the devices are left on their targets.
//
// PriceComfort needs no model of the zone and holds from the first day.
// SavingsGate weighs plans, so it is only as good as the twins behind them.

// PriceComfort moves a zone's target with the day's mean price. All prices
// are in the unit of Request.Price.
type PriceComfort struct {
	// CheapPrice is the day's mean price at or below which the target is
	// raised by all of RaiseK. NormalPrice is where the target is the
	// owner's own, and DearPrice the mean at or above which it is lowered by
	// all of LowerK. Between them the shift is linear, never a step.
	CheapPrice  float64 `json:"cheap_price"`
	NormalPrice float64 `json:"normal_price"`
	DearPrice   float64 `json:"dear_price"`
	// RaiseK and LowerK are how far the target moves, in kelvin. Zero turns
	// that direction off: a floor may follow cheap days up while the room
	// air only follows dear days down.
	RaiseK float64 `json:"raise_k"`
	LowerK float64 `json:"lower_k"`
	// StepK rounds the shift to the control's step, halves away from zero.
	// Zero leaves it unrounded.
	StepK float64 `json:"step_k,omitempty"`
}

// Enabled reports whether the rule moves anything.
func (c PriceComfort) Enabled() bool { return c.RaiseK > 0 || c.LowerK > 0 }

// Validate refuses a rule that cannot be read one way. A direction that is
// off needs no price.
func (c PriceComfort) Validate() error {
	if !finite(c.CheapPrice, c.NormalPrice, c.DearPrice, c.RaiseK, c.LowerK, c.StepK) {
		return errors.New("heatplan: price comfort must be finite")
	}
	if c.RaiseK < 0 || c.LowerK < 0 || c.RaiseK > 10 || c.LowerK > 10 || c.StepK < 0 || c.StepK > 5 {
		return errors.New("heatplan: price comfort moves a target 0 to 10 K, in steps of at most 5 K")
	}
	if c.RaiseK > 0 && !(c.CheapPrice < c.NormalPrice) {
		return errors.New("heatplan: the cheap price must be below the normal price")
	}
	if c.LowerK > 0 && !(c.NormalPrice < c.DearPrice) {
		return errors.New("heatplan: the dear price must be above the normal price")
	}
	return nil
}

// ShiftK is how far the target moves on a day whose mean price is dayMean:
// above zero on a cheap day, below on a dear one. An invalid rule, or a mean
// that is not a number, moves nothing.
func (c PriceComfort) ShiftK(dayMean float64) float64 {
	if !finite(dayMean) || c.Validate() != nil {
		return 0
	}
	var shift float64
	switch {
	case c.RaiseK > 0 && dayMean < c.NormalPrice:
		shift = c.RaiseK * math.Min(1, (c.NormalPrice-dayMean)/(c.NormalPrice-c.CheapPrice))
	case c.LowerK > 0 && dayMean > c.NormalPrice:
		shift = -c.LowerK * math.Min(1, (dayMean-c.NormalPrice)/(c.DearPrice-c.NormalPrice))
	}
	if c.StepK > 0 {
		// math.Round rounds halves away from zero, which is what a
		// thermostat's half-degree step should do in both directions.
		shift = math.Round(shift/c.StepK) * c.StepK
	}
	return shift
}

// Apply returns the zone with its target moved for a day whose mean price is
// dayMean. The band is the owner's hard limit and stays where it is: a
// target is moved inside it and no further. The zone passed in is not
// changed.
func (c PriceComfort) Apply(z Zone, dayMean float64) Zone {
	shift := c.ShiftK(dayMean)
	if shift == 0 {
		return z
	}
	target := make([]float64, len(z.TargetC))
	for i, t := range z.TargetC {
		t += shift
		if i < len(z.MinC) && t < z.MinC[i] {
			t = z.MinC[i]
		}
		if i < len(z.MaxC) && t > z.MaxC[i] {
			t = z.MaxC[i]
		}
		target[i] = t
	}
	z.TargetC = target
	return z
}

// DayMean is the mean of one day's prices, and whether enough of the day is
// known to call it that: at least minHours of slots of slotH hours each. A
// caller passes the slots of the day the comfort is for, the ones already
// past included, not the horizon from now.
func DayMean(price []float64, slotH, minHours float64) (mean float64, ok bool) {
	if !(slotH > 0) || len(price) == 0 || float64(len(price))*slotH < minHours {
		return 0, false
	}
	var sum float64
	for _, p := range price {
		if !finite(p) {
			return 0, false
		}
		sum += p
	}
	return sum / float64(len(price)), true
}

// SavingsGate is the owner's least saving for planned heat to be acted on.
type SavingsGate struct {
	// MinPerDay is what the plans must save together in a day, in the unit
	// of Request.Price times kWh. Zero or less opens the gate always.
	MinPerDay float64 `json:"min_per_day"`
	// KeepShare is the share of MinPerDay below which an open gate closes
	// again, so that a saving near the limit does not open and close it
	// every slot. From above 0 to 1; zero means 1, no hysteresis.
	KeepShare float64 `json:"keep_share,omitempty"`
}

// Validate refuses a gate that cannot be read one way.
func (g SavingsGate) Validate() error {
	if !finite(g.MinPerDay, g.KeepShare) || g.KeepShare < 0 || g.KeepShare > 1 {
		return errors.New("heatplan: the savings gate keeps a share of 0 to 1 of a finite limit")
	}
	return nil
}

// Saving is what the plan saves against the device left alone at the
// target. A plan that leaves its band saves nothing: it is not feasible,
// and what it seems to save is comfort it did not deliver.
func (p Plan) Saving() float64 {
	if p.BandExcursionKh > 0 || !finite(p.Cost, p.BaselineCost) {
		return 0
	}
	return math.Max(0, p.BaselineCost-p.Cost)
}

// Open reports whether the plans together save enough to act on, and what
// they save scaled to a day. horizonH is the horizon the plans cover; a
// horizon longer than a day is scaled down and a shorter one up, since a
// morning with half a day of prices would otherwise never reach a daily
// limit. wasOpen is the gate's last answer.
//
// A closed gate means every zone is left on its target, which with
// PriceComfort is the target that day is worth. An invalid gate, or a
// horizon that is not a length of time, is closed: the devices then run
// themselves, which is always safe.
func (g SavingsGate) Open(plans []Plan, horizonH float64, wasOpen bool) (open bool, perDay float64) {
	if g.Validate() != nil || !(horizonH > 0) || !finite(horizonH) {
		return false, 0
	}
	var saved float64
	for _, p := range plans {
		saved += p.Saving()
	}
	perDay = saved * 24 / horizonH
	if g.MinPerDay <= 0 {
		return true, perDay
	}
	limit := g.MinPerDay
	if wasOpen && g.KeepShare > 0 {
		limit *= g.KeepShare
	}
	return perDay >= limit, perDay
}
