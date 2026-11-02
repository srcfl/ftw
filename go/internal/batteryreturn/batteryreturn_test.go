package batteryreturn

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

var stockholm = func() *time.Location {
	loc, err := time.LoadLocation(Zone)
	if err != nil {
		panic(err)
	}
	return loc
}()

// The test year starts at local midnight; the report runs two days in.
var (
	yearStart = time.Date(2025, 1, 1, 0, 0, 0, 0, stockholm)
	twoDays   = yearStart.Add(48 * time.Hour)
	reportNow = twoDays.Add(settle)
)

const (
	grid    = "meter:1/grid_meter"
	battery = "inverter:1/battery"
	solar   = "inverter:1/pv"
	charger = "charger:1/vehicle_charger"
)

// oneBattery is 10 kWh, configured, at an unplaced site.
var oneBattery = Site{CapacityWh: 10_000, Batteries: 1}

// flows is one hour of a site, in kWh.
type flows struct{ imp, exp, chg, dis, pv float64 }

// fakeLedger holds hourly points as the store returns them for 1 h buckets.
type fakeLedger struct {
	assets []state.EnergyAsset
	points map[string][]state.EnergyLedgerPoint
}

func (f *fakeLedger) EnergyAssets() ([]state.EnergyAsset, error) { return f.assets, nil }

func (f *fakeLedger) LoadEnergyHistoryContext(_ context.Context, q state.EnergyHistoryQuery) ([]state.EnergyLedgerPoint, bool, error) {
	var out []state.EnergyLedgerPoint
	for _, p := range f.points[q.AssetID] {
		if p.BucketStartMS < q.UntilMS && p.BucketStartMS+p.BucketLenMS > q.SinceMS {
			out = append(out, p)
		}
	}
	return out, false, nil
}

func (f *fakeLedger) add(asset string, flow state.EnergyFlow, at time.Time, length time.Duration, wh float64, quality string) {
	f.points[asset] = append(f.points[asset], state.EnergyLedgerPoint{
		AssetID: asset, Flow: flow, BucketStartMS: at.UnixMilli(), BucketLenMS: length.Milliseconds(),
		EnergyWh: wh, Quality: quality,
	})
}

func (f *fakeLedger) install(id string, kind state.EnergyAssetKind) {
	f.assets = append(f.assets, state.EnergyAsset{AssetID: id, Kind: kind,
		FirstSeenMS: yearStart.Add(-24 * time.Hour).UnixMilli(), LastSeenMS: reportNow.UnixMilli()})
}

// drop removes an asset's points for the hours from..to-1.
func (f *fakeLedger) drop(asset string, from, to int) {
	var kept []state.EnergyLedgerPoint
	for _, p := range f.points[asset] {
		i := int((p.BucketStartMS - yearStart.UnixMilli()) / hourMS)
		if i < from || i >= to {
			kept = append(kept, p)
		}
	}
	f.points[asset] = kept
}

// mark sets the quality of an asset's points in hour i.
func (f *fakeLedger) mark(asset string, i int, quality string) {
	for j, p := range f.points[asset] {
		if p.BucketStartMS == yearStart.Add(time.Duration(i)*time.Hour).UnixMilli() {
			f.points[asset][j].Quality = quality
		}
	}
}

// site builds a ledger in which every meter reads every hour of the two
// days and the hour after them; an hour missing from the map is quiet.
func site(hours map[int]flows) *fakeLedger {
	f := &fakeLedger{points: map[string][]state.EnergyLedgerPoint{}}
	f.install(grid, state.AssetGridMeter)
	f.install(battery, state.AssetBattery)
	f.install(solar, state.AssetPV)
	for i := range 49 {
		at, h := yearStart.Add(time.Duration(i)*time.Hour), hours[i]
		f.add(grid, state.FlowGridImport, at, time.Hour, h.imp*1000, "measured")
		f.add(grid, state.FlowGridExport, at, time.Hour, h.exp*1000, "measured")
		f.add(battery, state.FlowBatteryCharge, at, time.Hour, h.chg*1000, "integrated")
		f.add(battery, state.FlowBatteryDischarge, at, time.Hour, h.dis*1000, "integrated")
		f.add(solar, state.FlowPVGeneration, at, time.Hour, h.pv*1000, "integrated")
	}
	return f
}

// arbitrage charges 10 kWh from the grid at night and sends 9 kWh to the
// grid in the evening, on each day listed.
func arbitrage(days ...int) map[int]flows {
	hours := map[int]flows{}
	for _, d := range days {
		for h := 1; h <= 4; h++ {
			hours[d*24+h] = flows{imp: 2.5, chg: 2.5}
		}
		for h := 18; h <= 20; h++ {
			hours[d*24+h] = flows{exp: 3, dis: 3}
		}
	}
	return hours
}

// Two days of arbitrage charge 20 kWh and discharge 18. The least
// efficiency that allows, with a 10 kWh battery that may have emptied over
// the two days, is (18 − 10) / 20 = 0.4: each night hour adds 1 kWh of grid
// energy to the mix.
//
// firstShare is the grid share after the first night: four hours, each
// leaving 10/11 of what the full battery held.
var firstShare = 1 - math.Pow(10.0/11, 4)

func build(t *testing.T, l Ledger, s Site) Report {
	t.Helper()
	r, err := Build(context.Background(), l, 2025, reportNow, s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1 {
		t.Fatalf("%s = %.1f Wh, want %.1f", what, got, want)
	}
}

func returnedOn(r Report, day int) float64 {
	var wh float64
	for _, h := range r.Hours[day*24 : day*24+24] {
		wh += h.Returned
	}
	return wh
}

func TestArbitrageReturnsGridEnergyAfterLoss(t *testing.T) {
	r := build(t, site(arbitrage(0, 1)), oneBattery)
	if math.Abs(r.Efficiency-0.4) > 1e-9 || r.CapacityWh != 10_000 || r.CompleteHours() != 48 || r.Problem != "" || r.Batteries != 1 {
		t.Fatalf("efficiency %v capacity %v complete %d problem %q", r.Efficiency, r.CapacityWh, r.CompleteHours(), r.Problem)
	}
	// The year starts with a battery full of energy from elsewhere. Each
	// night hour's grid energy pushes the same amount of the mix out, so
	// the first evening returns 9 kWh at the grid share firstShare.
	near(t, "first day", returnedOn(r, 0), 9000*firstShare)
	// The second night adds 4 kWh of grid energy to the 1 kWh left, and the
	// evening empties what the mix holds.
	near(t, "second day", returnedOn(r, 1), 1000*(4+firstShare))
	near(t, "year", r.Returned, returnedOn(r, 0)+returnedOn(r, 1))
	near(t, "january", r.Months[0].Returned, r.Returned)
	if r.Months[0].Hours != 48 || r.Months[1].Hours != 0 {
		t.Fatalf("months %+v", r.Months[:2])
	}
	if h := r.Hours[2]; h.GridToBattery != 2500 || h.BatteryToGrid != 0 {
		t.Fatalf("charging hour %+v", h)
	}
	if h := r.Hours[19]; h.GridToBattery != 0 || h.BatteryToGrid != 3000 {
		t.Fatalf("export hour %+v", h)
	}
	near(t, "held after the second night", r.Hours[24+4].GridHeld+r.Hours[24+4].OtherHeld, 5000)
}

func TestStoredSolarSentToTheGridIsNotReturned(t *testing.T) {
	hours := map[int]flows{}
	for d := range 2 {
		for h := 10; h <= 13; h++ {
			hours[d*24+h] = flows{pv: 4, chg: 2.5, exp: 1.5}
		}
		for h := 18; h <= 20; h++ {
			hours[d*24+h] = flows{exp: 3, dis: 3}
		}
	}
	if r := build(t, site(hours), oneBattery); r.Returned != 0 {
		t.Fatalf("returned %.1f Wh of solar", r.Returned)
	}
}

func TestGridEnergyUsedAtHomeIsNotReturned(t *testing.T) {
	hours := map[int]flows{}
	for d := range 2 {
		for h := 1; h <= 4; h++ {
			hours[d*24+h] = flows{imp: 2.5, chg: 2.5}
		}
		for h := 7; h <= 9; h++ {
			hours[d*24+h] = flows{dis: 3}
		}
		for h := 11; h <= 15; h++ {
			hours[d*24+h] = flows{pv: 1, exp: 1}
		}
	}
	if r := build(t, site(hours), oneBattery); r.Returned != 0 {
		t.Fatalf("returned %.1f Wh used at home", r.Returned)
	}
}

func TestOnlyTheShareThatLeftTheSiteIsReturned(t *testing.T) {
	hours := arbitrage(0, 1)
	for h := 18; h <= 20; h++ {
		hours[24+h] = flows{exp: 1, dis: 3} // 2 kWh of each hour stays at home
	}
	r := build(t, site(hours), oneBattery)
	if h := r.Hours[24+19]; h.BatteryToGrid != 1000 {
		t.Fatalf("export hour %+v", h)
	}
	// Each hour sends a third of its discharge to the grid.
	near(t, "second day", returnedOn(r, 1), 1000*(4+firstShare)/3)
}

func TestSolarInTheHourCountsFirst(t *testing.T) {
	// Charging 3 kWh while the sun gives 2 takes at least 1 from the grid;
	// sending 3 out while the sun gives 2 takes at least 1 from the battery.
	r := build(t, site(map[int]flows{
		10: {pv: 2, chg: 3, imp: 1.5},
		18: {pv: 2, dis: 3, exp: 3},
	}), oneBattery)
	if r.Hours[10].GridToBattery != 1000 || r.Hours[18].BatteryToGrid != 1000 {
		t.Fatalf("charge %+v export %+v", r.Hours[10], r.Hours[18])
	}
}

func TestAMissingHourForgetsWhatTheBatteryHeld(t *testing.T) {
	l := site(arbitrage(0, 1))
	// The battery's reading for noon on the second day has a gap.
	l.mark(battery, 36, "gap")
	r := build(t, l, oneBattery)
	// The hour before it may have ended early, so it does not count either.
	if r.Hours[35].Complete || r.Hours[36].Complete || r.CompleteHours() != 46 {
		t.Fatalf("hours 35/36 complete %v/%v, %d complete hours", r.Hours[35].Complete, r.Hours[36].Complete, r.CompleteHours())
	}
	if got := returnedOn(r, 1); got != 0 {
		t.Fatalf("second day returned %.1f Wh after the gap", got)
	}
	near(t, "first day", returnedOn(r, 0), 9000*firstShare)
}

func TestOnlyMeasuredReadingsCount(t *testing.T) {
	// A recovered counter delta is spread evenly over an outage, so it can
	// move midday export into an evening hour.
	for _, quality := range []string{"recovered", "gap", "estimated", "invalid", "reset"} {
		// Without the grid meter the hour attributes nothing, but the
		// battery's readings keep the mix.
		l := site(arbitrage(0, 1))
		l.mark(grid, 19, quality)
		r := build(t, l, oneBattery)
		if r.Hours[19].Complete || r.Hours[18].Complete || r.Hours[18].Returned != 0 || r.Hours[19].Returned != 0 || r.Hours[20].Returned <= 0 {
			t.Fatalf("%s grid reading counted: %+v", quality, r.Hours[18:21])
		}
		// Without the battery's, the mix starts over.
		l = site(arbitrage(0, 1))
		l.mark(battery, 19, quality)
		if r := build(t, l, oneBattery); r.Hours[19].Complete || returnedOn(r, 0) != 0 {
			t.Fatalf("%s battery reading counted", quality)
		}
	}
}

func TestAMeterThatFellSilentLeavesItsHoursIncomplete(t *testing.T) {
	// The solar driver stopped after the first day. Its last reading is then.
	l := site(arbitrage(0, 1))
	l.drop(solar, 24, 49)
	l.assets[2].LastSeenMS = yearStart.Add(24 * time.Hour).UnixMilli()
	if r := build(t, l, oneBattery); r.CompleteHours() != 23 || r.Hours[30].Complete || returnedOn(r, 1) != 0 {
		t.Fatalf("%d complete hours without solar", r.CompleteHours())
	}
	// Solar added in the middle of the year cannot vouch for the hours
	// before it.
	l = site(arbitrage(0, 1))
	l.drop(solar, 0, 24)
	l.assets[2].FirstSeenMS = yearStart.Add(24 * time.Hour).UnixMilli()
	if r := build(t, l, oneBattery); r.CompleteHours() != 24 || r.Hours[10].Complete || !r.Hours[30].Complete {
		t.Fatalf("%d complete hours before solar", r.CompleteHours())
	}
}

func TestDailyTotalsCannotBeSplitIntoHours(t *testing.T) {
	l := site(arbitrage(0, 1))
	l.drop(battery, 0, 24)
	l.add(battery, state.FlowBatteryCharge, yearStart, 24*time.Hour, 10_000, "measured")
	r := build(t, l, oneBattery)
	if r.CompleteHours() != 24 || r.Hours[0].Complete || r.Hours[23].Complete || !r.Hours[24].Complete {
		t.Fatalf("%d complete hours", r.CompleteHours())
	}
}

func TestNoGridMeterCountsNothing(t *testing.T) {
	l := site(arbitrage(0, 1))
	l.assets = l.assets[1:]
	if r := build(t, l, oneBattery); r.CompleteHours() != 0 || r.Returned != 0 {
		t.Fatalf("%d complete hours, %.1f Wh", r.CompleteHours(), r.Returned)
	}
}

func TestOneSizedBatteryOrNoFigure(t *testing.T) {
	if r := build(t, site(arbitrage(0, 1)), Site{}); r.Problem != NoSize || r.Returned != 0 || r.CompleteHours() != 0 {
		t.Fatalf("no size: problem %q, %.1f Wh", r.Problem, r.Returned)
	}
	// A second battery: one mix would let the first battery's exports carry
	// grid energy the second one holds.
	l := site(arbitrage(0, 1))
	l.install("inverter:2/battery", state.AssetBattery)
	for i := range 49 {
		l.add("inverter:2/battery", state.FlowBatteryCharge, yearStart.Add(time.Duration(i)*time.Hour), time.Hour, 0, "integrated")
	}
	for _, s := range []Site{oneBattery, {CapacityWh: 20_000, Batteries: 2}} {
		if r := build(t, l, s); r.Problem != NotOneBattery || r.Returned != 0 || r.Batteries != 2 {
			t.Fatalf("two batteries, %d sized: problem %q, %.1f Wh", s.Batteries, r.Problem, r.Returned)
		}
	}
	// A size for a battery the ledger never read.
	if r := build(t, site(arbitrage(0, 1)), Site{CapacityWh: 20_000, Batteries: 2}); r.Problem != NotOneBattery {
		t.Fatalf("one battery, two sizes: problem %q", r.Problem)
	}
	// No battery at all is no problem, just nothing.
	l = site(nil)
	l.assets = append(l.assets[:1], l.assets[2:]...)
	if r := build(t, l, Site{}); r.Problem != "" || r.Returned != 0 || r.CompleteHours() != 48 {
		t.Fatalf("no battery: problem %q, %d complete hours", r.Problem, r.CompleteHours())
	}
}

func TestTheSizeIsAtLeastWhatTheBatteryGainedInADay(t *testing.T) {
	// Configured as 5 kWh, the battery gains 9 kWh a night after loss.
	r := build(t, site(arbitrage(0, 1)), Site{CapacityWh: 5000, Batteries: 1})
	if r.CapacityWh != 9000 {
		t.Fatalf("capacity %v", r.CapacityWh)
	}
	// A larger size only counts lower.
	small, large := build(t, site(arbitrage(0, 1)), oneBattery), build(t, site(arbitrage(0, 1)), Site{CapacityWh: 15_000, Batteries: 1})
	if large.Returned >= small.Returned {
		t.Fatalf("15 kWh returned %.1f, 10 kWh %.1f", large.Returned, small.Returned)
	}
}

func TestACarGivingEnergyBackMustBeRead(t *testing.T) {
	// A charger that never gives energy back is not needed.
	l := site(arbitrage(0, 1))
	l.install(charger, state.AssetVehicleCharger)
	l.add(charger, state.FlowVehicleCharge, yearStart.Add(3*time.Hour), time.Hour, 7000, "integrated")
	if r := build(t, l, oneBattery); r.CompleteHours() != 48 {
		t.Fatalf("%d complete hours with a plain charger", r.CompleteHours())
	}
	// One that does must read every hour, and what it gave counts like solar.
	l = site(arbitrage(0, 1))
	l.install(charger, state.AssetVehicleCharger)
	for i := range 49 {
		if i == 30 {
			continue // offline
		}
		wh := 0.0
		if i == 19 {
			wh = 2000
		}
		l.add(charger, state.FlowVehicleDischarge, yearStart.Add(time.Duration(i)*time.Hour), time.Hour, wh, "integrated")
	}
	r := build(t, l, oneBattery)
	if r.Hours[30].Complete || r.Hours[29].Complete || r.CompleteHours() != 46 {
		t.Fatalf("%d complete hours with the car charger offline", r.CompleteHours())
	}
	if h := r.Hours[19]; h.VehicleDischarge != 2000 || h.BatteryToGrid != 1000 || r.Problem != "" {
		t.Fatalf("export hour %+v", h)
	}
}

func TestSolarIsZeroAfterDusk(t *testing.T) {
	// A solar inverter sleeps from 16:00 in a Stockholm January. Its first
	// reading after waking ends the outage.
	sleeping := func(wake int) *fakeLedger {
		l := site(arbitrage(0, 1))
		for d := range 3 {
			l.drop(solar, d*24-8, d*24+wake)
			l.mark(solar, d*24+wake, "gap")
		}
		return l
	}
	placed := Site{CapacityWh: 10_000, Batteries: 1, Lat: 59.33, Lon: 18.07, Placed: true}
	full := build(t, site(arbitrage(0, 1)), placed)
	// Waking at 06:00, before dawn, loses nothing.
	if r := build(t, sleeping(6), placed); r.CompleteHours() != 48 || r.Returned != full.Returned {
		t.Fatalf("%d complete hours, %.1f Wh", r.CompleteHours(), r.Returned)
	}
	// Waking at 10:00 leaves the morning without solar readings. Those
	// hours attribute nothing, and the battery's readings keep the mix.
	r := build(t, sleeping(10), placed)
	for _, i := range []int{8, 9, 10, 32, 33, 34} {
		if r.Hours[i].Complete {
			t.Fatalf("hour %d counted without solar", i)
		}
	}
	for _, i := range []int{2, 19, 26, 43} {
		if !r.Hours[i].Complete {
			t.Fatalf("dark hour %d not counted", i)
		}
	}
	near(t, "late wake", r.Returned, full.Returned)
	// Unplaced, nothing says the sun was down.
	if r := build(t, sleeping(6), oneBattery); r.Hours[2].Complete || r.Hours[19].Complete || r.Returned != 0 {
		t.Fatalf("night hours counted at an unplaced site")
	}
}

func TestTheYearRunsOnSwedishTime(t *testing.T) {
	l := site(arbitrage(0, 1))
	// 23:00 UTC on 31 December is the first hour of the Swedish year.
	if got := l.points[grid][0].BucketStartMS; got != time.Date(2024, 12, 31, 23, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("first hour %v", time.UnixMilli(got).UTC())
	}
	r := build(t, l, oneBattery)
	if len(r.Hours) != 48 || !r.Hours[0].Start.Equal(yearStart) {
		t.Fatalf("%d hours from %v", len(r.Hours), r.Hours[0].Start)
	}
	// The hour that just ended waits for its last readings.
	r, err := Build(context.Background(), l, 2025, twoDays.Add(2*time.Minute), oneBattery)
	if err != nil || len(r.Hours) != 47 {
		t.Fatalf("%d hours, err %v", len(r.Hours), err)
	}
	// A year that has not started has no hours.
	if r, err := Build(context.Background(), l, 2026, reportNow, oneBattery); err != nil || len(r.Hours) != 0 || r.Returned != 0 {
		t.Fatalf("2026: %d hours, err %v", len(r.Hours), err)
	}
}

// TestTheStoreHoursAreTheReportHours reads hours the real ledger wrote:
// a grid meter by its counters and a battery by power, every two minutes.
func TestTheStoreHoursAreTheReportHours(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	from := yearStart.Add(-10 * time.Minute)
	until := yearStart.Add(3*time.Hour + 10*time.Minute)
	var obs []state.EnergyObservation
	for at := from; !at.After(until); at = at.Add(2 * time.Minute) {
		imported, exported := 2000*at.Sub(from).Hours(), 0.0
		charging, discharging := 2000.0, 0.0
		obs = append(obs,
			state.EnergyObservation{AssetID: grid, DeviceID: "meter:1", AssetKind: state.AssetGridMeter, Flow: state.FlowGridImport, AtMs: at.UnixMilli(), CounterWh: &imported},
			state.EnergyObservation{AssetID: grid, DeviceID: "meter:1", AssetKind: state.AssetGridMeter, Flow: state.FlowGridExport, AtMs: at.UnixMilli(), CounterWh: &exported},
			state.EnergyObservation{AssetID: battery, DeviceID: "inverter:1", AssetKind: state.AssetBattery, Flow: state.FlowBatteryCharge, AtMs: at.UnixMilli(), PowerW: &charging},
			state.EnergyObservation{AssetID: battery, DeviceID: "inverter:1", AssetKind: state.AssetBattery, Flow: state.FlowBatteryDischarge, AtMs: at.UnixMilli(), PowerW: &discharging},
		)
	}
	if err := st.RecordTickWithOptionalHistory(nil, nil, obs); err != nil {
		t.Fatal(err)
	}
	r, err := Build(context.Background(), st, 2025, yearStart.Add(3*time.Hour+settle), oneBattery)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Hours) != 3 || r.CompleteHours() != 3 {
		t.Fatalf("%d hours, %d complete", len(r.Hours), r.CompleteHours())
	}
	for _, h := range r.Hours {
		near(t, "import", h.GridImport, 2000)
		near(t, "charge", h.BatteryCharge, 2000)
		near(t, "grid to battery", h.GridToBattery, 2000)
	}
}

// TestASpreadOutageIsNotAnHour is the review's case: the grid meter's
// counter is offline from 09:30 to 22:30 while midday solar is exported and
// the battery feeds the house in the evening. The ledger spreads the export
// over the outage; none of it may count as battery energy sent out.
func TestASpreadOutageIsNotAnHour(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var obs []state.EnergyObservation
	var imported, exported float64
	from := yearStart.Add(-10 * time.Minute)
	for at := from; at.Before(yearStart.Add(25 * time.Hour)); at = at.Add(2 * time.Minute) {
		h := at.In(stockholm).Hour()
		pv, charge, discharge := 0.0, 0.0, 0.0
		switch {
		case h >= 1 && h < 5:
			charge = 2500
			imported += 2500.0 / 30
		case h >= 10 && h < 15:
			pv = 4000
			exported += 4000.0 / 30
		case h >= 18 && h < 21:
			discharge = 2500
		}
		p, c, d := pv, charge, discharge
		obs = append(obs,
			state.EnergyObservation{AssetID: solar, DeviceID: "inverter:1", AssetKind: state.AssetPV, Flow: state.FlowPVGeneration, AtMs: at.UnixMilli(), PowerW: &p},
			state.EnergyObservation{AssetID: battery, DeviceID: "inverter:1", AssetKind: state.AssetBattery, Flow: state.FlowBatteryCharge, AtMs: at.UnixMilli(), PowerW: &c},
			state.EnergyObservation{AssetID: battery, DeviceID: "inverter:1", AssetKind: state.AssetBattery, Flow: state.FlowBatteryDischarge, AtMs: at.UnixMilli(), PowerW: &d},
		)
		if local := at.In(stockholm); local.Day() == 1 && (local.Hour()*60+local.Minute() >= 9*60+30 && local.Hour()*60+local.Minute() < 22*60+30) {
			continue // the meter is offline
		}
		i, e := imported, exported
		obs = append(obs,
			state.EnergyObservation{AssetID: grid, DeviceID: "meter:1", AssetKind: state.AssetGridMeter, Flow: state.FlowGridImport, AtMs: at.UnixMilli(), CounterWh: &i},
			state.EnergyObservation{AssetID: grid, DeviceID: "meter:1", AssetKind: state.AssetGridMeter, Flow: state.FlowGridExport, AtMs: at.UnixMilli(), CounterWh: &e},
		)
	}
	if err := st.RecordTickWithOptionalHistory(nil, nil, obs); err != nil {
		t.Fatal(err)
	}
	r, err := Build(context.Background(), st, 2025, yearStart.Add(24*time.Hour+settle), oneBattery)
	if err != nil {
		t.Fatal(err)
	}
	if r.Returned != 0 || r.Hours[19].Complete {
		t.Fatalf("returned %.1f Wh; hour 19 %+v", r.Returned, r.Hours[19])
	}
}
