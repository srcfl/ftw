package heatplan

import (
	"errors"
	"math"
	"testing"

	"github.com/srcfl/ftw/go/internal/thermal"
)

func propose(t *testing.T, r Request) Plan {
	t.Helper()
	p, err := Propose(r)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(p.Slots) != len(r.Price) {
		t.Fatalf("plan has %d slots for a horizon of %d", len(p.Slots), len(r.Price))
	}
	for i, s := range p.Slots {
		if !finite(s.Value, s.TempC, s.HeatKWh, s.ElecKWh, s.Cost) || s.HeatKWh < 0 || s.ElecKWh < 0 {
			t.Fatalf("slot %d is not a finite, non-negative proposal: %+v", i, s)
		}
	}
	return p
}

// heat sums the heat bought in the hours [from, to) of every day.
func heat(p Plan, from, to float64) float64 {
	var sum float64
	for i, s := range p.Slots {
		if between(math.Mod(float64(i)*slotH, 24), from, to) {
			sum += s.HeatKWh
		}
	}
	return sum
}

func tempRange(p Plan, from, to float64) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for i, s := range p.Slots {
		if between(math.Mod(float64(i)*slotH, 24), from, to) {
			lo, hi = math.Min(lo, s.TempC), math.Max(hi, s.TempC)
		}
	}
	return lo, hi
}

func cheapNight(n int, night float64) []float64 {
	return hours(n, func(h float64) float64 {
		if between(h, 1, 5) {
			return night
		}
		return 100
	})
}

func TestFlatPriceLeavesTheDeviceAlone(t *testing.T) {
	for name, r := range map[string]Request{
		"radiators":      radiatorHouse(day, flat(day, 100)),
		"air to air":     airToAirHouse(day, flat(day, 100), flat(day, 0)),
		"slab":           slabHouse(day, flat(day, 100), flat(day, 0)),
		"bathroom floor": bathroomFloor(day, flat(day, 100), 3.5),
	} {
		p := propose(t, r)
		// The last slots may differ: the horizon's end is valued, not planned.
		for i, s := range p.Slots[:day-8] {
			want := r.Zone.TargetC[i]
			if r.Actuator.Strategy == thermal.InfluenceCurveOffset {
				want = 0
			}
			if s.Value != want {
				t.Fatalf("%s: slot %d proposes %v with nothing to gain; the device's own value is %v", name, i, s.Value, want)
			}
		}
		if p.BandExcursionKh != 0 {
			t.Fatalf("%s: left its band by %v Kh", name, p.BandExcursionKh)
		}
	}
}

// Direct electric heat in a heavy house: buy before the peak, buy nothing
// during it.
func TestRadiatorsCoastThroughAPeak(t *testing.T) {
	p := propose(t, radiatorHouse(day, eveningPeak(day, 100, 300)))
	if got := heat(p, 17, 20); got > 0.1 {
		t.Fatalf("bought %.1f kWh of heat at three times the price", got)
	}
	if lo, _ := tempRange(p, 0, 24); lo < 20 {
		t.Fatalf("coasted to %.2f °C, under the 20 °C band", lo)
	}
	if _, hi := tempRange(p, 14, 17); hi < 21.5 {
		t.Fatalf("preheated only to %.2f °C before the peak", hi)
	}
	if p.Cost > 0.85*p.BaselineCost {
		t.Fatalf("cost %.0f against %.0f left alone: the peak was not avoided", p.Cost, p.BaselineCost)
	}
	if p.BandExcursionKh != 0 {
		t.Fatalf("left the band by %v Kh", p.BandExcursionKh)
	}
}

// The same peak behind an air-to-air unit. The air it heats holds little, so
// the band runs out before the peak does and some heat is bought in it; a
// plan that pretended otherwise would leave the band.
func TestShortInertiaCannotCoastAWholePeak(t *testing.T) {
	price := eveningPeak(day, 100, 300)
	light := propose(t, airToAirHouse(day, price, flat(day, 0)))
	heavy := propose(t, radiatorHouse(day, price))
	if light.BandExcursionKh != 0 {
		t.Fatalf("left the band by %v Kh", light.BandExcursionKh)
	}
	if heat(light, 17, 20) < 1 {
		t.Fatalf("a zone with τ = 20 h and 3 kWh/K bought only %.2f kWh through a three-hour peak", heat(light, 17, 20))
	}
	if heat(light, 17, 20) <= heat(heavy, 17, 20) {
		t.Fatal("the light zone should need more heat in the peak than the heavy one")
	}
	// It still starts the peak as warm as the band allows.
	if _, hi := tempRange(light, 16, 17); hi < 21.8 {
		t.Fatalf("entered the peak at %.2f °C with a 22 °C ceiling", hi)
	}
}

// A heat pump on a slab preheats through its curve offset, and pays for it:
// a hotter flow is a worse COP. With a long inertia it still pays at twice
// the price.
func TestSlabPreheatsThroughTheOffset(t *testing.T) {
	r := slabHouse(day, eveningPeak(day, 100, 200), flat(day, 0))
	p := propose(t, r)
	if got := heat(p, 17, 20); got > 0.1 {
		t.Fatalf("bought %.1f kWh of heat in the peak", got)
	}
	raised := false
	for i, s := range p.Slots {
		if s.Value != math.Trunc(s.Value) || s.Value < -3 || s.Value > 3 {
			t.Fatalf("slot %d proposes offset %v, not a declared step", i, s.Value)
		}
		if h := float64(i) * slotH; between(h, 14, 17) && s.Value > 0 {
			raised = true
		}
	}
	if !raised {
		t.Fatal("never raised the offset ahead of the peak")
	}
	if p.Cost >= p.BaselineCost {
		t.Fatalf("cost %.0f, no better than %.0f left alone", p.Cost, p.BaselineCost)
	}
}

// Heat a bathroom floor loses inside a heated house is not lost: it saves the
// heat pump the same heat. The floor is then cheaper to run than its own
// meter says, by the pump's COP.
func TestLossIntoAHeatedHouseIsCredited(t *testing.T) {
	alone := propose(t, bathroomFloor(day, flat(day, 100), 0))
	inside := propose(t, bathroomFloor(day, flat(day, 100), 3.5))
	var elecAlone, elecInside float64
	for i := range alone.Slots {
		elecAlone += alone.Slots[i].ElecKWh
		elecInside += inside.Slots[i].ElecKWh
	}
	if math.Abs(elecAlone-elecInside) > 0.01 {
		t.Fatalf("the floor draws %.2f kWh alone and %.2f kWh inside the house; the credit must not change the meter", elecAlone, elecInside)
	}
	want := alone.Cost * (1 - 1/3.5)
	if math.Abs(inside.Cost-want) > 0.02*want {
		t.Fatalf("cost inside the house %.1f, want %.1f: the floor's cost less what the pump is spared", inside.Cost, want)
	}
}

// A tank on an immersion heater stores a small price difference as hot as
// the band allows: its heat costs the same at any temperature. A heat pump
// does not, because the last kelvins cost it more than the night saves.
func TestHotWaterStoresOnlyWhatItsSourceMakesWorthwhile(t *testing.T) {
	price := cheapNight(day, 88)
	direct := propose(t, hotWaterTank(day, price, Source{Kind: SourceDirectElectric}))
	pump := propose(t, hotWaterTank(day, price, Source{Kind: SourceHeatPump}))
	if _, hi := tempRange(direct, 1, 5); hi < 64 {
		t.Fatalf("the immersion heater charged only to %.1f °C on the cheap night", hi)
	}
	if _, hi := tempRange(pump, 1, 5); hi > 61 {
		t.Fatalf("the heat pump charged to %.1f °C for a 12 %% saving", hi)
	}
	for name, p := range map[string]Plan{"direct": direct, "pump": pump} {
		if lo, _ := tempRange(p, 0, 24); lo < 50-1e-6 {
			t.Fatalf("%s: tank at %.2f °C, under its 50 °C floor", name, lo)
		}
		if p.BandExcursionKh != 0 {
			t.Fatalf("%s: left the band by %v Kh", name, p.BandExcursionKh)
		}
	}
	// A large difference is worth the worse COP.
	deep := propose(t, hotWaterTank(day, cheapNight(day, 40), Source{Kind: SourceHeatPump}))
	if _, hi := tempRange(deep, 1, 5); hi < 64 {
		t.Fatalf("the heat pump charged only to %.1f °C at 40 %% of the price", hi)
	}
}

// A spa must be ready when it is used and has no comfort in between. Its
// heater takes hours to lift it, so the plan has to start early, and it
// starts where the heat is cheapest.
func TestSpaIsReadyForUseOnCheapHeat(t *testing.T) {
	price := hours(day, func(h float64) float64 {
		switch {
		case between(h, 1, 5):
			return 40
		case between(h, 17, 20):
			return 300
		}
		return 100
	})
	p := propose(t, spa(day, price))
	if lo, _ := tempRange(p, 19, 22); lo < 37.5-1e-6 {
		t.Fatalf("spa at %.2f °C while in use, under 37.5 °C", lo)
	}
	if p.BandExcursionKh != 0 {
		t.Fatalf("left the band by %v Kh", p.BandExcursionKh)
	}
	if got := heat(p, 17, 20); got > 0.1 {
		t.Fatalf("bought %.1f kWh in the peak", got)
	}
	total := heat(p, 0, 24)
	if night := heat(p, 1, 5); night < 0.8*total {
		t.Fatalf("%.1f of %.1f kWh bought in the cheap night", night, total)
	}
}

// At one price all day, a pool's air-source pump still has a cheapest hour:
// the warmest one.
func TestPoolPumpRunsInTheWarmHours(t *testing.T) {
	r := pool(day, flat(day, 100), summerDay(day))
	p := propose(t, r)
	total := heat(p, 0, 24)
	if total < 10 {
		t.Fatalf("pool bought %.1f kWh in a day; the plan only deferred", total)
	}
	if warm := heat(p, 11, 19); warm < 0.95*total {
		t.Fatalf("%.1f of %.1f kWh bought between 11 and 19", warm, total)
	}
	if p.Cost >= p.BaselineCost {
		t.Fatalf("cost %.0f, no better than %.0f left alone", p.Cost, p.BaselineCost)
	}
	checkRuns(t, p, r.Actuator)
}

func checkRuns(t *testing.T, p Plan, a Actuator) {
	t.Helper()
	run, on := max(1, a.RunSlots), a.On
	for i, s := range p.Slots {
		now := s.Value == 1
		if s.Value != 0 && s.Value != 1 {
			t.Fatalf("slot %d proposes %v for a start/stop control", i, s.Value)
		}
		if now == on {
			run++
			continue
		}
		if need := map[bool]int{true: a.MinOnSlots, false: a.MinOffSlots}[on]; run < need {
			t.Fatalf("slot %d switches after %d slots; the device needs %d", i, run, need)
		}
		on, run = now, 1
	}
}

func TestMinimumOnAndOffTimesAreKept(t *testing.T) {
	// A jagged price that would have a free device switching every slot.
	price := make([]float64, day)
	for i := range price {
		price[i] = 100 + 80*float64(i%2)
	}
	for _, c := range []struct {
		on  bool
		run int
	}{{false, 1}, {true, 1}, {true, 6}, {false, 6}} {
		r := pool(day, price, summerDay(day))
		r.Zone.StartC = 26.2
		r.Actuator.MinOnSlots, r.Actuator.MinOffSlots = 4, 6
		r.Actuator.On, r.Actuator.RunSlots = c.on, c.run
		checkRuns(t, propose(t, r), r.Actuator)
	}
}

func TestAnUndersizedHeaterIsReportedNotHidden(t *testing.T) {
	r := radiatorHouse(day, flat(day, 100))
	r.Actuator.MaxHeatKW = 2 // the house loses 4.2 kW at target
	p := propose(t, r)
	if p.BandExcursionKh <= 0 {
		t.Fatal("a house that cannot be held reported no excursion")
	}
	for i, s := range p.Slots {
		if s.HeatKWh < 2*slotH-1e-9 {
			t.Fatalf("slot %d holds back heat while the zone falls out of its band", i)
		}
	}
}

func TestNegativePriceFillsTheBand(t *testing.T) {
	price := hours(day, func(h float64) float64 {
		if between(h, 12, 14) {
			return -20
		}
		return 100
	})
	p := propose(t, radiatorHouse(day, price))
	// It may arrive cooler to take more; what it must not do is hold back.
	if got := heat(p, 12, 14); got < 2*8-0.01 {
		t.Fatalf("took %.1f kWh in two hours of being paid to heat; the radiators give 16", got)
	}
	if p.BandExcursionKh != 0 {
		t.Fatalf("left the band by %v Kh", p.BandExcursionKh)
	}
}

func TestProposeRefusesWhatItCannotAnswer(t *testing.T) {
	bad := map[string]func(*Request){
		"no horizon":          func(r *Request) { r.Price = nil },
		"short series":        func(r *Request) { r.OutdoorC = r.OutdoorC[:10] },
		"nan price":           func(r *Request) { r.Price[3] = math.NaN() },
		"target outside band": func(r *Request) { r.Zone.TargetC[5] = 25 },
		"no inertia":          func(r *Request) { r.Zone.TauH = 0 },
		"negative capacity":   func(r *Request) { r.Zone.CapacityKWhPerK = -1 },
		"no heat":             func(r *Request) { r.Actuator.MaxHeatKW = 0 },
		"falling values":      func(r *Request) { r.Actuator.Values = []float64{21, 20} },
		"too many values":     func(r *Request) { r.Actuator.Values = Values(0, 40, 1, 0, 100) },
		"credit below one":    func(r *Request) { r.Zone.LossCreditCOP = flat(day, 0.5) },
		"unknown source":      func(r *Request) { r.Actuator.Source.Kind = "wood" },
		"hour-long slots":     func(r *Request) { r.SlotH = 2 },
	}
	for name, mutate := range bad {
		r := radiatorHouse(day, flat(day, 100))
		mutate(&r)
		if _, err := Propose(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	r := radiatorHouse(day, flat(day, 100))
	r.Actuator.Strategy = thermal.InfluenceDiscreteMode
	if _, err := Propose(r); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("discrete mode: got %v, want ErrUnsupported", err)
	}
	offset := slabHouse(day, flat(day, 100), flat(day, 0))
	offset.Actuator.ZoneKPerUnit = 0
	if _, err := Propose(offset); err == nil {
		t.Fatal("a curve offset with no learned effect on the zone was planned")
	}
}

func TestValues(t *testing.T) {
	eq := func(got, want []float64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if math.Abs(got[i]-want[i]) > 1e-9 {
				return false
			}
		}
		return true
	}
	if got := Values(-3, 3, 1, 0, 16); !eq(got, []float64{-3, -2, -1, 0, 1, 2, 3}) {
		t.Fatalf("whole range: %v", got)
	}
	// Thinned, with both ends and the device's own value kept.
	got := Values(5, 30, 0.5, 21.5, 8)
	if len(got) > 8 || got[0] != 5 || got[len(got)-1] != 30 {
		t.Fatalf("thinned range lost an end or its limit: %v", got)
	}
	kept := false
	for i, v := range got {
		kept = kept || v == 21.5
		if i > 0 && v <= got[i-1] {
			t.Fatalf("not rising: %v", got)
		}
	}
	if !kept {
		t.Fatalf("thinned range dropped the device's own value: %v", got)
	}
	for _, bad := range [][]float64{Values(1, 0, 1, 0, 8), Values(0, 1, 0, 0, 8), Values(0, 1, 1, 0, 2)} {
		if bad != nil {
			t.Fatalf("a range that is none returned %v", bad)
		}
	}
}

// A thermostat cannot be told to draw less; it can only be told when. With
// the cheap hours taken by something else, the tank charges in the cheapest
// hours that have room for it.
func TestImportLimitMovesTheDraw(t *testing.T) {
	price := hours(day, func(h float64) float64 {
		switch {
		case between(h, 2, 4):
			return 40
		case between(h, 4, 6):
			return 60
		}
		return 100
	})
	free := hotWaterTank(day, price, Source{Kind: SourceDirectElectric})
	limited := hotWaterTank(day, price, Source{Kind: SourceDirectElectric})
	limited.MaxElecKW = hours(day, func(h float64) float64 {
		if between(h, 2, 4) {
			return 1 // the car is charging
		}
		return 5
	})
	pf, pl := propose(t, free), propose(t, limited)
	if heat(pf, 2, 4) < 3 {
		t.Fatalf("the free tank took %.1f kWh in the cheapest hours; the test needs it to want them", heat(pf, 2, 4))
	}
	if pl.OverLimitKWh != 0 {
		t.Fatalf("%.2f kWh over the limit with room elsewhere", pl.OverLimitKWh)
	}
	for k, s := range pl.Slots {
		if kw := s.ElecKWh / slotH; kw > limited.MaxElecKW[k]+1e-6 {
			t.Fatalf("slot %d draws %.2f kW against a limit of %.0f kW", k, kw, limited.MaxElecKW[k])
		}
	}
	if heat(pl, 4, 6) < 3 {
		t.Fatalf("took %.1f kWh in the next cheapest hours", heat(pl, 4, 6))
	}
	// To the planner's own resolution: its temperature grid is 0.1 K here.
	if lo, _ := tempRange(pl, 0, 24); lo < 49.95 || pl.BandExcursionKh > 0.01 {
		t.Fatalf("the limit cost the tank its floor: %.2f °C", lo)
	}
	if pf.OverLimitKWh != 0 {
		t.Fatal("a plan without a limit reported being over one")
	}
}

// Heat is planned inside the site's energy plan, never through it. With the
// site's headroom taken for eight hours, the house coasts out of its band;
// the plan says so and does not draw the difference.
func TestImportLimitOutranksTheBand(t *testing.T) {
	busy := func(h float64) bool { return between(h, 14, 22) }
	r := radiatorHouse(day, flat(day, 100))
	r.Actuator.Values = Values(15, 22, 0.5, 21, 16)
	r.MaxElecKW = hours(day, func(h float64) float64 {
		if busy(h) {
			return 1 // the house needs 4.2 kW to hold its target
		}
		return 10
	})
	p := propose(t, r)
	for k, s := range p.Slots {
		if kw := s.ElecKWh / slotH; kw > r.MaxElecKW[k]+1e-6 {
			t.Fatalf("slot %d draws %.2f kW through a %.0f kW limit", k, kw, r.MaxElecKW[k])
		}
	}
	if p.OverLimitKWh != 0 {
		t.Fatalf("reports %.2f kWh over the limit", p.OverLimitKWh)
	}
	if lo, _ := tempRange(p, 14, 22); lo >= 20 || p.BandExcursionKh <= 0 {
		t.Fatalf("lowest %.2f °C, excursion %v Kh: eight hours on 1 kW must take the house under 20 °C, and be reported", lo, p.BandExcursionKh)
	}
	// It did what it could inside the plan: full before, and back after.
	if _, hi := tempRange(p, 13, 14); hi < 21.9 {
		t.Fatalf("entered the limited hours at %.2f °C with a 22 °C ceiling", hi)
	}
	if got := heat(p, 22, 24); got < 2*8-0.1 {
		t.Fatalf("took %.1f kWh in the two hours after the limit lifted; the radiators give 16", got)
	}
}

// A thermostat regulates by itself at the lowest value it can be given.
// What that draws through the limit cannot be planned away, and is reported.
func TestWhatTheDeviceDrawsByItselfIsReported(t *testing.T) {
	r := radiatorHouse(day, flat(day, 100)) // lowest value 20 °C: 4 kW
	r.MaxElecKW = flat(day, 3)
	p := propose(t, r)
	if p.OverLimitKWh <= 0 {
		t.Fatal("the thermostat's own draw through the limit was not reported")
	}
	for k, s := range p.Slots {
		if s.Value > 21 {
			t.Fatalf("slot %d raises the setpoint to %v under a limit the house already passes", k, s.Value)
		}
	}
}

// Solar the site would have exported is the cheapest heat there is, as far
// as it goes and no further.
func TestSurplusIsUsedBeforeImport(t *testing.T) {
	site := make([]SiteSlot, day)
	for k := range site {
		site[k] = SiteSlot{ImportPrice: 100, ExportPrice: 30, GridKW: 0.5}
		if between(float64(k)*slotH, 11, 14) {
			site[k].GridKW = -2 // exporting 2 kW
		}
	}
	r := hotWaterTank(day, nil, Source{Kind: SourceDirectElectric})
	r.Site(site, 0)
	p := propose(t, r)
	if got := heat(p, 11, 14); got < 3 {
		t.Fatalf("took %.1f kWh while the site exported", got)
	}
	// The same tank with no surplus has no reason to charge at midday.
	plain := hotWaterTank(day, flat(day, 100), Source{Kind: SourceDirectElectric})
	if got := heat(propose(t, plain), 11, 14); got > 0.5 {
		t.Fatalf("took %.1f kWh at midday with nothing to gain", got)
	}
	// Surplus is priced only as far as it goes: 3 kWh in an hour is 2 at the
	// export price and 1 at the import price.
	one := Request{SlotH: 1, OutdoorC: []float64{0},
		Zone: Zone{TauH: 200, CapacityKWhPerK: 0.35, StartC: 50, BoundaryC: []float64{20},
			TargetC: []float64{60}, MinC: []float64{50}, MaxC: []float64{65}},
		Actuator: Actuator{Strategy: thermal.InfluenceSetpoint, Values: []float64{65}, MaxHeatKW: 3,
			Source: Source{Kind: SourceDirectElectric}}}
	one.Site([]SiteSlot{{ImportPrice: 100, ExportPrice: 30, GridKW: -2}}, 0)
	if got := propose(t, one).Slots[0]; math.Abs(got.ElecKWh-3) > 1e-9 || math.Abs(got.Cost-(2*30+1*100)) > 1e-6 {
		t.Fatalf("3 kWh against a 2 kW surplus cost %.1f for %.2f kWh, want 160 for 3", got.Cost, got.ElecKWh)
	}
	if p.Cost >= p.BaselineCost {
		t.Fatalf("cost %.0f, no better than %.0f left alone", p.Cost, p.BaselineCost)
	}
}

func TestSiteSetsHeadroomAndSurplus(t *testing.T) {
	var r Request
	r.Site([]SiteSlot{
		{ImportPrice: 100, ExportPrice: 30, GridKW: 3},
		{ImportPrice: 100, ExportPrice: 30, GridKW: -1.5},
		{ImportPrice: 100, ExportPrice: 30, GridKW: 7},
		{ImportPrice: 20, ExportPrice: 30, GridKW: -1},
	}, 5)
	want := []struct{ max, surplus, surplusPrice float64 }{{2, 0, 100}, {6.5, 1.5, 30}, {0, 0, 100}, {6, 1, 20}}
	for k, w := range want {
		if r.MaxElecKW[k] != w.max || r.SurplusKW[k] != w.surplus || r.SurplusPrice[k] != w.surplusPrice {
			t.Errorf("slot %d: limit %v surplus %v at %v, want %+v", k, r.MaxElecKW[k], r.SurplusKW[k], r.SurplusPrice[k], w)
		}
	}
	r.Site([]SiteSlot{{ImportPrice: 100}}, 0)
	if r.MaxElecKW != nil {
		t.Fatal("no import limit must mean no limit, not a limit of zero")
	}
}

// Two zones planned one after the other must not both claim the same
// surplus or the same headroom.
func TestAfterHandsTheNextZoneWhatIsLeft(t *testing.T) {
	site := make([]SiteSlot, day)
	for k := range site {
		site[k] = SiteSlot{ImportPrice: 100, ExportPrice: 30, GridKW: 1}
		if between(float64(k)*slotH, 2, 4) {
			site[k].ImportPrice = 40
		}
	}
	const limit = 5.0
	tank := hotWaterTank(day, nil, Source{Kind: SourceDirectElectric})
	tank.Site(site, limit)
	first := propose(t, tank)
	second := spa(day, nil)
	second.Site(After(site, first, slotH), limit)
	next := propose(t, second)
	for k := range site {
		total := site[k].GridKW + (first.Slots[k].ElecKWh+next.Slots[k].ElecKWh)/slotH
		if total > limit+1e-6 {
			t.Fatalf("slot %d: the site imports %.2f kW with both zones, over its %.0f kW limit", k, total, limit)
		}
	}
	if first.OverLimitKWh != 0 || next.OverLimitKWh != 0 {
		t.Fatal("a zone reported passing the limit")
	}
}

// batterySite is a day with an evening peak at 300 that the battery covers.
// With ample set it also covers the shoulder after it, at 150.
func batterySite(ample bool) []SiteSlot {
	site := make([]SiteSlot, day)
	for k := range site {
		h := float64(k) * slotH
		site[k] = SiteSlot{ImportPrice: 100, ExportPrice: 30, GridKW: 1}
		switch {
		case between(h, 17, 20):
			site[k] = SiteSlot{ImportPrice: 300, ExportPrice: 30, GridKW: 0, BatteryDischargeKW: 1, BatterySpareKW: 2}
		case between(h, 20, 23):
			site[k].ImportPrice = 150
			if ample {
				site[k] = SiteSlot{ImportPrice: 150, ExportPrice: 30, GridKW: 0, BatteryDischargeKW: 1, BatterySpareKW: 2}
			}
		}
	}
	return site
}

// A battery that holds energy does not make the peak cheap. Heat it carries
// is bought back in the cheapest slot the battery was to cover, and only as
// much as half of what it was to spend there.
func TestBatteryEnergyCostsWhatTheSiteBuysBack(t *testing.T) {
	var ample, scarce Request
	ample.Site(batterySite(true), 5)
	scarce.Site(batterySite(false), 5)
	for k := range ample.Price {
		h := float64(k) * slotH
		if !between(h, 17, 20) {
			if ample.SurplusKW[k] != 0 || scarce.SurplusKW[k] != 0 {
				t.Fatalf("slot %d: battery energy offered outside the slots dearer than the one given up", k)
			}
			continue
		}
		// 3 kWh planned for the shoulder, half of it on offer, against
		// 6 kWh of spare power in the peak: a quarter of each slot's 2 kW.
		if math.Abs(ample.SurplusKW[k]-0.5) > 1e-9 || math.Abs(ample.SurplusPrice[k]-150) > 1e-9 {
			t.Fatalf("slot %d: %.2f kW at %.0f, want 0.5 kW at the shoulder's 150", k, ample.SurplusKW[k], ample.SurplusPrice[k])
		}
		if math.Abs(ample.MaxElecKW[k]-5.5) > 1e-9 {
			t.Fatalf("slot %d: headroom %.2f kW, want the 5 kW limit plus the 0.5 kW the battery carries", k, ample.MaxElecKW[k])
		}
		// A battery planned only into the peak has nothing to give up:
		// every kWh heat took would be bought back at the peak's price.
		if scarce.SurplusKW[k] != 0 || scarce.SurplusPrice[k] != 300 || scarce.MaxElecKW[k] != 5 {
			t.Fatalf("slot %d: a battery planned only into the peak offered %.2f kW at %.0f", k, scarce.SurplusKW[k], scarce.SurplusPrice[k])
		}
	}
	// The battery cannot give back what it has already spent: a shoulder
	// before the peak frees nothing for it.
	early := batterySite(false)
	for k := range early {
		if between(float64(k)*slotH, 14, 17) {
			early[k] = SiteSlot{ImportPrice: 150, ExportPrice: 30, GridKW: 0, BatteryDischargeKW: 1, BatterySpareKW: 2}
		}
	}
	var before Request
	before.Site(early, 5)
	for k := range before.Price {
		if before.SurplusKW[k] != 0 {
			t.Fatalf("slot %d: offered %.2f kW from a shoulder that comes before the peak", k, before.SurplusKW[k])
		}
	}
	// A small zone is offered what it can take, not what the battery has.
	small := Request{Actuator: Actuator{MaxHeatKW: 0.2}}
	small.Site(batterySite(true), 0)
	for k := range small.Price {
		if between(float64(k)*slotH, 17, 20) && math.Abs(small.SurplusKW[k]-0.2) > 1e-9 {
			t.Fatalf("slot %d: a 0.2 kW floor is offered %.2f kW", k, small.SurplusKW[k])
		}
	}
}

// The heat a light room must buy in the peak costs the shoulder's price for
// what the battery carries, and the import price for the rest.
func TestBatteryMakesAPeakCheaperToHeatThrough(t *testing.T) {
	plan := func(ample bool) Plan {
		r := airToAirHouse(day, nil, flat(day, 0))
		r.Site(batterySite(ample), 0)
		return propose(t, r)
	}
	with, without := plan(true), plan(false)
	peakCost := func(p Plan) (cost, elec float64) {
		for k, s := range p.Slots {
			if between(float64(k)*slotH, 17, 20) {
				cost, elec = cost+s.Cost, elec+s.ElecKWh
			}
		}
		return cost, elec
	}
	costWith, elecWith := peakCost(with)
	costWithout, elecWithout := peakCost(without)
	if elecWith < 0.2 || elecWithout < 0.2 {
		t.Fatalf("the room bought %.2f and %.2f kWh in the peak; the test needs it to buy some", elecWith, elecWithout)
	}
	// 0.5 kW of the peak draw at the shoulder's 150, the rest at 300.
	if got := costWith / elecWith; got < 150 || got > 290 {
		t.Fatalf("peak electricity cost %.1f per kWh with the battery, want between the shoulder's 150 and the peak's 300", got)
	}
	if got := costWithout / elecWithout; math.Abs(got-300) > 1 {
		t.Fatalf("peak electricity cost %.1f per kWh without, want the import price 300", got)
	}
	if with.Cost >= without.Cost {
		t.Fatalf("cost %.0f with the battery, %.0f without", with.Cost, without.Cost)
	}
	for name, p := range map[string]Plan{"with": with, "without": without} {
		if p.BandExcursionKh != 0 {
			t.Fatalf("%s: left the band by %v Kh", name, p.BandExcursionKh)
		}
	}
}
