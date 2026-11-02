// Package batteryreturn works out the figure a Swedish household reports to
// Skatteverket to get back the energy tax on electricity it bought from the
// grid, stored in a battery and fed back to the same grid (11 kap. 13 § lagen
// (1994:1776) om skatt på energi). The owner applies once per calendar year
// and must be able to show how the figure was reached, so the report keeps
// every hour.
//
// The ledger says how much each meter counted, not where each kWh went. The
// report therefore counts low wherever it cannot know:
//
//   - In each hour the battery took from the grid only what solar and a
//     discharging car could not have supplied, and gave to the grid only
//     what they could not have exported. Every source on the site must be
//     one FTW reads.
//   - Nothing says how much the battery holds, so no share of its content
//     can be known. What the house takes from the battery counts as grid
//     energy first, and what goes to the grid counts as other energy first.
//     Grid energy counts after the least round-trip efficiency its run of
//     readings allows, so losses are never refunded; the loss stays in the
//     account as other energy. Within an hour, the order credits least.
//   - An hour counts only when every meter that reported during the year
//     read all of it. An hour that does not count adds nothing, and what the
//     battery took in it counts as energy from elsewhere. Without the
//     battery's own readings the battery is taken to be full of energy from
//     elsewhere after the hour. The year starts the same way, so the
//     configured battery size is required.
//   - One battery. With two, one account would let one battery's exports
//     carry the other's grid energy.
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
	// CapacityWh is the configured size of the batteries together, and
	// Batteries how many of them have a size. The account needs an upper bound
	// on what the battery holds.
	CapacityWh float64
	Batteries  int
	// Lat and Lon place the site when Placed. Then a solar inverter that
	// sleeps after dusk reads 0 instead of leaving the night incomplete.
	Lat, Lon float64
	Placed   bool
}

// Hour is one clock hour of the basis. Energies are Wh.
type Hour struct {
	Start            time.Time
	GridImport       float64
	GridExport       float64
	BatteryCharge    float64
	BatteryDischarge float64
	Solar            float64
	VehicleDischarge float64
	// GridToBattery is the least grid energy the battery can have taken.
	GridToBattery float64
	// BatteryToGrid is the least battery energy that can have gone out to
	// the grid.
	BatteryToGrid float64
	// Returned is the part of BatteryToGrid that was grid energy stored
	// earlier: the refundable energy.
	Returned float64
	// GridHeld and OtherHeld are what the battery holds after the hour.
	GridHeld  float64
	OtherHeld float64
	// Efficiency is the least round-trip efficiency the readings of this
	// hour's run allow.
	Efficiency float64
	Complete   bool
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
	// NoSize: the battery has no configured size.
	NoSize Problem = "battery_size"
	// NotOneBattery: the ledger read more than one battery in the year, or
	// a different number than have a size.
	NotOneBattery Problem = "batteries"
)

// Report is one calendar year, or the part of it that has passed.
type Report struct {
	Year     int
	Returned float64
	Hours    []Hour
	Months   [12]Month
	// Efficiency is the hours' efficiencies weighted by charge.
	Efficiency float64
	// CapacityWh caps what the battery holds: the configured size, or the most it
	// gained in a day if that is more.
	CapacityWh float64
	// Batteries is how many batteries the ledger read in the year.
	Batteries int
	// Problem is set when nothing is returned for a reason other than the
	// readings.
	Problem Problem
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
	r := Report{Year: year, Efficiency: 1, CapacityWh: site.CapacityWh}
	switch {
	case site.Batteries == 0:
		r.Problem = NoSize
	case site.Batteries > 1:
		r.Problem = NotOneBattery
	}
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
	owed, batteries := spans(assets, from, to)
	r.Batteries = batteries
	metered := false
	unread := make([]bool, n) // a battery's readings are missing
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
			case state.FlowBatteryDischarge:
				h.BatteryDischarge += p.EnergyWh
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
			unread[i], r.Hours[i].Complete = true, false
		}
		// A meter that reported during the year must read every hour it
		// answers for. Silence, before or after, is not a quiet hour.
		for i := range r.Hours {
			if owes(i) && (!ok(i) || owes(i+1) && !ok(i+1)) {
				r.Hours[i].Complete = false
				unread[i] = unread[i] || role == roleBattery
			}
		}
	}
	switch {
	case r.Batteries == 0:
		r.Problem = "" // nothing stored, nothing to size
	case r.Batteries != 1 || site.Batteries > 1:
		r.Problem = NotOneBattery
	}

	var in, out float64
	for i := range r.Hours {
		h := &r.Hours[i]
		if !unread[i] {
			in += h.BatteryCharge
			out += h.BatteryDischarge
		}
		// Without a grid meter nothing says what crossed the connection.
		h.Complete = h.Complete && metered
		if !h.Complete {
			continue
		}
		h.GridToBattery = clamp(h.BatteryCharge-h.Solar-h.VehicleDischarge, h.GridImport)
		h.BatteryToGrid = clamp(h.GridExport-h.Solar-h.VehicleDischarge, h.BatteryDischarge)
	}
	if in > 0 {
		// A battery that holds what it gained in a day is at least that
		// large, whatever the configuration says.
		eff := min(out/in, 1)
		var held, low, high float64
		for i, h := range r.Hours {
			if unread[i] || i > 0 && h.Start.In(loc).YearDay() != r.Hours[i-1].Start.In(loc).YearDay() {
				held, low, high = 0, 0, 0
			}
			if unread[i] {
				continue
			}
			held += eff*h.BatteryCharge - h.BatteryDischarge
			low, high = min(low, held), max(high, held)
			r.CapacityWh = max(r.CapacityWh, high-low)
		}
	}

	k := r.CapacityWh

	// Each run of hours with the battery read gets the least efficiency its
	// own readings allow: out = efficiency × in − what the battery gained,
	// and it gained at most a full battery. A short run allows little, and
	// a run never borrows readings from across a gap.
	var used, charged float64
	for a := 0; a < n; {
		if unread[a] {
			a++
			continue
		}
		b, in, out := a, 0.0, 0.0
		for ; b < n && !unread[b]; b++ {
			in += r.Hours[b].BatteryCharge
			out += r.Hours[b].BatteryDischarge
		}
		eff := 0.0
		if in > 0 {
			eff = clamp((out-k)/in, 1)
		}
		for i := a; i < b; i++ {
			r.Hours[i].Efficiency = eff
		}
		used += eff * in
		charged += in
		a = b
	}
	if charged > 0 {
		r.Efficiency = used / charged
	}

	grid, other := 0.0, k // unknown content at the start
	for i := range r.Hours {
		h := &r.Hours[i]
		m := &r.Months[h.Start.In(loc).Month()-1]
		m.Hours++
		if unread[i] || r.Problem != "" {
			grid, other = 0, k
			h.GridHeld, h.OtherHeld = grid, other
			continue
		}
		// An incomplete hour has no bounds: all its discharge counts as
		// used at home and all its charge as energy from elsewhere.
		if h.Complete {
			m.CompleteHours++
		}
		// The hour plays in the order that credits least: other energy
		// arrives, the export leaves and takes other energy first, grid
		// energy arrives, the house takes its share and takes grid energy
		// first. Grid energy counts after the least efficiency; its loss
		// stays in the battery as other energy, so the account never falls
		// below what the battery holds.
		other += h.BatteryCharge - h.GridToBattery
		fromOther := min(other, h.BatteryToGrid)
		other -= fromOther
		h.Returned = min(grid, h.BatteryToGrid-fromOther)
		grid -= h.Returned
		grid += h.Efficiency * h.GridToBattery
		other += (1 - h.Efficiency) * h.GridToBattery
		home := h.BatteryDischarge - h.BatteryToGrid
		fromGrid := min(grid, home)
		grid -= fromGrid
		other = max(0, other-(home-fromGrid))
		// What does not fit a full battery leaves as grid energy first.
		if over := grid + other - k; over > 0 {
			cut := min(grid, over)
			grid -= cut
			other -= over - cut
		}
		h.GridHeld, h.OtherHeld = grid, other
		m.Returned += h.Returned
		r.Returned += h.Returned
	}
	return r, nil
}

// span is the time [from, to) an asset answers for, in Unix ms.
type span struct{ from, to int64 }

// handoverMS allows for the old and new identity reading in the same tick
// in either order.
const handoverMS = int64(2 * time.Minute / time.Millisecond)

// spans gives each asset that matters for [from, to) the time it answers
// for, and counts the batteries.
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
func spans(assets []state.EnergyAsset, from, to int64) (map[string]span, int) {
	byRole := map[role][]state.EnergyAsset{}
	for _, a := range assets {
		if r := roleOf(a.Kind); r != roleNone {
			byRole[r] = append(byRole[r], a)
		}
	}
	out := map[string]span{}
	batteries := 0
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
			if role == roleBattery {
				batteries++
			}
			for k, a := range meter {
				sp := span{math.MinInt64, math.MaxInt64}
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
