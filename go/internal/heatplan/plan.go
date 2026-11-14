// Package heatplan proposes when a heated zone should take its heat.
//
// It is the planner of docs/adr/0012: a dynamic program over the price and
// weather horizon whose state is the zone's predicted temperature and whose
// actions are the few values a declared control accepts. It proposes; it
// reads no device and sends nothing. Core checks every proposed value
// against the declared bounds, the lease and the minimum on and off times
// before anything is dispatched.
//
// One model covers every heated thing, because they differ in numbers and
// not in form:
//
//	C · dT/dt = Q − UA · (T − T_boundary) + other
//
// What differs, and what decides whether moving heat in time pays:
//
//   - What a kWh of heat costs. Direct electric heat costs the price. A heat
//     pump's heat costs the price divided by a COP that moves with the
//     outdoor air and with how hot the sink is (Source). Preheating through
//     a heat pump raises the sink and so costs efficiency; heating a tank or
//     a pool gets dearer the hotter it already is; an air-source pump is
//     cheapest in the warmest hours whatever the price does.
//   - How long the heat stays. τ = C/UA is hours for the air of a room
//     behind an air-to-air unit, days for a slab or a covered pool. The
//     planner preheats only as far ahead as the zone holds it.
//   - Where lost heat goes. A room loses to outdoors and the heat is gone.
//     A bathroom floor inside a house a heat pump keeps warm loses to the
//     rooms around it, where it replaces heat the pump would have made at
//     COP 3–4 (Zone.LossCreditCOP). Charging that floor with direct
//     electricity therefore rarely pays, and letting it coast costs less
//     than its own meter suggests.
//   - What the band is for. A room has a target and comfort on both sides
//     of it. A tank or a spa has no comfort inside its band, only a floor,
//     a ceiling, and times it must be ready by (per-slot bands).
package heatplan

import (
	"errors"
	"math"

	"github.com/srcfl/ftw/go/internal/thermal"
)

// ErrUnsupported is returned for a control the planner cannot yet reason
// about. The device then runs itself, as it does without a trusted twin.
var ErrUnsupported = errors.New("heatplan: control strategy not supported")

const (
	maxSlots  = 4 * 24 * 4
	maxValues = 16
	// The temperature grid. Fine enough that a 0.5 K setpoint step spans
	// several points, coarse enough for a Raspberry Pi to plan every zone
	// each quarter.
	minGridStepK = 0.02
	gridPoints   = 400
	// Being warmer than asked is a smaller fault than being colder, and the
	// extra loss already charges for it.
	overTargetShare = 0.25
	// Leaving the band is not a trade. The weight only has to dominate any
	// energy saving; the plan reports the excursion and the caller decides.
	bandWeightFactor = 1e3
	// A value other than the device's own must earn this share of the
	// do-nothing cost per slot, so equal-cost plans do not chatter.
	chatterShare = 1e-4
	// How far a kWh over the site's import limit outweighs the worst the
	// band could charge for the heat it carries. Large, because a control
	// offers few values: a sliver over the limit can buy a whole slot of
	// heat, and must still lose.
	limitWeightFactor = 1e3
)

// Zone is one heated thing as its twin describes it, in thermal terms.
type Zone struct {
	// TauH is the inertia C/UA: how long the zone coasts.
	TauH float64
	// CapacityKWhPerK is C: the heat one kelvin holds.
	CapacityKWhPerK float64
	// OtherGainKPerH is what neither loss nor the control explains: sun,
	// people, other heat. The twin's constant term.
	OtherGainKPerH float64
	// StartC is the measured temperature now.
	StartC float64

	// Per slot, each as long as the horizon.

	// BoundaryC is what the zone loses heat to: the outdoor forecast for a
	// house or an outdoor pool, the room a tank stands in, the rooms around
	// a bathroom floor.
	BoundaryC []float64
	// TargetC is what the owner asked for. MinC and MaxC are the hard band
	// around it. They vary by slot so a spa can be let down between uses
	// and a tank can have a floor.
	TargetC, MinC, MaxC []float64
	// DisturbanceK is a forecast change no heating explains: a hot-water
	// draw, a lid left open. Nil means none.
	DisturbanceK []float64
	// LossCreditCOP is the efficiency of the other source that heats the
	// space this zone loses to. Heat lost there is not lost: it saves that
	// source the same heat. Zero in a slot means the loss is gone, as it is
	// to outdoors or to a house nothing else is heating. Nil means zero.
	LossCreditCOP []float64

	// ComfortWeight prices distance from the target, as a multiple of what
	// holding the zone one kelvin warmer costs. Above 1, sitting below the
	// target never pays by itself and only moving heat between prices does.
	// Zero is a store with no comfort inside its band.
	ComfortWeight float64
}

// Actuator is the one declared control the plan moves, and the heat behind it.
type Actuator struct {
	// Strategy is the control's declared lever.
	//   setpoint      the value is the temperature the device regulates
	//                 the zone to.
	//   curve_offset  the device regulates the zone to its own target plus
	//                 value × ZoneKPerUnit, and its flow runs
	//                 value × SupplyKPerUnit hotter.
	//   on_off        1 runs the device at full output up to the band's
	//                 ceiling, 0 stops it.
	Strategy thermal.InfluenceStrategy
	// Values are the candidates, rising. For on_off they are ignored.
	Values []float64
	// MaxHeatKW is the heat at full output.
	MaxHeatKW float64
	Source    Source

	ZoneKPerUnit   float64
	SupplyKPerUnit float64

	// MinOnSlots and MinOffSlots are the declared protection times, in
	// slots, rounded up by the caller. on_off only.
	MinOnSlots, MinOffSlots int
	// On and RunSlots say what the device is doing now and for how long.
	On       bool
	RunSlots int
}

// Request is one zone, one control and one horizon.
type Request struct {
	// SlotH is the length of a slot in hours.
	SlotH float64
	// Price is the marginal cost of a kWh of electricity in each slot, in
	// any one currency: import price, or what export would have paid while
	// the site has surplus.
	Price []float64
	// OutdoorC is the outdoor forecast, for the heat pump's cold side and
	// its supply curve. It is not the zone's boundary unless the caller
	// says so in Zone.BoundaryC.
	OutdoorC []float64
	// MaxElecKW is how much electricity the zone may draw in each slot
	// before the site passes its import limit: a capacity tariff's peak, or
	// the fuse. Heat is planned inside the site's energy plan and never
	// through it, so this outranks the zone's own band: the plan lets the
	// zone leave its band before it proposes a value that draws more. A
	// control cannot enforce a power, so what the device's lowest value or
	// a compressor's minimum run still draws is reported, not hidden. Nil
	// means no limit.
	MaxElecKW []float64
	// SurplusKW is power the site would otherwise export in each slot, and
	// SurplusPrice what a kWh of it would have earned. The zone's first
	// SurplusKW cost SurplusPrice; the rest costs Price. Nil means none.
	SurplusKW, SurplusPrice []float64
	Zone                    Zone
	Actuator                Actuator
}

// Slot is the proposal for one slot.
type Slot struct {
	// Value is the control value to hold through the slot.
	Value float64 `json:"value"`
	// TempC is the predicted temperature at the slot's end.
	TempC   float64 `json:"temp_c"`
	HeatKWh float64 `json:"heat_kwh"`
	ElecKWh float64 `json:"elec_kwh"`
	// Cost is the electricity bought, less the credit for heat passed on.
	Cost float64 `json:"cost"`
}

// Plan is a proposal and what it was weighed against.
type Plan struct {
	Slots []Slot `json:"slots"`
	// Cost is the plan's energy cost over the horizon, credit included, with
	// the heat missing or left over at the end valued at what it would cost
	// to buy then. BaselineCost is the same for the device left alone at
	// the target.
	Cost         float64 `json:"cost"`
	BaselineCost float64 `json:"baseline_cost"`
	// BandExcursionKh is how far and how long the prediction leaves the
	// band. Above zero the zone cannot be held with this control and the
	// caller must not treat the plan as feasible.
	BandExcursionKh float64 `json:"band_excursion_kh"`
	// OverLimitKWh is the electricity predicted above MaxElecKW that no
	// declared value could avoid: the device's own regulation at its lowest
	// value, or a compressor's minimum run.
	OverLimitKWh float64 `json:"over_limit_kwh"`
}

// Values returns the candidates a number control offers between lo and hi
// in whole steps from lo, thinned evenly to at most limit, and always
// keeping keep (the device's own value) when it lies on a step.
func Values(lo, hi, step, keep float64, limit int) []float64 {
	if !(step > 0) || hi < lo || limit < 3 || !finite(lo, hi, step) {
		return nil
	}
	n := int(math.Floor((hi-lo)/step+1e-9)) + 1
	stride := 1
	// Room for the far end and the kept value beside the even picks.
	for n > limit && (n-1)/stride+1 > limit-2 {
		stride++
	}
	idx := map[int]bool{n - 1: true}
	for i := 0; i < n; i += stride {
		idx[i] = true
	}
	if k := math.Round((keep - lo) / step); k >= 0 && k < float64(n) && math.Abs(lo+k*step-keep) < 1e-6 {
		idx[int(k)] = true
	}
	out := make([]float64, 0, len(idx))
	for i := 0; i < n; i++ {
		if idx[i] {
			out = append(out, lo+float64(i)*step)
		}
	}
	return out
}

func (r Request) validate() error {
	n := len(r.Price)
	z, a := r.Zone, r.Actuator
	if n == 0 || n > maxSlots {
		return errors.New("heatplan: horizon must be 1 to 384 slots")
	}
	if !(r.SlotH > 0) || r.SlotH > 1 {
		return errors.New("heatplan: slot length must be within an hour")
	}
	for _, s := range [][]float64{r.OutdoorC, z.BoundaryC, z.TargetC, z.MinC, z.MaxC} {
		if len(s) != n {
			return errors.New("heatplan: every series must cover the horizon")
		}
	}
	for _, s := range [][]float64{z.DisturbanceK, z.LossCreditCOP, r.MaxElecKW, r.SurplusKW} {
		if s != nil && len(s) != n {
			return errors.New("heatplan: every series must cover the horizon")
		}
	}
	if (r.SurplusKW == nil) != (r.SurplusPrice == nil) || len(r.SurplusKW) != len(r.SurplusPrice) {
		return errors.New("heatplan: surplus needs both its power and its price")
	}
	for i := 0; i < n; i++ {
		if !finite(r.Price[i], r.OutdoorC[i], z.BoundaryC[i], z.TargetC[i], z.MinC[i], z.MaxC[i]) {
			return errors.New("heatplan: series must be finite")
		}
		if z.MinC[i] > z.TargetC[i] || z.TargetC[i] > z.MaxC[i] || z.MinC[i] < -30 || z.MaxC[i] > 100 {
			return errors.New("heatplan: target must lie inside its band")
		}
		if r.OutdoorC[i] < -60 || r.OutdoorC[i] > 60 || z.BoundaryC[i] < -60 || z.BoundaryC[i] > 100 {
			return errors.New("heatplan: temperature out of range")
		}
		if r.MaxElecKW != nil && (!finite(r.MaxElecKW[i]) || r.MaxElecKW[i] < 0) {
			return errors.New("heatplan: the electricity limit must be zero or more")
		}
		if r.SurplusKW != nil && (!finite(r.SurplusKW[i], r.SurplusPrice[i]) || r.SurplusKW[i] < 0) {
			return errors.New("heatplan: surplus must be zero or more")
		}
		if z.DisturbanceK != nil && !finite(z.DisturbanceK[i]) {
			return errors.New("heatplan: series must be finite")
		}
		if z.LossCreditCOP != nil && (!finite(z.LossCreditCOP[i]) || z.LossCreditCOP[i] < 0 ||
			(z.LossCreditCOP[i] > 0 && z.LossCreditCOP[i] < 1)) {
			return errors.New("heatplan: loss credit must be a COP of 1 or more, or zero")
		}
	}
	// The same envelope the twin calls physical, widened for a tank and a
	// pool: a spa holds 2 kWh/K, a pool 60.
	if !finite(z.TauH, z.CapacityKWhPerK, z.OtherGainKPerH, z.StartC, z.ComfortWeight) ||
		z.TauH < 1 || z.TauH > 5000 || z.CapacityKWhPerK < 0.05 || z.CapacityKWhPerK > 1000 ||
		math.Abs(z.OtherGainKPerH) > 3 || z.StartC < -30 || z.StartC > 100 ||
		z.ComfortWeight < 0 || z.ComfortWeight > 100 {
		return errors.New("heatplan: zone model is not physical")
	}
	if !finite(a.MaxHeatKW) || a.MaxHeatKW <= 0 || a.MaxHeatKW > 500 {
		return errors.New("heatplan: heat output out of range")
	}
	if err := a.Source.validate(); err != nil {
		return err
	}
	switch a.Strategy {
	case thermal.InfluenceOnOff:
		if a.MinOnSlots < 0 || a.MinOffSlots < 0 || a.MinOnSlots > 48 || a.MinOffSlots > 48 || a.RunSlots < 0 {
			return errors.New("heatplan: minimum on and off times out of range")
		}
	case thermal.InfluenceSetpoint, thermal.InfluenceCurveOffset:
		if len(a.Values) == 0 || len(a.Values) > maxValues {
			return errors.New("heatplan: a control needs 1 to 16 candidate values")
		}
		for i, v := range a.Values {
			if !finite(v) || (i > 0 && v <= a.Values[i-1]) {
				return errors.New("heatplan: candidate values must be finite and rising")
			}
		}
		if a.Strategy == thermal.InfluenceCurveOffset {
			if !finite(a.ZoneKPerUnit, a.SupplyKPerUnit) || a.ZoneKPerUnit <= 0 || a.ZoneKPerUnit > 5 ||
				a.SupplyKPerUnit < 0 || a.SupplyKPerUnit > 10 {
				return errors.New("heatplan: a curve offset needs its learned effect on the zone")
			}
		}
	default:
		return ErrUnsupported
	}
	return nil
}

// action is one thing the planner may ask for in a slot, reduced to what the
// device then does: regulate the zone toward a temperature, with its flow
// shifted.
type action struct {
	value   float64
	on      bool // on_off only
	supplyK float64
	// setpoint returns the temperature the device regulates to in slot k.
	setpoint func(k int) float64
}

type solver struct {
	r     Request
	n     int
	decay float64 // e^(−h/τ)
	a     float64 // 1/τ
	acts  []action
	// neutral is the index of the action the device takes by itself in a
	// slot: its own target.
	neutral func(k int) int

	comfortPerKh float64
	bandPerKh    float64
	limitPerKWh  float64
	chatter      float64

	lo, step float64
	points   int
}

// Propose plans the horizon. An error means the request is not one the
// planner can answer; the device then runs itself.
func Propose(r Request) (Plan, error) {
	if err := r.validate(); err != nil {
		return Plan{}, err
	}
	s := newSolver(r)
	plan := s.run(false)
	base := s.run(true)
	plan.BaselineCost = base.Cost
	return plan, nil
}

func newSolver(r Request) *solver {
	z, a := r.Zone, r.Actuator
	s := &solver{r: r, n: len(r.Price), a: 1 / z.TauH}
	s.decay = math.Exp(-r.SlotH * s.a)

	switch a.Strategy {
	case thermal.InfluenceOnOff:
		s.acts = []action{
			{value: 0, setpoint: func(int) float64 { return math.Inf(-1) }},
			{value: 1, on: true, setpoint: func(k int) float64 { return z.MaxC[k] }},
		}
	case thermal.InfluenceSetpoint:
		for _, v := range a.Values {
			v := v
			s.acts = append(s.acts, action{value: v, setpoint: func(int) float64 { return v }})
		}
	case thermal.InfluenceCurveOffset:
		for _, v := range a.Values {
			v := v
			s.acts = append(s.acts, action{value: v, supplyK: v * a.SupplyKPerUnit,
				setpoint: func(k int) float64 { return z.TargetC[k] + v*a.ZoneKPerUnit }})
		}
	}
	// The device's own choice: the candidate that regulates closest to the
	// target. For on_off there is none; its thermostat is not a candidate.
	s.neutral = func(k int) int {
		if a.Strategy == thermal.InfluenceOnOff {
			return -1
		}
		best, bestD := 0, math.Inf(1)
		for i, act := range s.acts {
			if d := math.Abs(act.setpoint(k) - z.TargetC[k]); d < bestD {
				best, bestD = i, d
			}
		}
		return best
	}

	// What a kWh of heat costs at the target, on average over the horizon.
	var heatPrice float64
	for k := 0; k < s.n; k++ {
		heatPrice += math.Max(0, r.Price[k]) / s.cop(k, z.TargetC[k], 0)
	}
	heatPrice /= float64(s.n)
	holdPerKh := s.holdCost(heatPrice)
	s.comfortPerKh = z.ComfortWeight * holdPerKh
	s.bandPerKh = bandWeightFactor * (heatPrice*z.CapacityKWhPerK + holdPerKh + 1e-9)
	s.chatter = chatterShare * holdPerKh * r.SlotH
	// The site's import limit outranks the band. A kWh held back costs the
	// zone at most 1/C kelvin outside its band for the rest of the horizon;
	// a kWh over the limit must cost more than that.
	s.limitPerKWh = limitWeightFactor * s.bandPerKh * float64(s.n) * r.SlotH / z.CapacityKWhPerK

	lo, hi := z.StartC, z.StartC
	for k := 0; k < s.n; k++ {
		lo, hi = math.Min(lo, z.MinC[k]), math.Max(hi, z.MaxC[k])
	}
	margin := math.Max(1, 0.1*(hi-lo))
	lo, hi = lo-margin, hi+margin
	s.step = math.Max(minGridStepK, (hi-lo)/gridPoints)
	s.points = int(math.Ceil((hi-lo)/s.step)) + 1
	s.lo = lo
	return s
}

// holdCost is what regulating the zone one kelvin warmer for an hour costs,
// averaged over the horizon. Comfort is priced in multiples of it, so it has
// to count everything a warmer zone costs: the extra loss, the worse COP of a
// heat pump whose sink is hotter for all the heat it delivers, and, against
// those, the credit for loss that warms space another source heats.
func (s *solver) holdCost(heatPrice float64) float64 {
	z, a := s.r.Zone, s.r.Actuator
	supplyPerK := 0.0
	if a.Strategy == thermal.InfluenceCurveOffset {
		supplyPerK = a.SupplyKPerUnit / a.ZoneKPerUnit
	}
	steady := func(k int, temp, supplyK float64) float64 {
		loss := z.CapacityKWhPerK * s.a * (temp - z.BoundaryC[k])
		heat := math.Max(0, loss-z.CapacityKWhPerK*z.OtherGainKPerH)
		cost := heat / s.cop(k, temp, supplyK)
		if z.LossCreditCOP != nil && z.LossCreditCOP[k] > 0 {
			cost -= loss / z.LossCreditCOP[k]
		}
		return cost * math.Max(0, s.r.Price[k])
	}
	var hold float64
	for k := 0; k < s.n; k++ {
		hold += steady(k, z.TargetC[k]+1, supplyPerK) - steady(k, z.TargetC[k], 0)
	}
	hold /= float64(s.n)
	// A zone that costs nothing to hold, such as one warmed by other gains
	// all day, still needs a scale for comfort.
	return math.Max(hold, 0.1*heatPrice*z.CapacityKWhPerK*s.a)
}

// cop is the source's efficiency in slot k with the zone at zoneC.
func (s *solver) cop(k int, zoneC, supplyK float64) float64 {
	src := s.r.Actuator.Source
	sink := zoneC
	if src.Supply != nil {
		sink = src.Supply.At(s.r.OutdoorC[k]) + supplyK
	}
	return src.COP(s.r.OutdoorC[k], sink)
}

type step struct {
	next, heatKWh, elecKWh, overKWh, cost, penalty float64
}

// advance moves the zone one slot from temp under an action. The first-order
// model is solved exactly over the slot, so a zone with little inertia does
// not overshoot the way a forward difference would.
func (s *solver) advance(k int, temp float64, act action) step {
	z, h := s.r.Zone, s.r.SlotH
	c := z.CapacityKWhPerK
	// The heat that lands the zone on the setpoint at the slot's end,
	// within what the device can give.
	sp := act.setpoint(k)
	var q float64
	if !math.IsInf(sp, -1) {
		eq := (sp - temp*s.decay) / (1 - s.decay)
		q = c * (s.a*(eq-z.BoundaryC[k]) - z.OtherGainKPerH)
		q = math.Max(0, math.Min(s.r.Actuator.MaxHeatKW, q))
	}
	eq := z.BoundaryC[k] + (q/c+z.OtherGainKPerH)/s.a
	next := eq + (temp-eq)*s.decay
	mid := (temp + next) / 2
	if z.DisturbanceK != nil {
		next += z.DisturbanceK[k]
	}

	out := step{next: next, heatKWh: q * h}
	out.elecKWh = out.heatKWh / s.cop(k, mid, act.supplyK)
	out.cost = out.elecKWh * s.r.Price[k]
	if s.r.SurplusKW != nil {
		// What the site would have exported is used first, at what it
		// would have earned.
		own := math.Min(out.elecKWh, s.r.SurplusKW[k]*h)
		out.cost += own * (s.r.SurplusPrice[k] - s.r.Price[k])
	}
	if s.r.MaxElecKW != nil {
		out.overKWh = math.Max(0, out.elecKWh-s.r.MaxElecKW[k]*h)
		out.penalty += s.limitPerKWh * out.overKWh
	}
	if z.LossCreditCOP != nil && z.LossCreditCOP[k] > 0 {
		// Heat passed to the space around the zone saves the source that
		// heats that space the same heat. A zone colder than its
		// surroundings takes heat from that source instead, and pays.
		lossKWh := c * s.a * (mid - z.BoundaryC[k]) * h
		out.cost -= lossKWh * s.r.Price[k] / z.LossCreditCOP[k]
	}

	switch {
	case mid < z.TargetC[k]:
		out.penalty += s.comfortPerKh * (z.TargetC[k] - mid) * h
	default:
		out.penalty += overTargetShare * s.comfortPerKh * (mid - z.TargetC[k]) * h
	}
	out.penalty += s.bandPerKh * s.excursion(k, next) * h
	return out
}

func (s *solver) excursion(k int, temp float64) float64 {
	z := s.r.Zone
	return math.Max(0, z.MinC[k]-temp) + math.Max(0, temp-z.MaxC[k])
}

// terminal values the heat missing or left over at the horizon's end, so a
// plan cannot profit by ending cold or lose by ending warm. Missing heat is
// bought at the mean of the last hours; heat left over is worth only the
// cheapest of them, since that is when it would otherwise have been bought.
func (s *solver) terminal(temp float64) float64 {
	z := s.r.Zone
	tail := s.n / 4
	if tail < 1 {
		tail = 1
	}
	mean, min := 0.0, math.Inf(1)
	for k := s.n - tail; k < s.n; k++ {
		p := math.Max(0, s.r.Price[k]) / s.cop(k, z.TargetC[k], 0)
		mean += p
		min = math.Min(min, p)
	}
	mean /= float64(tail)
	d := z.TargetC[s.n-1] - temp
	if d > 0 {
		return d * z.CapacityKWhPerK * mean
	}
	return d * z.CapacityKWhPerK * min
}

// Run state for on_off: index 0..MinOff−1 is off for that many slots plus
// one, capped; then MinOn states for on. Other strategies have one state.
func (s *solver) runStates() (off, on int) {
	a := s.r.Actuator
	if a.Strategy != thermal.InfluenceOnOff {
		return 1, 0
	}
	return max(1, a.MinOffSlots), max(1, a.MinOnSlots)
}

// allowed says whether an action may follow run state rs, and the state it
// leads to.
func (s *solver) allowed(rs int, act action) (int, bool) {
	off, on := s.runStates()
	if on == 0 {
		return 0, true
	}
	if rs < off { // off for rs+1 slots, capped at off
		if !act.on {
			return min(rs+1, off-1), true
		}
		return off, rs == off-1
	}
	held := rs - off // on for held+1 slots, capped at on
	if act.on {
		return off + min(held+1, on-1), true
	}
	return 0, held == on-1
}

func (s *solver) startState() int {
	off, on := s.runStates()
	if on == 0 {
		return 0
	}
	a := s.r.Actuator
	run := max(1, a.RunSlots)
	if a.On {
		return off + min(run, on) - 1
	}
	return min(run, off) - 1
}

func (s *solver) interp(v []float64, temp float64) float64 {
	x := (temp - s.lo) / s.step
	i := int(math.Floor(x))
	if i < 0 {
		i = 0
	}
	if i > s.points-2 {
		i = s.points - 2
	}
	return v[i] + (v[i+1]-v[i])*(x-float64(i))
}

// run solves the horizon. With baseline set it follows the device's own
// choice in every slot instead of choosing.
func (s *solver) run(baseline bool) Plan {
	off, on := s.runStates()
	states := off + on

	pick := func(k, rs int, temp float64, value [][]float64) (int, step, int) {
		if baseline {
			i := s.neutral(k)
			if i < 0 {
				// A device with only start and stop keeps its own
				// thermostat: it holds the target.
				act := action{setpoint: func(k int) float64 { return s.r.Zone.TargetC[k] }}
				return -1, s.advance(k, temp, act), rs
			}
			return i, s.advance(k, temp, s.acts[i]), rs
		}
		best, bestCost, bestStep, bestNext := -1, math.Inf(1), step{}, 0
		neutral := s.neutral(k)
		for i, act := range s.acts {
			next, ok := s.allowed(rs, act)
			if !ok {
				continue
			}
			st := s.advance(k, temp, act)
			cost := st.cost + st.penalty + s.interp(value[next], st.next)
			if i != neutral {
				cost += s.chatter
			}
			if cost < bestCost {
				best, bestCost, bestStep, bestNext = i, cost, st, next
			}
		}
		return best, bestStep, bestNext
	}

	var values [][][]float64
	if !baseline {
		values = make([][][]float64, s.n+1)
		last := make([][]float64, states)
		for rs := range last {
			last[rs] = make([]float64, s.points)
			for i := range last[rs] {
				last[rs][i] = s.terminal(s.lo + float64(i)*s.step)
			}
		}
		values[s.n] = last
		for k := s.n - 1; k >= 0; k-- {
			cur := make([][]float64, states)
			for rs := range cur {
				cur[rs] = make([]float64, s.points)
				for i := range cur[rs] {
					temp := s.lo + float64(i)*s.step
					j, st, next := pick(k, rs, temp, values[k+1])
					if j < 0 {
						cur[rs][i] = math.Inf(1)
						continue
					}
					cur[rs][i] = st.cost + st.penalty + s.interp(values[k+1][next], st.next)
					if j != s.neutral(k) {
						cur[rs][i] += s.chatter
					}
				}
			}
			values[k] = cur
		}
	}

	plan := Plan{Slots: make([]Slot, 0, s.n)}
	temp, rs := s.r.Zone.StartC, s.startState()
	for k := 0; k < s.n; k++ {
		var next [][]float64
		if !baseline {
			next = values[k+1]
		}
		j, st, nrs := pick(k, rs, temp, next)
		value := s.r.Zone.TargetC[k]
		if j >= 0 {
			value = s.acts[j].value
		}
		temp, rs = st.next, nrs
		plan.Slots = append(plan.Slots, Slot{Value: value, TempC: temp, HeatKWh: st.heatKWh, ElecKWh: st.elecKWh, Cost: st.cost})
		plan.Cost += st.cost
		plan.BandExcursionKh += s.excursion(k, temp) * s.r.SlotH
		plan.OverLimitKWh += st.overKWh
	}
	plan.Cost += s.terminal(temp)
	return plan
}
