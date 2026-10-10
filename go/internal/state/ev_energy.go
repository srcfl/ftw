package state

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// How EV charging cost is attributed.
//
// Charger energy is the persisted ledger flow vehicle_charge, one asset per
// charger. The price is prices.total_ore_kwh: (spot + grid tariff) × (1 + VAT),
// the same consumer import price the savings and daily-cost paths use.
//
// The ledger does not meter which source fed the car. When the same interval
// also records site import and household use, grid import is shared between
// home use and EV charging in proportion to their energy. Only the car's grid
// share is priced. Solar and home-battery energy in that interval has no
// further cash cost — any grid energy that filled the battery was priced when
// it crossed the meter, and this path does not trace that provenance.
// Opportunity cost of export the car displaced is not included.
//
// An interval with charger energy but no site import, export, solar or
// household use is priced entirely as grid import. Grid-attributed energy
// with no covering price slot still counts as energy; its cost is omitted
// and the window is marked partial.
const (
	EVAttributionSiteMix     = "site_mix"
	EVAttributionImportPrice = "import_price"
	EVAttributionMixed       = "mixed"

	EVAttributionNote = "Charging energy is metered per charger. In each interval, grid import is shared between home use and EV charging in proportion to their energy, and only that grid share is priced at the site's import price (spot plus grid tariff, including VAT). Energy covered by solar or the home battery adds no further charge. When the site meter is missing for an interval, all of that interval's charging is priced as grid import. Hours with no price still count as energy and leave the cost partial."
)

// EVChargeSample is one charger's energy in one ledger bucket.
type EVChargeSample struct {
	AssetID  string
	Label    string
	StartMs  int64
	LenMs    int64
	EnergyWh float64
}

// EVDay is one local calendar day, or the part of today already elapsed.
// StartMs and EndMs come from local midnights, so a DST day is 23 or 25 hours.
type EVDay struct {
	Day     string
	StartMs int64
	EndMs   int64
}

// EVChargerCost is energy and cash cost for one charger, one day, or a window.
type EVChargerCost struct {
	AssetID    string
	Label      string
	EnergyWh   float64
	GridWh     float64
	OnsiteWh   float64
	CostOre    float64
	UnpricedWh float64
	// SiteMixWh is energy attributed with a site import/load split.
	// ImportPriceWh is energy priced entirely as grid import because that
	// interval had no site flows to split.
	SiteMixWh     float64
	ImportPriceWh float64
}

// CostPartial is true when some grid-attributed energy had no price slot.
func (c EVChargerCost) CostPartial() bool {
	return c.UnpricedWh > 1e-6
}

// EVDayCost is one local day, including a zero day with no charging.
type EVDayCost struct {
	Day      string
	Total    EVChargerCost
	Chargers []EVChargerCost
}

// EVEnergyWindow is one rolling span of local days, oldest day first.
type EVEnergyWindow struct {
	Days        int
	SinceMs     int64
	UntilMs     int64
	Daily       []EVDayCost
	Chargers    []EVChargerCost
	Total       EVChargerCost
	Attribution string
}

// LocalDays returns n local days ending at now. Day boundaries are local
// midnights, not 24-hour unix steps, so a DST transition stays inside the
// civil day it belongs to. The last day ends at now.
func LocalDays(now time.Time, n int) []EVDay {
	if n < 1 {
		return nil
	}
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	out := make([]EVDay, n)
	for i := 0; i < n; i++ {
		start := today.AddDate(0, 0, -(n - 1 - i))
		end := start.AddDate(0, 0, 1)
		if i == n-1 {
			end = now
		}
		if !end.After(start) {
			end = start
		}
		out[i] = EVDay{Day: start.Format("2006-01-02"), StartMs: start.UnixMilli(), EndMs: end.UnixMilli()}
	}
	return out
}

type evAcc struct {
	EVChargerCost
}

func (a *evAcc) add(part EVChargerCost) {
	a.EnergyWh += part.EnergyWh
	a.GridWh += part.GridWh
	a.OnsiteWh += part.OnsiteWh
	a.CostOre += part.CostOre
	a.UnpricedWh += part.UnpricedWh
	a.SiteMixWh += part.SiteMixWh
	a.ImportPriceWh += part.ImportPriceWh
	if a.Label == "" {
		a.Label = part.Label
	}
}

// AggregateEVCharging prices charger samples into local days.
// A bucket belongs to the day that contains its start. Samples outside the
// days are ignored. Slots may overlap the bucket edges; energy is spread
// evenly across the bucket, matching savings.pricedWh.
func AggregateEVCharging(samples []EVChargeSample, site []LedgerFlowBucket, slots []CostSlot, days []EVDay) EVEnergyWindow {
	out := EVEnergyWindow{Days: len(days), Attribution: EVAttributionSiteMix}
	if len(days) == 0 {
		return out
	}
	out.SinceMs = days[0].StartMs
	out.UntilMs = days[len(days)-1].EndMs

	siteByBucket := make(map[evBucketKey]LedgerFlowBucket, len(site))
	for _, b := range site {
		siteByBucket[evBucketKey{b.StartMs, b.LenMs}] = b
	}

	type bucketSamples struct {
		start, length int64
		rows          []EVChargeSample
	}
	order := make([]evBucketKey, 0)
	grouped := make(map[evBucketKey]*bucketSamples)
	for _, sample := range samples {
		if sample.AssetID == "" || sample.LenMs <= 0 || sample.EnergyWh <= 0 || !plausibleEnergy(sample.EnergyWh, sample.LenMs) {
			continue
		}
		key := evBucketKey{sample.StartMs, sample.LenMs}
		g, ok := grouped[key]
		if !ok {
			g = &bucketSamples{start: sample.StartMs, length: sample.LenMs}
			grouped[key] = g
			order = append(order, key)
		}
		g.rows = append(g.rows, sample)
	}

	dayChargers := make([]map[string]*evAcc, len(days))
	dayTotals := make([]evAcc, len(days))
	chargerTotals := make(map[string]*evAcc)
	var total evAcc
	for i := range days {
		dayChargers[i] = make(map[string]*evAcc)
	}

	for _, key := range order {
		g := grouped[key]
		dayIdx := evDayIndex(days, g.start)
		if dayIdx < 0 {
			continue
		}
		evTotal := 0.0
		for _, row := range g.rows {
			evTotal += row.EnergyWh
		}
		if evTotal <= 0 {
			continue
		}
		gridFrac, siteMix := evGridFraction(siteByBucket[key], evTotal)
		_, pricedMs := bucketImportCost(g.start, g.length, 1, slots)
		pricedFrac := 1.0
		if pricedMs < g.length {
			pricedFrac = float64(pricedMs) / float64(g.length)
		}
		for _, row := range g.rows {
			part := EVChargerCost{
				AssetID:  row.AssetID,
				Label:    row.Label,
				EnergyWh: row.EnergyWh,
				GridWh:   row.EnergyWh * gridFrac,
				OnsiteWh: row.EnergyWh * (1 - gridFrac),
			}
			part.UnpricedWh = part.GridWh * (1 - pricedFrac)
			if siteMix {
				part.SiteMixWh = row.EnergyWh
			} else {
				part.ImportPriceWh = row.EnergyWh
			}
			ore, _ := bucketImportCost(g.start, g.length, part.GridWh, slots)
			part.CostOre = ore
			dayChargers[dayIdx][row.AssetID] = accAdd(dayChargers[dayIdx][row.AssetID], part)
			dayTotals[dayIdx].add(part)
			chargerTotals[row.AssetID] = accAdd(chargerTotals[row.AssetID], part)
			total.add(part)
		}
	}

	out.Daily = make([]EVDayCost, len(days))
	for i, day := range days {
		out.Daily[i] = EVDayCost{
			Day:      day.Day,
			Total:    dayTotals[i].EVChargerCost,
			Chargers: chargerList(dayChargers[i]),
		}
	}
	out.Chargers = chargerList(chargerTotals)
	out.Total = total.EVChargerCost
	out.Attribution = evAttribution(out.Total)
	return out
}

func accAdd(dst *evAcc, part EVChargerCost) *evAcc {
	if dst == nil {
		dst = &evAcc{EVChargerCost: EVChargerCost{AssetID: part.AssetID, Label: part.Label}}
	}
	dst.add(part)
	return dst
}

func chargerList(m map[string]*evAcc) []EVChargerCost {
	if len(m) == 0 {
		return nil
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]EVChargerCost, len(ids))
	for i, id := range ids {
		out[i] = m[id].EVChargerCost
		out[i].AssetID = id
	}
	return out
}

func evAttribution(total EVChargerCost) string {
	switch {
	case total.SiteMixWh > 0 && total.ImportPriceWh > 0:
		return EVAttributionMixed
	case total.ImportPriceWh > 0:
		return EVAttributionImportPrice
	default:
		return EVAttributionSiteMix
	}
}

func evDayIndex(days []EVDay, start int64) int {
	for i := range days {
		if start >= days[i].StartMs && start < days[i].EndMs {
			return i
		}
	}
	return -1
}

type evBucketKey struct {
	start  int64
	length int64
}

// evGridFraction returns the share of EV energy treated as grid import, and
// whether a site split was available. Missing site flows price the whole
// interval as import.
func evGridFraction(site LedgerFlowBucket, evWh float64) (frac float64, siteMix bool) {
	if evWh <= 0 {
		return 0, false
	}
	if !evSiteObserved(site) {
		return 1, false
	}
	demand := site.LoadWh + evWh
	if demand <= 0 {
		return 0, true
	}
	frac = site.ImportWh / demand
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return frac, true
}

func evSiteObserved(site LedgerFlowBucket) bool {
	return site.LenMs > 0 && (site.ImportWh > 0 || site.ExportWh > 0 || site.PVWh > 0 || site.LoadWh > 0 || site.EVDischargeWh > 0)
}

// bucketImportCost spreads gridWh evenly across the price slots the bucket
// overlaps. The ore math matches savings.pricedWh for an import.
func bucketImportCost(start, length int64, gridWh float64, slots []CostSlot) (ore float64, pricedMs int64) {
	if length <= 0 {
		return 0, 0
	}
	end := start + length
	for _, sl := range slots {
		if sl.EndMs <= start || sl.StartMs >= end {
			continue
		}
		a := start
		if sl.StartMs > a {
			a = sl.StartMs
		}
		b := end
		if sl.EndMs < b {
			b = sl.EndMs
		}
		overlap := b - a
		if overlap <= 0 {
			continue
		}
		pricedMs += overlap
		if gridWh != 0 {
			part := gridWh * float64(overlap) / float64(length)
			ore += part * sl.ImportOreKwh / 1000
		}
	}
	if pricedMs > length {
		pricedMs = length
	}
	return ore, pricedMs
}

// LoadEVChargeSamples reads persisted vehicle-charge ledger rows whose bucket
// starts in [since, until). The ledger survives restarts; this is a read.
func (s *Store) LoadEVChargeSamples(ctx context.Context, since, until int64) ([]EVChargeSample, error) {
	if s == nil || s.history == nil || until <= since {
		return nil, nil
	}
	rows, err := s.history.QueryContext(ctx, `
		SELECT e.asset_id, COALESCE(a.label, ''), e.bucket_start_ms, e.bucket_len_ms, SUM(e.energy_wh)
		FROM energy_ledger_entries e
		LEFT JOIN energy_assets a ON a.asset_id = e.asset_id
		WHERE e.schema_version = ?
		  AND e.flow = ?
		  AND e.bucket_start_ms >= ? AND e.bucket_start_ms < ?
		  AND e.energy_wh > 0
		  AND e.provenance != 'implausible_energy'
		GROUP BY e.asset_id, e.bucket_start_ms, e.bucket_len_ms
		ORDER BY e.bucket_start_ms, e.asset_id`,
		EnergyLedgerSchemaVersion, FlowVehicleCharge, since, until)
	if err != nil {
		return nil, fmt.Errorf("load EV charge samples: %w", err)
	}
	defer rows.Close()
	var out []EVChargeSample
	for rows.Next() {
		var sample EVChargeSample
		if err := rows.Scan(&sample.AssetID, &sample.Label, &sample.StartMs, &sample.LenMs, &sample.EnergyWh); err != nil {
			return nil, err
		}
		if !plausibleEnergy(sample.EnergyWh, sample.LenMs) {
			continue
		}
		out = append(out, sample)
	}
	return out, rows.Err()
}
