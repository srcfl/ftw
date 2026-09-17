package state

import (
	"context"
	"github.com/srcfl/ftw/go/internal/gridcost"
)

func addDayCost(a, b DayCostBreakdown) DayCostBreakdown {
	a.ImportWh += b.ImportWh
	a.ExportWh += b.ExportWh
	a.LoadWh += b.LoadWh
	a.EVWh += b.EVWh
	a.ImportCostOre += b.ImportCostOre
	a.ExportRevenueOre += b.ExportRevenueOre
	a.BaselineHouseOre += b.BaselineHouseOre
	a.HistoryCoveredMs += b.HistoryCoveredMs
	a.PricedCoveredMs += b.PricedCoveredMs
	return a
}

func (s *Store) integrateHistoryRange(ctx context.Context, since, until int64, slots []priceSlot, ep ExportPricing) (DayCostBreakdown, error) {
	from, ok, err := s.siteEnergyFrom(ctx)
	if err != nil {
		return DayCostBreakdown{}, err
	}
	if !ok || until <= from {
		return s.integrateLegacyHistoryRange(ctx, since, until, slots, ep)
	}
	out := DayCostBreakdown{ExpectedMs: until - since}
	if since < from {
		old, err := s.integrateLegacyHistoryRange(ctx, since, from, slots, ep)
		if err != nil {
			return out, err
		}
		out = addDayCost(out, old)
	}
	idx := 0
	err = s.walkSiteEnergy(ctx, max(since, from), until, func(r siteEnergyRow) error {
		out.ImportWh += r.imp
		out.ExportWh += r.exp
		out.LoadWh += r.load
		out.HistoryCoveredMs += r.covered
		for idx < len(slots) && slots[idx].EndMs <= r.first {
			idx++
		}
		// All observed energy must fit one price window. Do not manufacture a
		// priced fraction when a custom tariff cuts through retained evidence.
		if idx < len(slots) && slots[idx].StartMs <= r.first && slots[idx].EndMs >= r.last {
			slot := slots[idx]
			out.PricedCoveredMs += r.covered
			out.ImportCostOre += r.imp * slot.TotalOre / 1000
			out.ExportRevenueOre += r.exp * gridcost.ExportPriceOre(slot.SpotOreKwh, ep) / 1000
			out.BaselineHouseOre += r.load * slot.TotalOre / 1000
			out.EVWh += r.ev
		}
		return nil
	})
	return out, err
}

func (s *Store) ImportWhIntervals(ctx context.Context, intervals [][2]int64) ([]float64, []int64, error) {
	from, ok, err := s.siteEnergyFrom(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !ok || len(intervals) == 0 {
		return s.legacyImportWhIntervals(ctx, intervals)
	}
	wh := make([]float64, len(intervals))
	covered := make([]int64, len(intervals))
	// One legacy scan, with the transition clamped in each ordered interval.
	var old [][2]int64
	for _, v := range intervals {
		if v[0] >= from {
			break
		}
		old = append(old, [2]int64{v[0], min(v[1], from)})
	}
	if len(old) > 0 {
		a, b, err := s.legacyImportWhIntervals(ctx, old)
		if err != nil {
			return nil, nil, err
		}
		copy(wh, a)
		copy(covered, b)
	}
	idx := 0
	err = s.walkSiteEnergy(ctx, max(from, intervals[0][0]), intervals[len(intervals)-1][1], func(r siteEnergyRow) error {
		for idx < len(intervals) && intervals[idx][1] <= r.first {
			idx++
		}
		if idx < len(intervals) && intervals[idx][0] <= r.first && intervals[idx][1] >= r.last {
			wh[idx] += r.imp
			covered[idx] += r.covered
		}
		return nil
	})
	return wh, covered, err
}
