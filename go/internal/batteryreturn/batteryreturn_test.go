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
	grid     = "meter:1/grid_meter"
	battery  = "inverter:1/battery"
	battery2 = "inverter:2/battery"
	solar    = "inverter:1/pv"
	charger  = "charger:1/vehicle_charger"
)

// labels name each asset after the driver that read it.
var labels = map[string]string{grid: "meter", battery: "battery", battery2: "battery 2", solar: "solar", charger: "charger"}

// sized is a site with batteries of these sizes in Wh, read by "battery"
// and "battery 2".
func sized(wh ...float64) Site {
	var s Site
	for i, w := range wh {
		s.Batteries = append(s.Batteries, SizedBattery{Driver: []string{"battery", "battery 2"}[i], CapacityWh: w})
	}
	return s
}

// oneBattery is 10 kWh, configured, at an unplaced site.
var oneBattery = sized(10_000)

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
	f.assets = append(f.assets, state.EnergyAsset{AssetID: id, Kind: kind, Label: labels[id],
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

// another adds a battery with its own driver that charges and discharges as
// given in kWh. The site's grid and solar stay as they are.
func (f *fakeLedger) another(id string, hours map[int]flows) *fakeLedger {
	f.install(id, state.AssetBattery)
	for i := range 49 {
		at, h := yearStart.Add(time.Duration(i)*time.Hour), hours[i]
		f.add(id, state.FlowBatteryCharge, at, time.Hour, h.chg*1000, "integrated")
		f.add(id, state.FlowBatteryDischarge, at, time.Hour, h.dis*1000, "integrated")
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

// Two days of arbitrage charge 20 kWh and discharge 18: an efficiency of
// 0.9.

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
	if b := r.Batteries; len(b) != 1 || math.Abs(b[0].Efficiency-0.9) > 1e-9 || b[0].CapacityWh != 10_000 || r.CompleteHours() != 48 || r.Problem != "" {
		t.Fatalf("batteries %+v complete %d problem %q", b, r.CompleteHours(), r.Problem)
	}
	// The year starts with a battery full of energy from elsewhere. Each
	// night thins it out, so the second evening returns more than the
	// first, and never more than the grid energy left after the loss.
	first, second := returnedOn(r, 0), returnedOn(r, 1)
	if first <= 0 || second <= first || r.Returned > 0.9*20_000 {
		t.Fatalf("returned %.1f then %.1f Wh", first, second)
	}
	near(t, "year", r.Returned, first+second)
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
	for i, h := range r.Hours {
		if h.GridHeld+h.OtherHeld > 10_000+1e-6 {
			t.Fatalf("hour %d holds %.1f Wh", i, h.GridHeld+h.OtherHeld)
		}
	}
}

func TestAnEmptyBatteryFilledFromTheGridReturnsAllItSends(t *testing.T) {
	// The house empties the battery first. More came out than went in, so
	// the efficiency is 1. Each night brings 10 kWh of grid energy and each
	// evening sends 9 to the grid; the second night tops up the 1 kWh left.
	hours := arbitrage(0, 1)
	hours[0] = flows{dis: 10}
	r := build(t, site(hours), oneBattery)
	near(t, "first day", returnedOn(r, 0), 9000)
	near(t, "second day", returnedOn(r, 1), 9000)
}

func TestWhatLeavesCarriesTheGridShare(t *testing.T) {
	// The house empties the battery, the grid puts in 5 kWh at night, the
	// house takes 2.5 in the morning, the sun puts in 5 at midday and 5 go
	// to the grid in the evening. Then a third of the battery is grid
	// energy: 2.5 of 7.5 kWh.
	r := build(t, site(map[int]flows{
		0:  {dis: 10},
		2:  {imp: 5, chg: 5},
		7:  {dis: 2.5},
		12: {pv: 5, chg: 5},
		19: {dis: 5, exp: 5},
	}), oneBattery)
	near(t, "returned", r.Returned, 5000.0/3)
}

func TestWithinAnHourTheChargeMixesInFirst(t *testing.T) {
	// The house empties the battery and the sun puts in 5 kWh. One hour the
	// grid puts in 4 while the house takes 4, so the house takes 4/9 grid
	// energy and leaves 2.22 kWh of it among 5. The next hour 5 kWh go to
	// the grid with that share.
	r := build(t, site(map[int]flows{
		8:  {dis: 10},
		36: {pv: 5, chg: 5},
		44: {imp: 4, chg: 4, dis: 4},
		45: {dis: 5, exp: 5},
	}), oneBattery)
	near(t, "returned", r.Returned, 4.0*5/9*1000)
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
	// The battery holds the same as in plain arbitrage and only a third of
	// each hour's discharge leaves the site.
	plain := build(t, site(arbitrage(0, 1)), oneBattery)
	near(t, "second day", returnedOn(r, 1), returnedOn(plain, 1)/3)
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
	// Without the gap the second day returns 2 kWh.
	if got := returnedOn(r, 1); got != 0 {
		t.Fatalf("second day returned %.1f Wh after the gap", got)
	}
}

func TestOnlyMeasuredReadingsCount(t *testing.T) {
	// A recovered counter delta is spread evenly over an outage, so it can
	// move midday export into an evening hour.
	for _, quality := range []string{"recovered", "gap", "estimated", "invalid", "reset"} {
		// Without the grid meter the hour attributes nothing, but the
		// battery's readings keep the mix.
		// Without the grid meter's reading the hour credits nothing, but
		// the battery's readings keep the account.
		l := site(arbitrage(0, 1))
		l.mark(grid, 44, quality)
		r := build(t, l, oneBattery)
		if h := r.Hours[44]; h.Complete || r.Hours[43].Complete || h.Returned != 0 || r.Hours[43].Returned != 0 || h.GridHeld+h.OtherHeld == 10_000 {
			t.Fatalf("%s grid reading counted: %+v", quality, r.Hours[42:45])
		}
		// Without the battery's, the account starts over: full of other
		// energy.
		l = site(arbitrage(0, 1))
		l.mark(battery, 43, quality)
		if r := build(t, l, oneBattery); r.Hours[43].Complete || r.Hours[43].GridHeld != 0 || r.Hours[43].OtherHeld != 10_000 || returnedOn(r, 1) != 0 {
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

// renew moves an asset to a new identity: the old one reads until 10
// minutes into hour from, the new one starts there with the ledger's
// baseline marker and reads from hour from+1 on. With silent > 0 the old
// one stops that many hours earlier.
func (f *fakeLedger) renew(id string, from, silent int) string {
	next := id + "+serial"
	handover := yearStart.Add(time.Duration(from)*time.Hour + 10*time.Minute).UnixMilli()
	for i, a := range f.assets {
		if a.AssetID != id {
			continue
		}
		f.assets[i].LastSeenMS = handover - int64(silent)*hourMS
		f.assets = append(f.assets, state.EnergyAsset{AssetID: next, Kind: a.Kind, FirstSeenMS: handover, LastSeenMS: a.LastSeenMS})
		break
	}
	var kept []state.EnergyLedgerPoint
	for _, p := range f.points[id] {
		switch i := int((p.BucketStartMS - yearStart.UnixMilli()) / hourMS); {
		case i > from:
			p.AssetID = next
			f.points[next] = append(f.points[next], p)
		case i <= from-silent:
			kept = append(kept, p)
		}
	}
	f.points[id] = kept
	f.add(next, state.FlowGridImport, yearStart.Add(time.Duration(from)*time.Hour), time.Hour, 0, "gap")
	return next
}

func TestANewIdentityContinuesTheMeter(t *testing.T) {
	// The inverter's driver learns its serial at 06:10 on the second day,
	// and the grid meter, battery and solar get new asset ids.
	l := site(arbitrage(0, 1))
	for _, id := range []string{grid, battery, solar} {
		l.renew(id, 30, 0)
	}
	r := build(t, l, oneBattery)
	if r.Problem != "" || len(r.Batteries) != 1 {
		t.Fatalf("problem %q, %d batteries", r.Problem, len(r.Batteries))
	}
	// Only the handover hour is incomplete; it resets the mix.
	if r.CompleteHours() != 47 || r.Hours[30].Complete || !r.Hours[29].Complete || !r.Hours[31].Complete {
		t.Fatalf("%d complete hours; 29/30/31 %v/%v/%v", r.CompleteHours(), r.Hours[29].Complete, r.Hours[30].Complete, r.Hours[31].Complete)
	}

	// A battery that fell silent at 22:10 on the first day and came back
	// under a new id at 06:10 owes the night it did not read.
	l = site(arbitrage(0, 1))
	l.renew(battery, 30, 8)
	r = build(t, l, oneBattery)
	if len(r.Batteries) != 1 || r.Problem != "" {
		t.Fatalf("problem %q, %d batteries", r.Problem, len(r.Batteries))
	}
	for i := 22; i <= 30; i++ {
		if r.Hours[i].Complete {
			t.Fatalf("hour %d counted while the battery was silent", i)
		}
	}
	if got := returnedOn(r, 1); got != 0 {
		t.Fatalf("second day returned %.1f Wh after the silent night", got)
	}
}

func TestANewBatteryStartsItsOwnAccount(t *testing.T) {
	// A new battery asset takes over at 06:10 on the second day, and both
	// read that hour without a gap marker. It may be new hardware, so the
	// grid energy the old battery took that night does not carry over.
	l := site(arbitrage(0, 1))
	const next = battery + "+new"
	handover := yearStart.Add(30*time.Hour + 10*time.Minute).UnixMilli()
	l.assets[1].LastSeenMS = handover
	l.assets = append(l.assets, state.EnergyAsset{AssetID: next, Kind: state.AssetBattery, FirstSeenMS: handover, LastSeenMS: reportNow.UnixMilli()})
	var kept []state.EnergyLedgerPoint
	for _, p := range l.points[battery] {
		i := int((p.BucketStartMS - yearStart.UnixMilli()) / hourMS)
		if i >= 30 {
			q := p
			q.AssetID = next
			l.points[next] = append(l.points[next], q)
		}
		if i <= 30 {
			kept = append(kept, p)
		}
	}
	l.points[battery] = kept
	r := build(t, l, oneBattery)
	if len(r.Batteries) != 1 || r.Problem != "" || r.Hours[30].Complete {
		t.Fatalf("problem %q, %d batteries, hour 30 complete %v", r.Problem, len(r.Batteries), r.Hours[30].Complete)
	}
	if got := returnedOn(r, 1); got != 0 {
		t.Fatalf("second day returned %.1f Wh across the swap", got)
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

func TestEveryBatteryNeedsItsSize(t *testing.T) {
	if r := build(t, site(arbitrage(0, 1)), Site{}); r.Problem != NoSize || r.Returned != 0 || r.CompleteHours() != 0 {
		t.Fatalf("no size: problem %q, %.1f Wh", r.Problem, r.Returned)
	}
	// A second battery without a size.
	if r := build(t, site(arbitrage(0, 1)).another(battery2, arbitrage(0, 1)), oneBattery); r.Problem != NoSize || r.Returned != 0 || len(r.Batteries) != 2 || r.Batteries[1].Sized {
		t.Fatalf("second battery unsized: problem %q, %.1f Wh, %+v", r.Problem, r.Returned, r.Batteries)
	}
	// A size for a battery the ledger never read, or did not read in the
	// year: what it did could have been counted for the first.
	r := build(t, site(arbitrage(0, 1)), sized(10_000, 5000))
	if r.Problem != Unread || r.Returned != 0 || len(r.Unread) != 1 || r.Unread[0] != "battery 2" {
		t.Fatalf("never read: problem %q, unread %v", r.Problem, r.Unread)
	}
	l := site(arbitrage(0, 1))
	l.install(battery2, state.AssetBattery)
	l.assets[3].FirstSeenMS, l.assets[3].LastSeenMS = yearStart.AddDate(-1, 0, 0).UnixMilli(), yearStart.Add(-time.Hour).UnixMilli()
	if r := build(t, l, sized(10_000, 5000)); r.Problem != Unread || len(r.Batteries) != 1 {
		t.Fatalf("read before the year: problem %q, %+v", r.Problem, r.Batteries)
	}
	// One first read after the year was not there to read.
	l.assets[3].FirstSeenMS, l.assets[3].LastSeenMS = reportNow.Add(time.Hour).UnixMilli(), reportNow.Add(2*time.Hour).UnixMilli()
	if r := build(t, l, sized(10_000, 5000)); r.Problem != "" || len(r.Batteries) != 1 || r.Returned != build(t, site(arbitrage(0, 1)), oneBattery).Returned {
		t.Fatalf("read after the year: problem %q, %.1f Wh", r.Problem, r.Returned)
	}
	// No battery at all is no problem, just nothing.
	l = site(nil)
	l.assets = append(l.assets[:1], l.assets[2:]...)
	if r := build(t, l, Site{}); r.Problem != "" || r.Returned != 0 || r.CompleteHours() != 48 {
		t.Fatalf("no battery: problem %q, %d complete hours", r.Problem, r.CompleteHours())
	}
}

// TestOneBatterysExportsDoNotCarryAnothersGridEnergy is the second review's
// case: battery A charges only from solar and sends 9 kWh to the grid each
// evening, while battery B charges 10 kWh from the grid each night and
// never discharges. One mix for both would count A's exports as B's grid
// energy.
func TestOneBatterysExportsDoNotCarryAnothersGridEnergy(t *testing.T) {
	a, b := map[int]flows{}, map[int]flows{}
	for d := range 2 {
		for h := 1; h <= 4; h++ {
			a[d*24+h] = flows{imp: 2.5}
			b[d*24+h] = flows{chg: 2.5}
		}
		for h := 10; h <= 13; h++ {
			a[d*24+h] = flows{pv: 2.5, chg: 2.5}
		}
		for h := 18; h <= 20; h++ {
			a[d*24+h] = flows{exp: 3, dis: 3}
		}
	}
	r := build(t, site(a).another(battery2, b), sized(10_000, 20_000))
	if r.Problem != "" || r.CompleteHours() != 48 || r.Returned != 0 {
		t.Fatalf("problem %q, %d complete hours, returned %.1f Wh", r.Problem, r.CompleteHours(), r.Returned)
	}
	// The bounds still give B the grid energy and A the export.
	if h := r.Hours[2].Batteries; h[0].GridToBattery != 0 || h[1].GridToBattery != 2500 {
		t.Fatalf("night %+v", h)
	}
	if h := r.Hours[19].Batteries; h[0].BatteryToGrid != 3000 || h[1].BatteryToGrid != 0 {
		t.Fatalf("evening %+v", h)
	}
}

// bothFromTheGrid is A doing arbitrage as above with 10 kWh, and B the same
// at half the power with 5 kWh: each night hour charges A 2.5 kWh and B
// 1.25, each evening hour sends 3 and 1.5 to the grid. It returns the site
// with A, and B.
func bothFromTheGrid() (map[int]flows, map[int]flows) {
	a, b := arbitrage(0, 1), map[int]flows{}
	for i, h := range a {
		b[i] = flows{chg: h.chg / 2, dis: h.dis / 2}
		a[i] = flows{imp: h.imp * 1.5, exp: h.exp * 1.5, chg: h.chg, dis: h.dis}
	}
	return a, b
}

func TestTwoBatteriesFromTheGridEachReturnTheirOwn(t *testing.T) {
	a, b := bothFromTheGrid()
	r := build(t, site(a).another(battery2, b), sized(10_000, 5000))
	if r.Problem != "" || r.CompleteHours() != 48 || len(r.Batteries) != 2 {
		t.Fatalf("problem %q, %d complete hours", r.Problem, r.CompleteHours())
	}
	// In the evening the export is both batteries' discharge, so each sent
	// all of its own: 4.5 kWh out less the other's 1.5 or 3.
	if h := r.Hours[19].Batteries; h[0].BatteryToGrid != 3000 || h[1].BatteryToGrid != 1500 {
		t.Fatalf("evening %+v", h)
	}
	// A is the one-battery arbitrage. B is the same at half the size and
	// half the flows, so it returns half as much.
	if bat := r.Batteries; math.Abs(bat[0].Efficiency-0.9) > 1e-9 || math.Abs(bat[1].Efficiency-0.9) > 1e-9 || bat[1].CapacityWh != 5000 {
		t.Fatalf("batteries %+v", bat)
	}
	plain := build(t, site(arbitrage(0, 1)), oneBattery)
	near(t, "A", r.Batteries[0].Returned, plain.Returned)
	near(t, "B", r.Batteries[1].Returned, plain.Returned/2)
	near(t, "year", r.Returned, 1.5*plain.Returned)
	if a, b := r.Hours[24+4].Batteries[0], r.Hours[24+4].Batteries[1]; math.Abs(b.GridHeld-a.GridHeld/2) > 1 || math.Abs(b.OtherHeld-a.OtherHeld/2) > 1 {
		t.Fatalf("after the second night A %+v B %+v", a, b)
	}
}

func TestExportEitherBatteryCouldHaveSentCountsLow(t *testing.T) {
	// On the second evening the batteries discharge 4.5 kWh an hour but only
	// 3 go to the grid. B's 1.5 could be part of it, so A sent at least 1.5;
	// A's 3 could be all of it, so B sent at least nothing.
	a, b := bothFromTheGrid()
	for h := 18; h <= 20; h++ {
		a[24+h] = flows{exp: 3, dis: 3}
	}
	r := build(t, site(a).another(battery2, b), sized(10_000, 5000))
	if h := r.Hours[24+19].Batteries; h[0].BatteryToGrid != 1500 || h[1].BatteryToGrid != 0 {
		t.Fatalf("evening %+v", h)
	}
	// Up to the second evening both hold what they did in the plain case.
	// Then half of A's discharge counts as sent out and none of B's.
	plain := build(t, site(arbitrage(0, 1)), oneBattery)
	near(t, "A", r.Batteries[0].Returned, returnedOn(plain, 0)+returnedOn(plain, 1)/2)
	near(t, "B", r.Batteries[1].Returned, returnedOn(plain, 0)/2)
}

func TestABatterysMissingHourForgetsOnlyItsAccount(t *testing.T) {
	// B's reading at 10:00 on the first day has a gap. Hours 9 and 10 count
	// for neither battery, but only B starts over full of other energy.
	a, b := bothFromTheGrid()
	l := site(a).another(battery2, b)
	l.mark(battery2, 10, "gap")
	r := build(t, l, sized(10_000, 5000))
	if r.CompleteHours() != 46 || r.Hours[9].Complete || r.Hours[10].Complete {
		t.Fatalf("%d complete hours", r.CompleteHours())
	}
	plain := build(t, site(arbitrage(0, 1)), oneBattery)
	near(t, "A", r.Batteries[0].Returned, plain.Returned)
	// B starts over after the gap full of other energy, so its first night
	// is forgotten: the first evening returns nothing and the second less
	// than without the gap.
	first := 0.0
	for _, h := range r.Hours[:24] {
		first += h.Batteries[1].Returned
	}
	if first != 0 || r.Batteries[1].Returned <= 0 || r.Batteries[1].Returned >= plain.Returned/2 {
		t.Fatalf("B returned %.1f on the first day, %.1f Wh in all", first, r.Batteries[1].Returned)
	}
}

func TestTheSizeIsAtLeastWhatTheBatteryGainedInADay(t *testing.T) {
	// Configured as 5 kWh, the battery gains 9 kWh a night after loss.
	r := build(t, site(arbitrage(0, 1)), sized(5000))
	if r.Batteries[0].CapacityWh != 9000 {
		t.Fatalf("capacity %v", r.Batteries[0].CapacityWh)
	}
	// A larger size only counts lower.
	small, large := build(t, site(arbitrage(0, 1)), oneBattery), build(t, site(arbitrage(0, 1)), sized(15_000))
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
	placed := Site{Batteries: oneBattery.Batteries, Lat: 59.33, Lon: 18.07, Placed: true}
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
			state.EnergyObservation{AssetID: battery, DeviceID: "inverter:1", AssetKind: state.AssetBattery, Label: "battery", Flow: state.FlowBatteryCharge, AtMs: at.UnixMilli(), PowerW: &charging},
			state.EnergyObservation{AssetID: battery, DeviceID: "inverter:1", AssetKind: state.AssetBattery, Label: "battery", Flow: state.FlowBatteryDischarge, AtMs: at.UnixMilli(), PowerW: &discharging},
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
			state.EnergyObservation{AssetID: battery, DeviceID: "inverter:1", AssetKind: state.AssetBattery, Label: "battery", Flow: state.FlowBatteryCharge, AtMs: at.UnixMilli(), PowerW: &c},
			state.EnergyObservation{AssetID: battery, DeviceID: "inverter:1", AssetKind: state.AssetBattery, Label: "battery", Flow: state.FlowBatteryDischarge, AtMs: at.UnixMilli(), PowerW: &d},
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
