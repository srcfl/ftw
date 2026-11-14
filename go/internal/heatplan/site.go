package heatplan

import (
	"math"
	"sort"
)

// SiteSlot is what the site's own plan says about one slot, before this
// zone's heat: what Core's battery and solar plan already decided.
type SiteSlot struct {
	// ImportPrice is the full cost of a kWh from the grid; ExportPrice
	// what a kWh sent to it earns.
	ImportPrice, ExportPrice float64
	// GridKW is the planned exchange with the grid, positive into the
	// site, after solar, the battery and the car.
	GridKW float64
	// BatteryDischargeKW is what the battery is planned to give in the
	// slot, zero or more. BatterySpareKW is how much more it could give:
	// its power limit less the plan.
	BatteryDischargeKW, BatterySpareKW float64
}

// Site sets the request's prices, surplus and import limit from the site's
// plan. importLimitKW is the most the site may import; zero or less means no
// limit. Set the Actuator first: its output bounds what the zone can take.
//
// The site's plan comes first and heat fits around it. Surplus is only what
// the plan still exports after the battery and the car, and headroom only
// what it leaves under the limit.
//
// A battery holding energy does not make a slot cheap. The battery follows
// the site's load, so heat moved into a slot where it discharges is carried
// by the battery, which then has less for later: the site buys that energy
// back in the cheapest slot the battery was still to cover. Heat is charged
// that price, not the price the battery was charged at. The energy on offer
// at it is only what the battery was to spend in those later, cheaper
// slots, kept by batteryShare for a forecast that runs short; past it, and
// wherever nothing cheaper follows, heat costs the import price.
func (r *Request) Site(site []SiteSlot, importLimitKW float64) {
	n := len(site)
	r.Price, r.SurplusKW, r.SurplusPrice = make([]float64, n), make([]float64, n), make([]float64, n)
	r.MaxElecKW = nil
	if importLimitKW > 0 {
		r.MaxElecKW = make([]float64, n)
	}
	fromBattery, buyBack := batteryOffer(site, r.Actuator.MaxHeatKW)
	for k, s := range site {
		r.Price[k] = s.ImportPrice
		r.SurplusPrice[k] = s.ImportPrice
		headroom := importLimitKW - s.GridKW
		switch {
		case s.GridKW < 0:
			r.SurplusKW[k] = -s.GridKW
			// Export never earns more than import costs; a slot that
			// claims so would be heated with imported power called
			// surplus.
			r.SurplusPrice[k] = math.Min(s.ExportPrice, s.ImportPrice)
		case fromBattery[k] > 0:
			r.SurplusKW[k] = fromBattery[k]
			r.SurplusPrice[k] = buyBack[k]
			// What the battery carries does not come through the meter.
			headroom += fromBattery[k]
		}
		if r.MaxElecKW != nil {
			r.MaxElecKW[k] = math.Max(0, headroom)
		}
	}
}

// batteryShare is how much of the energy the battery was to spend in its
// cheapest covered slots heat may count on. The rest is left for a house
// that uses more, or a battery that holds less, than the site's plan
// forecast: heat that counts on energy which is then not there has moved
// load into the dearest slots at the import price.
const batteryShare = 0.5

// batteryOffer finds the power heat may take from the battery in each slot,
// and the price it pays for it. The battery's energy is finite: what heat
// takes in one slot the battery cannot give in a later one, and when the
// site plans again it drops the cheapest slot it still meant to cover. So a
// slot is offered energy only from cheaper slots that come after it, at
// their price. A slot that gives is not also offered energy.
func batteryOffer(site []SiteSlot, zoneKW float64) (kw, price []float64) {
	kw, price = make([]float64, len(site)), make([]float64, len(site))
	type covered struct {
		slot              int
		price, give, want float64
		gave              bool
	}
	var c []covered
	for k, s := range site {
		if s.GridKW < 0 || !(s.BatteryDischargeKW > 0) {
			continue
		}
		want := math.Max(0, s.BatterySpareKW)
		if zoneKW > 0 {
			want = math.Min(want, zoneKW)
		}
		c = append(c, covered{slot: k, price: s.ImportPrice, give: batteryShare * s.BatteryDischargeKW, want: want})
	}
	givers := make([]int, len(c))
	for i := range givers {
		givers[i] = i
	}
	sort.SliceStable(givers, func(i, j int) bool { return c[givers[i]].price < c[givers[j]].price })
	for _, gi := range givers {
		g := &c[gi]
		if kw[g.slot] > 0 {
			continue
		}
		// The dearest earlier slots first, a price level at a time.
		for left := g.give; left > 1e-12; {
			level, room := g.price, 0.0
			for _, t := range c[:gi] {
				if t.gave || t.price <= g.price || t.want-kw[t.slot] <= 1e-12 {
					continue
				}
				switch {
				case t.price > level:
					level, room = t.price, t.want-kw[t.slot]
				case t.price == level:
					room += t.want - kw[t.slot]
				}
			}
			if room <= 0 {
				break
			}
			part := math.Min(1, left/room)
			for _, t := range c[:gi] {
				if t.gave || t.price != level || t.want-kw[t.slot] <= 1e-12 {
					continue
				}
				x := part * (t.want - kw[t.slot])
				price[t.slot] = (price[t.slot]*kw[t.slot] + g.price*x) / (kw[t.slot] + x)
				kw[t.slot] += x
			}
			left -= part * room
			g.gave = true
		}
	}
	return kw, price
}

// After returns the site's plan with a zone's proposal added to it, for
// planning the next zone: surplus one zone takes and headroom it fills are
// not offered twice. Zones planned this way share the site in the order
// they are planned; they are not optimised together.
func After(site []SiteSlot, p Plan, slotH float64) []SiteSlot {
	out := append([]SiteSlot(nil), site...)
	for k := range out {
		if k < len(p.Slots) && slotH > 0 {
			out[k].GridKW += p.Slots[k].ElecKWh / slotH
		}
	}
	return out
}
