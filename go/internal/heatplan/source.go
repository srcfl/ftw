package heatplan

import (
	"errors"
	"math"
	"sort"
)

// SourceKind says how a device turns electricity into heat.
type SourceKind string

const (
	// SourceDirectElectric is a resistive heater: a floor cable, a panel
	// radiator, an immersion heater, a spa heater. One kWh in, one kWh out,
	// whatever the weather and whatever it heats.
	SourceDirectElectric SourceKind = "direct_electric"
	// SourceHeatPump moves heat. What a kWh of electricity delivers depends
	// on how far the heat is lifted: from the cold side (outdoor air, ground)
	// to the sink (a room, a flow temperature, a tank, a pool).
	SourceHeatPump SourceKind = "heat_pump"
)

// Defaults for a heat pump whose driver declares nothing more than its kind.
// They are a starting point, not a claim about any device: 50 % of the Carnot
// limit and 5 K across each heat exchanger give 3.5 at 0 °C outdoor and 35 °C
// flow, 2.3 at −15/45 and 2.6 for hot water at 55 °C, the range the
// manufacturers' own tables sit in.
const (
	defaultCarnotFraction = 0.5
	defaultApproachK      = 5.0
	defaultMaxCOP         = 6.0
	minLiftK              = 8.0
)

// COPPoint is one declared operating point: the COP at an outdoor
// temperature, with the sink at Source.CurveSinkC.
type COPPoint struct {
	OutdoorC float64
	COP      float64
}

// SupplyCurve is a hydronic heat pump's weather compensation: the flow
// temperature it runs at for an outdoor temperature, linear between two
// points and held flat outside them.
type SupplyCurve struct {
	ColdOutdoorC float64 // e.g. −15
	ColdSupplyC  float64 // e.g. 45
	MildOutdoorC float64 // e.g. 15
	MildSupplyC  float64 // e.g. 25
}

// At is the flow temperature at an outdoor temperature.
func (c SupplyCurve) At(outdoorC float64) float64 {
	if outdoorC <= c.ColdOutdoorC {
		return c.ColdSupplyC
	}
	if outdoorC >= c.MildOutdoorC {
		return c.MildSupplyC
	}
	f := (outdoorC - c.ColdOutdoorC) / (c.MildOutdoorC - c.ColdOutdoorC)
	return c.ColdSupplyC + f*(c.MildSupplyC-c.ColdSupplyC)
}

// Source is the efficiency of one heat input.
type Source struct {
	Kind SourceKind

	// The rest describes a heat pump and is ignored for direct electric.

	// ColdSideC is the temperature the pump takes heat from when that is
	// not outdoor air: brine from the ground, exhaust air. Nil means
	// outdoor air, which moves with the forecast and frosts.
	ColdSideC *float64
	// Supply is the flow temperature of a hydronic pump. Nil means the
	// pump heats the zone itself and the sink is the zone's own
	// temperature: the room for air-to-air, the water for a tank or a pool.
	Supply *SupplyCurve
	// SinkApproachK is how much hotter the condenser runs than the sink.
	// Zero means 5 K, right for water. An air-to-air unit blows room air
	// over its coil and needs 15–20 K.
	SinkApproachK float64
	// CarnotFraction is the share of the ideal COP the pump reaches. Zero
	// means 0.5. Unused when Curve is declared.
	CarnotFraction float64
	// Curve is the pump's declared COP by outdoor temperature, measured
	// with the sink at CurveSinkC. It replaces CarnotFraction; a sink at
	// another temperature scales it by the ratio of the ideal COPs.
	Curve      []COPPoint
	CurveSinkC float64
	// FrostLoss is the share of output an air-source pump loses to
	// defrosting at its worst, around +1 °C in damp air. It fades to
	// nothing at −7 and +6 °C. Zero means the curve already includes it.
	FrostLoss float64
	// MaxCOP caps the result. Zero means 6.
	MaxCOP float64
}

func (s Source) validate() error {
	switch s.Kind {
	case SourceDirectElectric:
		return nil
	case SourceHeatPump:
	default:
		return errors.New("heatplan: unknown source kind")
	}
	if s.SinkApproachK < 0 || s.SinkApproachK > 40 || s.CarnotFraction < 0 || s.CarnotFraction > 1 ||
		s.FrostLoss < 0 || s.FrostLoss > 0.5 || s.MaxCOP < 0 || !finite(s.SinkApproachK, s.CarnotFraction, s.FrostLoss, s.MaxCOP) {
		return errors.New("heatplan: heat pump efficiency out of range")
	}
	if s.ColdSideC != nil && (!finite(*s.ColdSideC) || *s.ColdSideC < -30 || *s.ColdSideC > 40) {
		return errors.New("heatplan: cold side temperature out of range")
	}
	if c := s.Supply; c != nil {
		if !finite(c.ColdOutdoorC, c.ColdSupplyC, c.MildOutdoorC, c.MildSupplyC) ||
			c.MildOutdoorC <= c.ColdOutdoorC || c.ColdSupplyC < c.MildSupplyC || c.MildSupplyC < 15 || c.ColdSupplyC > 80 {
			return errors.New("heatplan: supply curve is not a heating curve")
		}
	}
	for i, p := range s.Curve {
		if !finite(p.OutdoorC, p.COP) || p.COP < 1 || p.COP > 10 {
			return errors.New("heatplan: declared COP out of range")
		}
		if i > 0 && p.OutdoorC <= s.Curve[i-1].OutdoorC {
			return errors.New("heatplan: declared COP points must rise in outdoor temperature")
		}
	}
	if len(s.Curve) > 0 && (!finite(s.CurveSinkC) || s.CurveSinkC < 5 || s.CurveSinkC > 80) {
		return errors.New("heatplan: declared COP needs the sink temperature it was measured at")
	}
	return nil
}

// COP is the heat delivered per unit of electricity with the outdoor air at
// outdoorC and the sink at sinkC. It is never below 1: a pump that cannot
// beat its own backup heater is running on it.
func (s Source) COP(outdoorC, sinkC float64) float64 {
	if s.Kind != SourceHeatPump {
		return 1
	}
	cold := outdoorC
	if s.ColdSideC != nil {
		cold = *s.ColdSideC
	}
	var cop float64
	if len(s.Curve) > 0 {
		cop = interpCOP(s.Curve, outdoorC) * s.ideal(sinkC, cold) / s.ideal(s.CurveSinkC, cold)
	} else {
		frac := s.CarnotFraction
		if frac == 0 {
			frac = defaultCarnotFraction
		}
		cop = frac * s.ideal(sinkC, cold)
	}
	if s.ColdSideC == nil && s.FrostLoss > 0 {
		cop *= 1 - s.FrostLoss*frostWeight(outdoorC)
	}
	max := s.MaxCOP
	if max == 0 {
		max = defaultMaxCOP
	}
	return math.Max(1, math.Min(max, cop))
}

// ideal is the Carnot COP between the evaporator and the condenser, each a
// heat exchanger's approach away from what it exchanges with.
func (s Source) ideal(sinkC, coldC float64) float64 {
	approach := s.SinkApproachK
	if approach == 0 {
		approach = defaultApproachK
	}
	condenser := sinkC + approach
	evaporator := coldC - defaultApproachK
	return (condenser + 273.15) / math.Max(minLiftK, condenser-evaporator)
}

// frostWeight is 1 where an air coil ices fastest and 0 where it does not:
// below −7 °C the air is too dry, above +6 °C the coil stays above freezing.
func frostWeight(outdoorC float64) float64 {
	switch {
	case outdoorC <= -7 || outdoorC >= 6:
		return 0
	case outdoorC < 1:
		return (outdoorC + 7) / 8
	default:
		return (6 - outdoorC) / 5
	}
}

func interpCOP(curve []COPPoint, outdoorC float64) float64 {
	i := sort.Search(len(curve), func(i int) bool { return curve[i].OutdoorC >= outdoorC })
	switch {
	case i == 0:
		return curve[0].COP
	case i == len(curve):
		return curve[len(curve)-1].COP
	}
	a, b := curve[i-1], curve[i]
	return a.COP + (b.COP-a.COP)*(outdoorC-a.OutdoorC)/(b.OutdoorC-a.OutdoorC)
}

func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
