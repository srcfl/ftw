package savings

import "github.com/srcfl/ftw/go/internal/gridcost"

// FlowBucket is one ledger interval. Energies are watt-hours, never negative.
// LoadWh is household use and does not include vehicle charging.
// AvailablePVWh is panel output before curtailment. Zero means PVWh is all
// that was recorded. The blind battery uses the higher figure only while
// exporting pays nothing or costs money.
type FlowBucket struct {
	StartMs       int64
	LenMs         int64
	ImportWh      float64
	ExportWh      float64
	PVWh          float64
	AvailablePVWh float64
	LoadWh        float64
	EVChargeWh    float64
	EVDischargeWh float64
}

// PriceSlot is one tariff window. ImportOreKwh is the full buy price.
// SpotOreKwh is the spot used to derive the export price.
type PriceSlot struct {
	StartMs      int64
	EndMs        int64
	ImportOreKwh float64
	SpotOreKwh   float64
}

// Battery is the household battery a blind self-consumption counterfactual
// may use. Zero capacity means solar is used immediately and the rest is
// exchanged with the grid. The charge starts at StartWh, clamped into
// [ReserveWh, CapacityWh], and carries across the whole window.
// ChargeEfficiency and DischargeEfficiency are fractions in (0, 1].
// Zero means no loss.
type Battery struct {
	CapacityWh          float64
	MaxChargeW          float64
	MaxDischargeW       float64
	StartWh             float64
	ReserveWh           float64
	ChargeEfficiency    float64
	DischargeEfficiency float64
}

// DayWindow is one local day, or the part of today that has already elapsed.
type DayWindow struct {
	StartMs int64
	EndMs   int64
}

// LedgerDay is the money and energy for one window.
type LedgerDay struct {
	ImportWh         float64
	ExportWh         float64
	LoadWh           float64
	EVWh             float64
	PVWh             float64
	ImportCostOre    float64
	ExportRevenueOre float64
	NoPVCostOre      float64
	SelfCostOre      float64
	SelfImportWh     float64
	SelfExportWh     float64
	SelfEndWh        float64
	CoveredMs        int64
	PricedMs         int64
}

// NoPVSavedOre is the cost of buying the recorded use from the grid, minus
// what the site actually paid.
func (d LedgerDay) NoPVSavedOre() float64 {
	return d.NoPVCostOre - (d.ImportCostOre - d.ExportRevenueOre)
}

// SelfSavedOre is the cost of a self-consumption battery, minus what the
// site actually paid. Positive means price-aware use beat that policy.
func (d LedgerDay) SelfSavedOre() float64 {
	return d.SelfCostOre - (d.ImportCostOre - d.ExportRevenueOre)
}

// EvaluateLedger prices the ledger and simulates self-consumption in time
// order. Buckets and slots must be sorted by start. A bucket's energy is
// spread evenly across the price slots it overlaps.
func EvaluateLedger(buckets []FlowBucket, slots []PriceSlot, days []DayWindow, bat Battery, ep gridcost.ExportPricing) []LedgerDay {
	out := make([]LedgerDay, len(days))
	charge := bat.OpeningWh()
	di := 0
	closed := 0
	closeThrough := func(until int) {
		for closed < until && closed < len(out) {
			out[closed].SelfEndWh = charge
			closed++
		}
	}
	for _, b := range buckets {
		for di+1 < len(days) && b.StartMs >= days[di].EndMs {
			di++
		}
		if di >= len(days) || b.StartMs < days[di].StartMs || b.StartMs >= days[di].EndMs {
			continue
		}
		closeThrough(di)
		addLedgerBucket(&out[di], b, slots, &charge, bat, ep)
	}
	closeThrough(len(out))
	return out
}

// OpeningWh is the stored energy the blind battery starts with.
func (b Battery) OpeningWh() float64 {
	if b.CapacityWh <= 0 {
		return 0
	}
	charge := b.StartWh
	if charge < 0 {
		charge = 0
	}
	if charge > b.CapacityWh {
		charge = b.CapacityWh
	}
	if floor := b.reserveWh(); charge < floor {
		charge = floor
	}
	return charge
}

func addLedgerBucket(day *LedgerDay, b FlowBucket, slots []PriceSlot, charge *float64, bat Battery, ep gridcost.ExportPricing) {
	if b.LenMs <= 0 {
		return
	}
	day.ImportWh += b.ImportWh
	day.ExportWh += b.ExportWh
	day.LoadWh += b.LoadWh
	day.EVWh += b.EVChargeWh
	day.PVWh += b.PVWh
	day.CoveredMs += b.LenMs

	impOre, priced := pricedWh(b.StartMs, b.LenMs, b.ImportWh, slots, false, ep)
	expOre, _ := pricedWh(b.StartMs, b.LenMs, b.ExportWh, slots, true, ep)
	day.ImportCostOre += impOre
	day.ExportRevenueOre += expOre
	day.PricedMs += priced

	demand := b.LoadWh + b.EVChargeWh - b.EVDischargeWh
	if demand < 0 {
		demand = 0
	}
	day.NoPVCostOre += pricedWhMust(b.StartMs, b.LenMs, demand, slots, false, ep)

	dumb := b
	dumb.PVWh = dumbPVWh(b, slots, ep)
	selfImp, selfExp := selfConsumption(dumb, charge, bat)
	day.SelfImportWh += selfImp
	day.SelfExportWh += selfExp
	day.SelfCostOre += pricedWhMust(b.StartMs, b.LenMs, selfImp, slots, false, ep)
	day.SelfCostOre -= pricedWhMust(b.StartMs, b.LenMs, selfExp, slots, true, ep)
}

// dumbPVWh is what a controller that cannot curtail would have produced.
// Extra panel output counts only when exporting it earns nothing.
func dumbPVWh(b FlowBucket, slots []PriceSlot, ep gridcost.ExportPricing) float64 {
	if b.AvailablePVWh <= b.PVWh {
		return b.PVWh
	}
	ore, ok := bucketExportOre(b.StartMs, b.LenMs, slots, ep)
	if !ok || ore > 0 {
		return b.PVWh
	}
	return b.AvailablePVWh
}

func bucketExportOre(start, length int64, slots []PriceSlot, ep gridcost.ExportPricing) (float64, bool) {
	if length <= 0 {
		return 0, false
	}
	end := start + length
	var weight, sum float64
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
		w := float64(b - a)
		weight += w
		sum += w * gridcost.ExportPriceOre(sl.SpotOreKwh, ep)
	}
	if weight <= 0 {
		return 0, false
	}
	return sum / weight, true
}

func selfConsumption(b FlowBucket, charge *float64, bat Battery) (importWh, exportWh float64) {
	demand := b.LoadWh + b.EVChargeWh - b.EVDischargeWh
	if demand < 0 {
		demand = 0
	}
	hours := float64(b.LenMs) / 3_600_000
	surplus := b.PVWh - demand
	if surplus >= 0 {
		take := surplus
		if bat.CapacityWh <= 0 {
			take = 0
		} else {
			room := bat.CapacityWh - *charge
			if room < 0 {
				room = 0
			}
			maxTake := room / bat.chargeEff()
			if maxTake < take {
				take = maxTake
			}
			limit := bat.MaxChargeW * hours
			if limit < take {
				take = limit
			}
		}
		*charge += take * bat.chargeEff()
		if bat.CapacityWh > 0 && *charge > bat.CapacityWh {
			*charge = bat.CapacityWh
		}
		return 0, surplus - take
	}
	need := -surplus
	give := need
	if bat.CapacityWh <= 0 {
		give = 0
	} else {
		avail := *charge - bat.reserveWh()
		if avail < 0 {
			avail = 0
		}
		maxGive := avail * bat.dischargeEff()
		if maxGive < give {
			give = maxGive
		}
		limit := bat.MaxDischargeW * hours
		if limit < give {
			give = limit
		}
	}
	*charge -= give / bat.dischargeEff()
	if floor := bat.reserveWh(); *charge < floor {
		*charge = floor
	}
	return need - give, 0
}

func (b Battery) chargeEff() float64 {
	if b.ChargeEfficiency <= 0 || b.ChargeEfficiency > 1 {
		return 1
	}
	return b.ChargeEfficiency
}

func (b Battery) dischargeEff() float64 {
	if b.DischargeEfficiency <= 0 || b.DischargeEfficiency > 1 {
		return 1
	}
	return b.DischargeEfficiency
}

func (b Battery) reserveWh() float64 {
	if b.ReserveWh <= 0 || b.CapacityWh <= 0 {
		return 0
	}
	if b.ReserveWh > b.CapacityWh {
		return b.CapacityWh
	}
	return b.ReserveWh
}

func pricedWh(start, length int64, wh float64, slots []PriceSlot, export bool, ep gridcost.ExportPricing) (ore float64, pricedMs int64) {
	if length <= 0 || wh == 0 {
		if length > 0 {
			pricedMs = overlapMs(start, start+length, slots)
		}
		return 0, pricedMs
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
		part := wh * float64(b-a) / float64(length)
		if export {
			ore += part * gridcost.ExportPriceOre(sl.SpotOreKwh, ep) / 1000
		} else {
			ore += part * sl.ImportOreKwh / 1000
		}
		pricedMs += b - a
	}
	return ore, pricedMs
}

func pricedWhMust(start, length int64, wh float64, slots []PriceSlot, export bool, ep gridcost.ExportPricing) float64 {
	ore, _ := pricedWh(start, length, wh, slots, export, ep)
	return ore
}

func overlapMs(start, end int64, slots []PriceSlot) int64 {
	var n int64
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
		n += b - a
	}
	return n
}
