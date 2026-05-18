package state

import "fmt"

// DayCostBreakdown decomposes grid traffic over a time range into actual cost
// (slot-weighted against the prices table) and the data needed to compute a
// flat-rate baseline (unweighted mean prices). All monetary values are in öre.
//
// The shape mirrors how mpc.ComputeBaselines computes "vs flat avg price" for
// the planner horizon: separate import / export means, applied to import /
// export volumes. Keeping the math identical means the historical answer for
// a finished day matches what the live badge said when that day was the plan.
type DayCostBreakdown struct {
	ImportWh         float64
	ExportWh         float64
	ImportCostOre    float64 // Σ slot ( import_wh × total_ore_kwh ) / 1000
	ExportRevenueOre float64 // Σ slot ( export_wh × export_price_ore ) / 1000
	AvgImportOreKwh  float64 // unweighted mean of total_ore_kwh over slots in range
	AvgExportOreKwh  float64 // unweighted mean of clamped export price over slots in range
}

// ActualCostOre is the net cost the household actually paid: import cost minus
// export revenue. Positive = paid out, negative = net earned.
func (b DayCostBreakdown) ActualCostOre() float64 {
	return b.ImportCostOre - b.ExportRevenueOre
}

// FlatCostOre is the no-timing baseline — same kWh volumes priced at the
// range's mean import / export prices. Matches mpc.ComputeBaselines'
// FlatAvgOre formula.
func (b DayCostBreakdown) FlatCostOre() float64 {
	return (b.ImportWh*b.AvgImportOreKwh - b.ExportWh*b.AvgExportOreKwh) / 1000.0
}

// SavedOre is FlatCostOre − ActualCostOre. Positive means the system spent
// less than flat-rate would have; negative means timing lost money relative
// to flat-rate.
func (b DayCostBreakdown) SavedOre() float64 {
	return b.FlatCostOre() - b.ActualCostOre()
}

// ExportPricing captures the runtime knobs that turn a slot's raw spot price
// into the öre/kWh the household actually earns when exporting. Mirrors the
// three relevant fields on mpc.Params so a single source of truth for the
// export model is used across plan-vs-actual and historical reporting.
type ExportPricing struct {
	BonusOreKwh float64 // added to spot before clamp
	FeeOreKwh   float64 // subtracted from spot before clamp
	FlatOreKwh  float64 // if > 0, overrides spot+bonus−fee entirely
}

// effectiveExportOre applies the same model as mpc.SlotExportPriceOre:
// flat override wins; otherwise spot+bonus−fee, clamped to ≥ 0.
func (ep ExportPricing) effectiveExportOre(spotOreKwh float64) float64 {
	if ep.FlatOreKwh > 0 {
		return ep.FlatOreKwh
	}
	if v := spotOreKwh + ep.BonusOreKwh - ep.FeeOreKwh; v > 0 {
		return v
	}
	return 0
}

// priceSlot is the in-memory shape of one price row used for cost-breakdown
// integration. EndMs is precomputed (slot_ts_ms + slot_len_min*60000) so the
// inner loop is a pure integer compare.
type priceSlot struct {
	StartMs, EndMs       int64
	SpotOreKwh, TotalOre float64
}

// maxSlotPadMs is the safety pad applied when filtering price_slots: a slot
// whose start is up to this far before sinceMs may still cover a sample
// inside the range. 1 day is comfortably above any real provider's slot
// length (NordPool 15 min, ENTSOE up to 60 min).
const maxSlotPadMs = int64(24 * 60 * 60 * 1000)

// DailyCostBreakdown integrates grid flow across [sinceMs, untilMs] using all
// three history tiers and prices it slot-by-slot against the prices table for
// zone. The price for each integration step is the slot whose [slot_ts_ms,
// slot_ts_ms+slot_len_min*60000) window contains the midpoint of (prev_ts,
// ts_ms] — using the midpoint instead of either edge keeps the result stable
// across slot boundaries (no off-by-one based on which side of the boundary a
// sample landed on).
//
// Two small SQL round-trips + a streaming history scan. The streaming scan
// pulls (ts, grid_w) rows in order and walks a pre-loaded slice of price
// slots with a sliding-pointer match — O(rows + slots) instead of
// O(rows × log prices) that an in-SQL per-row lookup against the (growing)
// prices table costs. The v0.76 release shipped the SQL form and the
// per-row lookups on a Pi-class device blocked the single-writer DB for
// long enough to stall the dispatch loop; this Go-side integration keeps
// the same per-step semantics but with predictable, range-proportional
// latency.
//
// Returns zeroes (not an error) when the history is empty over the range —
// callers can render that as "no data" without special-casing nil.
func (s *Store) DailyCostBreakdown(sinceMs, untilMs int64, zone string, ep ExportPricing) (DayCostBreakdown, error) {
	slots, err := s.loadPriceSlotsForRange(zone, sinceMs, untilMs)
	if err != nil {
		return DayCostBreakdown{}, fmt.Errorf("DailyCostBreakdown: load slots: %w", err)
	}

	out, err := s.integrateHistoryRange(sinceMs, untilMs, slots, ep)
	if err != nil {
		return DayCostBreakdown{}, fmt.Errorf("DailyCostBreakdown: integrate: %w", err)
	}

	avgImp, avgExp, err := s.avgSlotPricesForRange(zone, sinceMs, untilMs, ep)
	if err != nil {
		return DayCostBreakdown{}, fmt.Errorf("DailyCostBreakdown: avg slots: %w", err)
	}
	out.AvgImportOreKwh = avgImp
	out.AvgExportOreKwh = avgExp
	return out, nil
}

// loadPriceSlotsForRange returns slots that may cover any sample-midpoint in
// [sinceMs, untilMs], sorted ascending by StartMs. The pre-range pad is
// maxSlotPadMs (1 day) — generous against any real provider slot length so
// a slot that started just before sinceMs and extends into it is included.
func (s *Store) loadPriceSlotsForRange(zone string, sinceMs, untilMs int64) ([]priceSlot, error) {
	rows, err := s.db.Query(`
		SELECT slot_ts_ms, slot_len_min, spot_ore_kwh, total_ore_kwh
		FROM prices
		WHERE zone = ?
		  AND slot_ts_ms < ?
		  AND slot_ts_ms >= ?
		ORDER BY slot_ts_ms ASC
	`, zone, untilMs, sinceMs-maxSlotPadMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var slots []priceSlot
	for rows.Next() {
		var start, lenMin int64
		var spot, total float64
		if err := rows.Scan(&start, &lenMin, &spot, &total); err != nil {
			return nil, err
		}
		slots = append(slots, priceSlot{
			StartMs:    start,
			EndMs:      start + lenMin*60000,
			SpotOreKwh: spot,
			TotalOre:   total,
		})
	}
	return slots, rows.Err()
}

// integrateHistoryRange streams every history-tier sample in [sinceMs, untilMs]
// in ts order and accumulates dt-weighted Wh and öre. The "current row's
// grid_w applied over (prev_ts, ts_ms]" rule mirrors the SQL form that
// `DailyEnergy` uses; the first row of the stream has no predecessor and is
// silently dropped (it provides the prev_ts for the next iteration only).
//
// Slots are walked with a sliding pointer: both sides are ascending, so for
// each midpoint we advance until the slot's EndMs is past the midpoint.
// Samples whose midpoint has no covering slot still count toward ImportWh /
// ExportWh but contribute zero to cost / revenue.
func (s *Store) integrateHistoryRange(sinceMs, untilMs int64, slots []priceSlot, ep ExportPricing) (DayCostBreakdown, error) {
	rows, err := s.db.Query(`
		WITH all_rows AS (
			SELECT ts_ms, COALESCE(grid_w, 0) AS grid_w FROM history_hot  WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms, COALESCE(grid_w, 0)            FROM history_warm WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms, COALESCE(grid_w, 0)            FROM history_cold WHERE ts_ms BETWEEN ? AND ?
		)
		SELECT ts_ms, grid_w FROM all_rows ORDER BY ts_ms ASC
	`,
		sinceMs, untilMs,
		sinceMs, untilMs,
		sinceMs, untilMs,
	)
	if err != nil {
		return DayCostBreakdown{}, err
	}
	defer rows.Close()

	var (
		out      DayCostBreakdown
		havePrev bool
		prevTs   int64
		slotIdx  int
	)
	for rows.Next() {
		var ts int64
		var gridW float64
		if err := rows.Scan(&ts, &gridW); err != nil {
			return DayCostBreakdown{}, err
		}
		if !havePrev {
			prevTs = ts
			havePrev = true
			continue
		}

		dtMs := ts - prevTs
		midTs := (ts + prevTs) / 2
		prevTs = ts

		// Sliding pointer: advance past slots whose EndMs <= midTs.
		// Slots may be sparse (gaps allowed), so we accept "no covering
		// slot" as a valid state — those samples contribute Wh but no öre.
		for slotIdx < len(slots) && slots[slotIdx].EndMs <= midTs {
			slotIdx++
		}
		var covering *priceSlot
		if slotIdx < len(slots) && slots[slotIdx].StartMs <= midTs && slots[slotIdx].EndMs > midTs {
			covering = &slots[slotIdx]
		}

		wh := gridW * float64(dtMs) / 3600000.0
		switch {
		case gridW > 0:
			out.ImportWh += wh
			if covering != nil {
				out.ImportCostOre += wh * covering.TotalOre / 1000.0
			}
		case gridW < 0:
			out.ExportWh += -wh
			if covering != nil {
				out.ExportRevenueOre += -wh * ep.effectiveExportOre(covering.SpotOreKwh) / 1000.0
			}
		}
	}
	return out, rows.Err()
}

// avgSlotPricesForRange computes the unweighted mean import / export öre/kWh
// over slots whose StartMs falls in [sinceMs, untilMs]. Matches the original
// SQL: this is the flat-rate baseline mpc.ComputeBaselines uses for the
// "vs flat avg price" comparison.
func (s *Store) avgSlotPricesForRange(zone string, sinceMs, untilMs int64, ep ExportPricing) (avgImport, avgExport float64, err error) {
	rows, err := s.db.Query(`
		SELECT spot_ore_kwh, total_ore_kwh
		FROM prices
		WHERE zone = ? AND slot_ts_ms BETWEEN ? AND ?
	`, zone, sinceMs, untilMs)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	var (
		n           int
		sumImp, sumExp float64
	)
	for rows.Next() {
		var spot, total float64
		if err := rows.Scan(&spot, &total); err != nil {
			return 0, 0, err
		}
		n++
		sumImp += total
		sumExp += ep.effectiveExportOre(spot)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, nil
	}
	return sumImp / float64(n), sumExp / float64(n), nil
}
