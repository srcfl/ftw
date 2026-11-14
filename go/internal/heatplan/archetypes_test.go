package heatplan

import (
	"math"

	"github.com/srcfl/ftw/go/internal/thermal"
)

// Seven heated things with round, plausible numbers. They are fixtures for
// the tests, not defaults: a real zone's numbers come from its twin.

const (
	slotH = 0.25
	day   = 96
)

func flat(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// hours builds a per-slot series from a value per hour of day.
func hours(n int, f func(hour float64) float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = f(math.Mod(float64(i)*slotH, 24))
	}
	return out
}

func between(h, from, to float64) bool { return h >= from && h < to }

// eveningPeak is a flat price with one expensive evening.
func eveningPeak(n int, base, peak float64) []float64 {
	return hours(n, func(h float64) float64 {
		if between(h, 17, 20) {
			return peak
		}
		return base
	})
}

func room(n int, tauH, capacity, outdoorC float64) Zone {
	return Zone{
		TauH: tauH, CapacityKWhPerK: capacity, StartC: 21,
		BoundaryC: flat(n, outdoorC), TargetC: flat(n, 21), MinC: flat(n, 20), MaxC: flat(n, 22),
		ComfortWeight: 2,
	}
}

// A timber house on panel radiators.
func radiatorHouse(n int, price []float64) Request {
	return Request{SlotH: slotH, Price: price, OutdoorC: flat(n, 0),
		Zone: room(n, 40, 8, 0),
		Actuator: Actuator{Strategy: thermal.InfluenceSetpoint, Values: Values(20, 22, 0.5, 21, 8),
			MaxHeatKW: 8, Source: Source{Kind: SourceDirectElectric}}}
}

// The same house behind one air-to-air unit. The unit heats the air, and the
// air is what its sensor sees, so the zone is light and quick.
func airToAirHouse(n int, price, outdoor []float64) Request {
	z := room(n, 20, 3, 0)
	z.BoundaryC = outdoor
	return Request{SlotH: slotH, Price: price, OutdoorC: outdoor, Zone: z,
		Actuator: Actuator{Strategy: thermal.InfluenceSetpoint, Values: Values(20, 22, 0.5, 21, 8),
			MaxHeatKW: 5, Source: Source{Kind: SourceHeatPump, SinkApproachK: 18, FrostLoss: 0.1}}}
}

// A concrete slab fed by an air-to-water pump on a heating curve.
func slabHouse(n int, price, outdoor []float64) Request {
	z := room(n, 100, 20, 0)
	z.BoundaryC = outdoor
	return Request{SlotH: slotH, Price: price, OutdoorC: outdoor, Zone: z,
		Actuator: Actuator{Strategy: thermal.InfluenceCurveOffset, Values: Values(-3, 3, 1, 0, 8),
			ZoneKPerUnit: 0.4, SupplyKPerUnit: 2, MaxHeatKW: 8,
			Source: Source{Kind: SourceHeatPump, Supply: &SupplyCurve{ColdOutdoorC: -15, ColdSupplyC: 45, MildOutdoorC: 15, MildSupplyC: 25}}}}
}

// A tiled bathroom floor on a heating cable. hpCOP above zero puts it inside
// a house a heat pump keeps at 21 °C.
func bathroomFloor(n int, price []float64, hpCOP float64) Request {
	z := Zone{TauH: 12, CapacityKWhPerK: 0.6, StartC: 23,
		BoundaryC: flat(n, 21), TargetC: flat(n, 23), MinC: flat(n, 21.5), MaxC: flat(n, 26),
		ComfortWeight: 2}
	if hpCOP > 0 {
		z.LossCreditCOP = flat(n, hpCOP)
	}
	return Request{SlotH: slotH, Price: price, OutdoorC: flat(n, 0), Zone: z,
		Actuator: Actuator{Strategy: thermal.InfluenceSetpoint, Values: Values(21.5, 26, 0.5, 23, 12),
			MaxHeatKW: 0.8, Source: Source{Kind: SourceDirectElectric}}}
}

// A 300 l hot-water tank in a 20 °C utility room, drawn from morning and
// evening.
func hotWaterTank(n int, price []float64, src Source) Request {
	draws := hours(n, func(h float64) float64 {
		switch {
		case between(h, 7, 7.5):
			return -4
		case between(h, 19, 19.5):
			return -5
		}
		return 0
	})
	return Request{SlotH: slotH, Price: price, OutdoorC: flat(n, 0),
		Zone: Zone{TauH: 200, CapacityKWhPerK: 0.35, StartC: 55,
			BoundaryC: flat(n, 20), TargetC: flat(n, 55), MinC: flat(n, 50), MaxC: flat(n, 65),
			DisturbanceK: draws},
		Actuator: Actuator{Strategy: thermal.InfluenceSetpoint, Values: Values(50, 65, 2.5, 55, 8),
			MaxHeatKW: 3, Source: src}}
}

// A 1500 l covered spa outdoors, used 19–22 and let down in between.
func spa(n int, price []float64) Request {
	use := func(h float64) bool { return between(h, 19, 22) }
	return Request{SlotH: slotH, Price: price, OutdoorC: flat(n, 0),
		Zone: Zone{TauH: 200, CapacityKWhPerK: 1.74, StartC: 34,
			BoundaryC: flat(n, 0),
			TargetC:   hours(n, func(h float64) float64 { return map[bool]float64{true: 38, false: 34}[use(h)] }),
			MinC:      hours(n, func(h float64) float64 { return map[bool]float64{true: 37.5, false: 30}[use(h)] }),
			MaxC:      flat(n, 40)},
		Actuator: Actuator{Strategy: thermal.InfluenceSetpoint, Values: Values(30, 40, 1, 34, 12),
			MaxHeatKW: 3, Source: Source{Kind: SourceDirectElectric}}}
}

// A 40 m³ covered pool on its own air-source pump, which only starts and
// stops.
func pool(n int, price, outdoor []float64) Request {
	return Request{SlotH: slotH, Price: price, OutdoorC: outdoor,
		Zone: Zone{TauH: 300, CapacityKWhPerK: 46, StartC: 27,
			BoundaryC: outdoor, TargetC: flat(n, 27), MinC: flat(n, 26), MaxC: flat(n, 29)},
		Actuator: Actuator{Strategy: thermal.InfluenceOnOff, MaxHeatKW: 10,
			MinOnSlots: 2, MinOffSlots: 2,
			Source: Source{Kind: SourceHeatPump, CarnotFraction: 0.3, MaxCOP: 7}}}
}

// A summer day: 12 °C at dawn, 24 °C mid-afternoon.
func summerDay(n int) []float64 {
	return hours(n, func(h float64) float64 { return 18 - 6*math.Cos((h-3)*math.Pi/12) })
}

// A winter day: −6 °C at dawn, 2 °C mid-afternoon.
func winterDay(n int) []float64 {
	return hours(n, func(h float64) float64 { return -2 - 4*math.Cos((h-3)*math.Pi/12) })
}
