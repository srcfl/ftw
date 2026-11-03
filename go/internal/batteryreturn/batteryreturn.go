// Package batteryreturn works out the figure a Swedish household reports to
// Skatteverket to get back the energy tax on electricity it bought from the
// grid, stored in a battery and fed back to the same grid (11 kap. 13 § lagen
// (1994:1776) om skatt på energi). The owner applies once per calendar year
// and must be able to show how the figure was reached, so the report keeps
// every hour.
//
// The ledger says how much each meter counted, not where each kWh went. The
// report counts low where the readings leave room, and shares in proportion
// where nothing can say:
//
//   - In each hour a battery took from the grid only what solar, a
//     discharging car and the other batteries could not have supplied, and
//     gave to the grid only what they could not have exported. Every source
//     on the site must be one FTW reads.
//   - Each battery keeps an account of its own, so one battery's exports
//     never carry another's grid energy. The account follows what the
//     battery holds and how much of it came from the grid. Energy enters
//     after the battery's round-trip efficiency over the year, so losses
//     are never refunded. Whatever leaves, to the house or the grid, carries
//     the grid's share of what the battery held. The ledger does not say
//     what came first within an hour, so the hour's charge mixes in before
//     its discharge leaves.
//   - An hour counts only when every meter that reported during the year
//     read all of it. An hour that does not count adds nothing, and what a
//     battery took in it counts as energy from elsewhere. Without a
//     battery's own readings that battery is taken to be full of energy from
//     elsewhere after the hour. The year starts the same way, so every
//     battery needs its configured size.
package batteryreturn

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/sunpos"
)

// Zone is the clock Skatteverket's calendar year runs on.
const Zone = "Europe/Stockholm"

const (
	hourMS = int64(time.Hour / time.Millisecond)
	// settle leaves the ledger time to write the hour that just ended.
	settle = 5 * time.Minute
	// civilDuskZenithDeg: with the sun 6° below the horizon a panel makes
	// nothing, so a sleeping solar inverter reads 0.
	civilDuskZenithDeg = 96
)

// Ledger is the part of the store the report reads.
type Ledger interface {
	EnergyAssets() ([]state.EnergyAsset, error)
	LoadEnergyHistoryContext(context.Context, state.EnergyHistoryQuery) ([]state.EnergyLedgerPoint, bool, error)
}

// Site is what the report needs beyond the ledger.
type Site struct {
	// Batteries are the drivers with a battery size in Settings. Each
	// account needs an upper bound on what its battery holds.
	Batteries []SizedBattery
	// Lat and Lon place the site when Placed. Then a solar inverter that
	// sleeps after dusk reads 0 instead of leaving the night incomplete.
	Lat, Lon float64
	Placed   bool
}

// SizedBattery is a driver with a battery size in Settings. The ledger names
// each asset after the driver that read it last, so the size belongs to the
// battery meter with an asset of that name.
type SizedBattery struct {
	Driver     string
	CapacityWh float64
}

// Hour is one clock hour of the basis. Energies are Wh.
type Hour struct {
	Start      time.Time
	GridImport float64
	GridExport float64
	// BatteryCharge and BatteryDischarge are the batteries together.
	BatteryCharge    float64
	BatteryDischarge float64
	Solar            float64
	VehicleDischarge float64
	// GridToBattery, BatteryToGrid, Returned, GridHeld and OtherHeld are the
	// sums over Batteries.
	GridToBattery float64
	BatteryToGrid float64
	Returned      float64
	GridHeld      float64
	OtherHeld     float64
	Complete      bool
	// Batteries is each battery's part, in the order of Report.Batteries.
	Batteries []BatteryHour
}

// BatteryHour is one battery's part of an hour. Energies are Wh.
type BatteryHour struct {
	Charge    float64
	Discharge float64
	// GridToBattery is the least grid energy the battery can have taken.
	GridToBattery float64
	// BatteryToGrid is the least of its energy that can have gone out to
	// the grid.
	BatteryToGrid float64
	// Returned is the part of BatteryToGrid that was grid energy stored in
	// this battery earlier: the refundable energy.
	Returned float64
	// GridHeld and OtherHeld are what it holds after the hour.
	GridHeld  float64
	OtherHeld float64
	// Efficiency is the battery's round-trip efficiency over the year.
	Efficiency float64
}

// Month sums the hours of one calendar month.
type Month struct {
	Returned      float64
	Hours         int
	CompleteHours int
}

// Problem says why a year has no figure.
type Problem string

const (
	// NoSize: a battery the ledger read in the year has no size in Settings.
	NoSize Problem = "battery_size"
	// Unread: the ledger did not read a battery that has a size in Settings.
	// What it did could have been counted for the others.
	Unread Problem = "battery_unread"
)

// Report is one calendar year, or the part of it that has passed.
type Report struct {
	Year     int
	Returned float64
	Hours    []Hour
	Months   [12]Month
	// Batteries are the batteries the ledger read in the year.
	Batteries []Battery
	// Unread names the drivers with a battery size that the ledger did not
	// read in the year.
	Unread []string
	// Problem is set when nothing is returned for a reason other than the
	// readings.
	Problem Problem
}

// Battery is one battery meter the ledger read in the year, with its own
// account.
type Battery struct {
	// AssetID is its latest identity, and Label the driver that read it
	// last.
	AssetID string
	Label   string
	// Sized: Settings holds its size.
	Sized bool
	// Efficiency is what came out over what went in, over its read hours.
	Efficiency float64
	// CapacityWh caps what it holds: the configured size, or the most it
	// gained in a day if that is more.
	CapacityWh float64
	Returned   float64
}

// CompleteHours counts the hours with complete readings.
func (r Report) CompleteHours() int {
	n := 0
	for _, m := range r.Months {
		n += m.CompleteHours
	}
	return n
}

type role int

const (
	roleNone role = iota
	roleGrid
	roleBattery
	roleSolar
	roleVehicle
)

func roleOf(kind state.EnergyAssetKind) role {
	switch kind {
	case state.AssetGridMeter:
		return roleGrid
	case state.AssetBattery:
		return roleBattery
	case state.AssetPV:
		return roleSolar
	case state.AssetVehicleCharger:
		return roleVehicle
	}
	return roleNone
}

// reads reports whether the report reads flow from an asset with this role.
func (r role) reads(flow state.EnergyFlow) bool {
	switch r {
	case roleGrid:
		return flow == state.FlowGridImport || flow == state.FlowGridExport
	case roleBattery:
		return flow == state.FlowBatteryCharge || flow == state.FlowBatteryDischarge
	case roleSolar:
		return flow == state.FlowPVGeneration
	case roleVehicle:
		return flow == state.FlowVehicleCharge || flow == state.FlowVehicleDischarge
	}
	return false
}

// readings is what one asset recorded in each hour.
type readings struct {
	seen []bool // a measured or integrated reading
	// resumed marks the hour a reading came back after an outage: a gap
	// marker, or a counter delta the ledger spread over the outage. The
	// outage's hours lack their share, so neither counts.
	resumed []bool
	bad     []bool // anything else that is not a reading
}

func (r readings) ok(i int) bool { return r.seen[i] && !r.resumed[i] && !r.bad[i] }

// Build reads the year from the ledger. Hours from now on are left out.
func Build(ctx context.Context, l Ledger, year int, now time.Time, site Site) (Report, error) {
	loc, err := time.LoadLocation(Zone)
	if err != nil {
		return Report{}, err
	}
	start := time.Date(year, 1, 1, 0, 0, 0, 0, loc)
	end := time.Date(year+1, 1, 1, 0, 0, 0, 0, loc)
	if cut := now.Add(-settle).Truncate(time.Hour); cut.Before(end) {
		end = cut
	}
	r := Report{Year: year}
	if !end.After(start) {
		return r, nil
	}
	from, to := start.UnixMilli(), end.UnixMilli()
	n := int((to - from) / hourMS)
	r.Hours = make([]Hour, n)
	for i := range r.Hours {
		r.Hours[i].Start = start.Add(time.Duration(i) * time.Hour)
		r.Hours[i].Complete = true
	}
	// dark[i]: the whole hour is past civil dusk at the site.
	dark := make([]bool, n+1)
	for i := range dark {
		if !site.Placed {
			break
		}
		at := start.Add(time.Duration(i) * time.Hour)
		dark[i] = true
		for _, t := range []time.Time{at, at.Add(30 * time.Minute), at.Add(time.Hour)} {
			dark[i] = dark[i] && sunpos.At(t, site.Lat, site.Lon).ZenithDeg > civilDuskZenithDeg
		}
	}

	assets, err := l.EnergyAssets()
	if err != nil {
		return Report{}, err
	}
	owed, meters := spans(assets, from, to)
	r.sizeBatteries(assets, meters, site.Batteries, to)
	for i := range r.Hours {
		r.Hours[i].Batteries = make([]BatteryHour, len(r.Batteries))
	}
	metered := false
	unread := make([][]bool, len(r.Batteries)) // a battery's readings are missing
	for b := range unread {
		unread[b] = make([]bool, n)
	}
	for _, a := range assets {
		role := roleOf(a.Kind)
		sp, found := owed[a.AssetID]
		if role == roleNone || !found {
			continue
		}
		// owes reports whether the asset answers for hour i.
		owes := func(i int) bool {
			at := from + int64(i)*hourMS
			return at < sp.to && at+hourMS > sp.from
		}
		b := sp.meter // the battery meter, for a battery
		// One hour past the end shows whether the last hour's readings ran
		// to its end.
		rd := readings{make([]bool, n+1), make([]bool, n+1), make([]bool, n+1)}
		vehicle := false
		err := loadAsset(ctx, l, a.AssetID, start, end.Add(time.Hour), func(p state.EnergyLedgerPoint) {
			i := int((p.BucketStartMS - from) / hourMS)
			if p.BucketLenMS != hourMS {
				// A daily total cannot be split into hours.
				for j := max(i, 0); j <= n && int64(j) < (p.BucketStartMS+p.BucketLenMS-from+hourMS-1)/hourMS; j++ {
					rd.bad[j] = true
				}
				return
			}
			if i < 0 || i > n || !role.reads(p.Flow) {
				return
			}
			vehicle = vehicle || p.Flow == state.FlowVehicleDischarge
			switch p.Quality {
			case "measured", "integrated":
				rd.seen[i] = true
			case "gap", "recovered":
				rd.resumed[i] = true
				return
			default:
				rd.bad[i] = true
				return
			}
			if i == n {
				return
			}
			h := &r.Hours[i]
			switch p.Flow {
			case state.FlowGridImport:
				h.GridImport += p.EnergyWh
			case state.FlowGridExport:
				h.GridExport += p.EnergyWh
			case state.FlowBatteryCharge:
				h.BatteryCharge += p.EnergyWh
				h.Batteries[b].Charge += p.EnergyWh
			case state.FlowBatteryDischarge:
				h.BatteryDischarge += p.EnergyWh
				h.Batteries[b].Discharge += p.EnergyWh
			case state.FlowPVGeneration:
				h.Solar += p.EnergyWh
			case state.FlowVehicleDischarge:
				h.VehicleDischarge += p.EnergyWh
			}
		})
		if err != nil {
			return Report{}, err
		}
		ok := rd.ok
		switch role {
		case roleVehicle:
			if !vehicle {
				continue // a charger that never gives energy back
			}
		case roleSolar:
			// Past dusk solar is 0 whether or not the inverter answers, and
			// the morning's first reading only ends a night without sun.
			ok = func(i int) bool {
				return dark[i] || (rd.seen[i] && !rd.bad[i] && (!rd.resumed[i] || i > 0 && dark[i-1]))
			}
		case roleGrid:
			metered = true
		}
		// A battery that takes over from another asset may be new hardware:
		// nothing of the old account carries over.
		if role == roleBattery && sp.from != math.MinInt64 && sp.from >= from && sp.from < to {
			i := int((sp.from - from) / hourMS)
			unread[b][i], r.Hours[i].Complete = true, false
		}
		// A meter that reported during the year must read every hour it
		// answers for. Silence, before or after, is not a quiet hour.
		for i := range r.Hours {
			if owes(i) && (!ok(i) || owes(i+1) && !ok(i+1)) {
				r.Hours[i].Complete = false
				if role == roleBattery {
					unread[b][i] = true
				}
			}
		}
	}

	for b := range r.Batteries {
		r.Batteries[b].fit(r.Hours, b, unread[b], loc)
	}
	for i := range r.Hours {
		h := &r.Hours[i]
		// Without a grid meter nothing says what crossed the connection.
		h.Complete = h.Complete && metered
		m := &r.Months[h.Start.In(loc).Month()-1]
		m.Hours++
		if !h.Complete {
			continue
		}
		if r.Problem == "" {
			m.CompleteHours++
		}
		for b := range h.Batteries {
			// Solar, a discharging car and the other batteries may each have
			// supplied this battery's charge or the export.
			bh := &h.Batteries[b]
			others := h.BatteryDischarge - bh.Discharge
			bh.GridToBattery = clamp(bh.Charge-h.Solar-h.VehicleDischarge-others, h.GridImport)
			bh.BatteryToGrid = clamp(h.GridExport-h.Solar-h.VehicleDischarge-others, bh.Discharge)
			h.GridToBattery += bh.GridToBattery
			h.BatteryToGrid += bh.BatteryToGrid
		}
	}
	for b := range r.Batteries {
		r.account(b, unread[b], loc)
	}
	return r, nil
}

// sizeBatteries lists the year's battery meters with their sizes and sets
// the problem, if any. A driver's size belongs to the meter with an asset of
// its name, so a new identity behind the driver keeps it.
func (r *Report) sizeBatteries(all []state.EnergyAsset, meters [][]state.EnergyAsset, sized []SizedBattery, to int64) {
	sizes := map[string]float64{}
	for _, s := range sized {
		sizes[s.Driver] = s.CapacityWh
	}
	read := map[string]bool{} // drivers named by a meter read in the year
	unsized := false
	for _, meter := range meters {
		bat := Battery{AssetID: meter[len(meter)-1].AssetID, Efficiency: 1}
		for _, a := range meter {
			if a.Label != "" {
				bat.Label = a.Label
			}
			// A meter named by two drivers takes the larger size, which
			// counts lower.
			if wh, ok := sizes[a.Label]; ok {
				bat.Sized, bat.CapacityWh = true, max(bat.CapacityWh, wh)
				read[a.Label] = true
			}
		}
		unsized = unsized || !bat.Sized
		r.Batteries = append(r.Batteries, bat)
	}
	for _, s := range sized {
		// A battery first read after the year was not there to read, like
		// solar added later.
		later := false
		for _, a := range all {
			later = later || a.Kind == state.AssetBattery && a.Label == s.Driver && a.FirstSeenMS >= to
		}
		if !read[s.Driver] && !later {
			r.Unread = append(r.Unread, s.Driver)
		}
	}
	switch {
	case unsized:
		r.Problem = NoSize
	case len(r.Unread) > 0:
		r.Problem = Unread
	}
}

// fit sets the battery's efficiency and size from its readings.
func (bat *Battery) fit(hours []Hour, b int, unread []bool, loc *time.Location) {
	var in, out float64
	for i, h := range hours {
		if !unread[i] {
			in += h.Batteries[b].Charge
			out += h.Batteries[b].Discharge
		}
	}
	bat.Efficiency = 0
	if in > 0 {
		bat.Efficiency = min(out/in, 1)
	}
	for i := range hours {
		hours[i].Batteries[b].Efficiency = bat.Efficiency
	}
	// A battery that holds what it gained in a day is at least that large,
	// whatever the configuration says.
	var held, low, high float64
	for i, h := range hours {
		if unread[i] || i > 0 && h.Start.In(loc).YearDay() != hours[i-1].Start.In(loc).YearDay() {
			held, low, high = 0, 0, 0
		}
		if unread[i] {
			continue
		}
		held += bat.Efficiency*h.Batteries[b].Charge - h.Batteries[b].Discharge
		low, high = min(low, held), max(high, held)
		bat.CapacityWh = max(bat.CapacityWh, high-low)
	}
}

// account follows battery b's grid and other energy through the year.
func (r *Report) account(b int, unread []bool, loc *time.Location) {
	bat := &r.Batteries[b]
	k := bat.CapacityWh
	grid, other := 0.0, k // unknown content at the start
	for i := range r.Hours {
		h := &r.Hours[i]
		bh := &h.Batteries[b]
		if unread[i] || r.Problem != "" {
			grid, other = 0, k
		} else {
			// An incomplete hour has no bounds: all its discharge counts as
			// used at home and all its charge as energy from elsewhere.
			grid += bh.Efficiency * bh.GridToBattery
			other += bh.Efficiency * (bh.Charge - bh.GridToBattery)
			// The account never holds more than a full battery.
			if held := grid + other; held > k {
				grid, other = grid*k/held, other*k/held
			}
			// What leaves carries the grid's share of what the battery held.
			// More out than the account holds means the account ran low;
			// the rest carries nothing from the grid.
			held := grid + other
			out := min(bh.Discharge, held)
			if held > 0 {
				share := grid / held
				bh.Returned = min(bh.BatteryToGrid, out) * share
				grid -= out * share
				other -= out * (1 - share)
			}
		}
		bh.GridHeld, bh.OtherHeld = grid, other
		h.GridHeld += grid
		h.OtherHeld += other
		h.Returned += bh.Returned
		bat.Returned += bh.Returned
		r.Months[h.Start.In(loc).Month()-1].Returned += bh.Returned
		r.Returned += bh.Returned
	}
}

// span is the time [from, to) an asset answers for, in Unix ms. A battery's
// meter is its index among the batteries.
type span struct {
	from, to int64
	meter    int
}

// handoverMS allows for the old and new identity reading in the same tick
// in either order.
const handoverMS = int64(2 * time.Minute / time.Millisecond)

// spans gives each asset that matters for [from, to) the time it answers
// for, and lists the battery meters.
//
// A device whose identity changes, a driver that learns the serial for
// instance, shows up as a new asset of the same kind once the old one
// stops. Assets of one kind that never read at the same time form one
// meter: each answers from its first reading until the next one's first
// reading, so the hours between them are owed by a meter that did not read
// them. The first answers from the beginning and the last to the end, so a
// meter that appears late or falls silent leaves those hours incomplete. A
// meter whose last reading came before the period is gone; one whose first
// came after it has not arrived.
func spans(assets []state.EnergyAsset, from, to int64) (map[string]span, [][]state.EnergyAsset) {
	byRole := map[role][]state.EnergyAsset{}
	for _, a := range assets {
		if r := roleOf(a.Kind); r != roleNone {
			byRole[r] = append(byRole[r], a)
		}
	}
	out := map[string]span{}
	var batteries [][]state.EnergyAsset
	for role, list := range byRole {
		sort.Slice(list, func(i, j int) bool { return list[i].FirstSeenMS < list[j].FirstSeenMS })
		var meters [][]state.EnergyAsset
		for _, a := range list {
			// Continue the meter that stopped last before this one began.
			best := -1
			for m, meter := range meters {
				last := meter[len(meter)-1].LastSeenMS
				if last <= a.FirstSeenMS+handoverMS && (best < 0 || last > meters[best][len(meters[best])-1].LastSeenMS) {
					best = m
				}
			}
			if best < 0 {
				meters = append(meters, []state.EnergyAsset{a})
			} else {
				meters[best] = append(meters[best], a)
			}
		}
		for _, meter := range meters {
			if meter[len(meter)-1].LastSeenMS < from || meter[0].FirstSeenMS >= to {
				continue
			}
			index := -1
			if role == roleBattery {
				index = len(batteries)
				batteries = append(batteries, meter)
			}
			for k, a := range meter {
				sp := span{math.MinInt64, math.MaxInt64, index}
				if k > 0 {
					sp.from = a.FirstSeenMS
				}
				if k+1 < len(meter) {
					sp.to = meter[k+1].FirstSeenMS
				}
				if sp.to > from && sp.from < to+hourMS {
					out[a.AssetID] = sp
				}
			}
		}
	}
	return out, batteries
}

// clamp keeps v within [0, hi].
func clamp(v, hi float64) float64 {
	return math.Max(0, math.Min(v, hi))
}

// loadAsset reads one asset month by month, so no single query scans a year.
func loadAsset(ctx context.Context, l Ledger, assetID string, start, end time.Time, each func(state.EnergyLedgerPoint)) error {
	for from := start; from.Before(end); {
		to := from.AddDate(0, 1, 0)
		if to.After(end) {
			to = end
		}
		buckets := int(to.Sub(from) / time.Hour)
		points, truncated, err := l.LoadEnergyHistoryContext(ctx, state.EnergyHistoryQuery{
			AssetID: assetID, SinceMS: from.UnixMilli(), UntilMS: to.UnixMilli(),
			BucketMS: hourMS, Limit: buckets + 1,
		})
		if err != nil {
			return err
		}
		if truncated {
			return fmt.Errorf("energy ledger for %s truncated", assetID)
		}
		for _, p := range points {
			each(p)
		}
		from = to
	}
	return nil
}
