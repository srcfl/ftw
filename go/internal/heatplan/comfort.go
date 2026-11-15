package heatplan

import (
	"errors"
	"math"
)

// This file holds the owner's rule for what heat is worth, beside the plan
// itself, and what a plan saves. The rule has no default in code: every
// number in it is the owner's, its zero value changes nothing, and a caller
// must not invent one.
//
// How warm the owner wants a zone depends on what the day costs. A cheap day
// may buy a warmer floor; a dear day may cost a cooler room. PriceComfort
// moves the target before planning, so the plan and its baseline both stand
// on the comfort that day is worth. It needs no model of the zone and holds
// from the first day.
//
// There is no least saving before a plan may act. What a plan saves is shown
// to the owner from the shadow run, and the owner decides with that number
// in view (ADR 0012, decision 9).

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
		// thermostat's half-degree step should do in both directions. It
		// never rounds past the owner's own limit: a raise of 2.5 K in
		// steps of 1 K is at most 2 K, not 3.
		// The small margin keeps 0.3 K in steps of 0.1 K at 0.3.
		const eps = 1e-9
		shift = math.Round(shift/c.StepK) * c.StepK
		if shift > c.RaiseK+eps {
			shift = math.Floor(c.RaiseK/c.StepK+eps) * c.StepK
		}
		if shift < -c.LowerK-eps {
			shift = -math.Floor(c.LowerK/c.StepK+eps) * c.StepK
		}
	}
	if shift == 0 {
		return 0 // not −0
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

// Saving is what the plan saves against the device left alone at the
// target, in the unit of Request.Price times kWh, over the plan's horizon.
// A plan that leaves its band saves nothing: it is not feasible, and what it
// seems to save is comfort it did not deliver.
func (p Plan) Saving() float64 {
	if p.BandExcursionKh > 0 || !finite(p.Cost, p.BaselineCost) {
		return 0
	}
	return math.Max(0, p.BaselineCost-p.Cost)
}
