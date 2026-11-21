package heatplan

import (
	"errors"
	"math"
	"sort"
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
// Two more rules are the owner's own number for when a saving is worth a
// change of comfort or a write to a device. Both are off at zero.
//
//   - SavingsGate: the least a day's plans must save together before they are
//     acted on. Below it every zone stays on its target. What a plan saves is
//     also shown to the owner from the shadow run; the gate is the owner's way
//     to say what a saving has to be worth to them.
//   - PeakRelief: a short, sharp price peak the owner will sit through at the
//     bottom of the band, with how sharp and how long as the owner's numbers.

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

// SavingsGate is the owner's least saving for planned heat to be acted on.
// It is a choice about what a saving is worth to this household, so it has
// no default: zero opens the gate always.
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

// Open reports whether the plans together save enough to act on, and what
// they save scaled to a day. horizonH is the horizon the plans cover; a
// horizon longer than a day is scaled down and a shorter one up, since a
// morning with half a day of prices would otherwise never reach a daily
// limit. wasOpen is the gate's last answer.
//
// The plans passed in are the plans of every zone the gate covers, each
// counted once: a limit is the household's, and a limit compared with one
// room's saving would read as never reached.
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

// PeakRelief lets a zone sit at the bottom of its band through a short price
// peak. A slot is in a peak when its price is at least MinPrice and at least
// Factor times the lowest price in the WindowH hours from it, itself
// included. Of every unbroken stretch of such slots only the dearest
// MaxHours are relieved, so a long dear period is not one peak: its
// beginning is left to the plan. With a price that is zero or negative
// somewhere in the window, the ratio holds for any slot at or above MinPrice.
//
// It is the owner's rule and has no default: MaxHours zero turns it off.
type PeakRelief struct {
	// MinPrice is the price below which no slot is a peak, in the unit of
	// Request.Price.
	MinPrice float64 `json:"min_price"`
	// Factor is how many times the window's lowest price a peak costs. Above 1.
	Factor float64 `json:"factor"`
	// WindowH is how far ahead the lowest price is looked for, in hours.
	WindowH float64 `json:"window_h"`
	// MaxHours is the longest relief in one peak.
	MaxHours float64 `json:"max_hours"`
}

// Enabled reports whether the rule relieves anything.
func (r PeakRelief) Enabled() bool { return r.MaxHours > 0 }

// Validate refuses a rule that cannot be read one way. A rule that is off
// needs no other number.
func (r PeakRelief) Validate() error {
	if !finite(r.MinPrice, r.Factor, r.WindowH, r.MaxHours) || r.MaxHours < 0 {
		return errors.New("heatplan: peak relief must be finite, and its longest relief not negative")
	}
	if r.MaxHours == 0 {
		return nil
	}
	if r.MaxHours > 24 || !(r.Factor > 1) || !(r.WindowH > 0) || r.WindowH > 48 {
		return errors.New("heatplan: peak relief needs a factor above 1, a window of up to 48 hours and a longest relief of up to 24 hours")
	}
	return nil
}

// Slots marks the slots of price that the rule relieves. An invalid or
// disabled rule, or a price that is not finite, marks none.
func (r PeakRelief) Slots(price []float64, slotH float64) []bool {
	out := make([]bool, len(price))
	if !r.Enabled() || r.Validate() != nil || !(slotH > 0) || !finite(slotH) {
		return out
	}
	for _, p := range price {
		if !finite(p) {
			return out
		}
	}
	window := int(math.Ceil(r.WindowH/slotH - 1e-9))
	keep := int(math.Floor(r.MaxHours/slotH + 1e-9))
	if window < 1 || keep < 1 {
		return out
	}
	n := len(price)
	peak := make([]bool, n)
	for k, p := range price {
		if p < r.MinPrice {
			continue
		}
		end := min(n, k+window)
		low := price[k]
		for _, q := range price[k:end] {
			low = math.Min(low, q)
		}
		peak[k] = low <= 0 || p >= r.Factor*low
	}
	for k := 0; k < n; {
		if !peak[k] {
			k++
			continue
		}
		j := k
		for j < n && peak[j] {
			j++
		}
		// The dearest slots of the stretch; the earlier one wins a tie.
		idx := make([]int, 0, j-k)
		for i := k; i < j; i++ {
			idx = append(idx, i)
		}
		sort.SliceStable(idx, func(a, b int) bool { return price[idx[a]] > price[idx[b]] })
		for rank, i := range idx {
			if rank < keep {
				out[i] = true
			}
		}
		k = j
	}
	return out
}

// Apply returns the zone with its target let down to the bottom of its band
// in the relieved slots. The band is the owner's hard limit and stays where
// it is. Apply it after PriceComfort, which moves the target for the day.
// The zone passed in is not changed.
func (r PeakRelief) Apply(z Zone, price []float64, slotH float64) Zone {
	relieved := r.Slots(price, slotH)
	changed := false
	target := make([]float64, len(z.TargetC))
	copy(target, z.TargetC)
	for i := range target {
		if i < len(relieved) && relieved[i] && i < len(z.MinC) && z.MinC[i] < target[i] {
			target[i] = z.MinC[i]
			changed = true
		}
	}
	if !changed {
		return z
	}
	z.TargetC = target
	return z
}
