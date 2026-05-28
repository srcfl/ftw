package state

import (
	"fmt"

	"github.com/frahlg/forty-two-watts/go/internal/gridcost"
)

// DayCostBreakdown decomposes a historical range into actual net grid cost and
// a no-PV/no-battery baseline. All monetary values are in öre.
//
// BaselineCostOre prices the household's total consumption — split between
// inflexible house load (priced slot-by-slot at the spot total) and flexible
// EV charging (priced at the day's time-weighted average import) — as if
// every Wh had been bought from the grid. The EV-as-average treatment exists
// because the EMS schedules EV charging into low-price slots: pricing those
// kWh at the slot they actually landed in would zero out the credit owed to
// the scheduler. Crediting them at the day's average price says "a dumb
// charger with no timing awareness would have paid this much" and lets the
// shifted timing show up as savings.
//
// ActualCostOre prices the real grid-boundary import/export. Their
// difference is the operator-facing savings from PV self-consumption,
// battery shifting, export revenue, and EV scheduling.
type DayCostBreakdown struct {
	ImportWh         float64
	ExportWh         float64
	LoadWh           float64 // house load (excludes EV) — kept stable for live chart
	EVWh             float64 // EV charging energy
	ImportCostOre    float64 // Σ slot ( import_wh × total_ore_kwh ) / 1000
	ExportRevenueOre float64 // Σ slot ( export_wh × export_price_ore ) / 1000
	BaselineHouseOre float64 // Σ slot ( house_load_wh × total_ore_kwh ) / 1000
	BaselineEvOre    float64 // ev_wh × AvgImportOreKwh / 1000
	BaselineCostOre  float64 // BaselineHouseOre + BaselineEvOre
	AvgImportOreKwh  float64 // time-weighted mean of total_ore_kwh over slots in range
	AvgExportOreKwh  float64 // time-weighted mean of effective export price over slots in range
	PriceSlotCount   int     // overlapping price slots used for the cost model
}

// ActualCostOre is the net cost the household actually paid: import cost minus
// export revenue. Positive = paid out, negative = net earned.
func (b DayCostBreakdown) ActualCostOre() float64 {
	return b.ImportCostOre - b.ExportRevenueOre
}

// FlatCostOre returns the legacy field name's value. The savings endpoint used
// to expose a flat-average timing baseline; callers now get the load-only
// no-PV/no-battery baseline through the same field for compatibility.
func (b DayCostBreakdown) FlatCostOre() float64 {
	return b.BaselineCostOre
}

// SavedOre is baseline load cost minus actual net cost. Positive means PV,
// battery dispatch, and export reduced cost relative to buying the recorded
// load from the grid.
func (b DayCostBreakdown) SavedOre() float64 {
	return b.BaselineCostOre - b.ActualCostOre()
}

// ExportPricing aliases the shared export pricing knobs used by the planner
// and historical cost reporting.
type ExportPricing = gridcost.ExportPricing

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

	avgImp, avgExp, priceSlots, err := s.avgSlotPricesForRange(zone, sinceMs, untilMs, ep)
	if err != nil {
		return DayCostBreakdown{}, fmt.Errorf("DailyCostBreakdown: avg slots: %w", err)
	}
	out.AvgImportOreKwh = avgImp
	out.AvgExportOreKwh = avgExp
	out.PriceSlotCount = priceSlots
	// Price EV energy at the day's time-weighted average import. If the
	// range has no overlapping price slots, AvgImportOreKwh is 0 and the
	// EV term collapses cleanly to 0 — matching the "no prices" rendering.
	out.BaselineEvOre = out.EVWh * avgImp / 1000.0
	out.BaselineCostOre = out.BaselineHouseOre + out.BaselineEvOre
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
// Samples whose midpoint has no covering slot still count toward ImportWh,
// ExportWh, LoadWh, and EVWh but contribute zero to cost / revenue.
//
// EV power is derived per row as `grid_w − bat_w − pv_w − load_w` (clamped
// non-negative — the same identity main.go uses in reverse to compute
// `load_w` for the history rows). Pricing of EVWh is deferred to the
// caller (DailyCostBreakdown applies the day's avg import).
func (s *Store) integrateHistoryRange(sinceMs, untilMs int64, slots []priceSlot, ep ExportPricing) (DayCostBreakdown, error) {
	rows, err := s.db.Query(`
		WITH all_rows AS (
			SELECT ts_ms,
			       COALESCE(grid_w, 0) AS grid_w,
			       COALESCE(load_w, 0) AS load_w,
			       COALESCE(bat_w,  0) AS bat_w,
			       COALESCE(pv_w,   0) AS pv_w
			FROM history_hot  WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms,
			       COALESCE(grid_w, 0),
			       COALESCE(load_w, 0),
			       COALESCE(bat_w,  0),
			       COALESCE(pv_w,   0)
			FROM history_warm WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms,
			       COALESCE(grid_w, 0),
			       COALESCE(load_w, 0),
			       COALESCE(bat_w,  0),
			       COALESCE(pv_w,   0)
			FROM history_cold WHERE ts_ms BETWEEN ? AND ?
		)
		SELECT ts_ms, grid_w, load_w, bat_w, pv_w FROM all_rows ORDER BY ts_ms ASC
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
		var gridW, loadW, batW, pvW float64
		if err := rows.Scan(&ts, &gridW, &loadW, &batW, &pvW); err != nil {
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

		if loadW > 0 {
			loadWh := loadW * float64(dtMs) / 3600000.0
			out.LoadWh += loadWh
			if covering != nil {
				out.BaselineHouseOre += loadWh * covering.TotalOre / 1000.0
			}
		}

		// EV = grid_w − bat_w − pv_w − load_w. Clamp negative noise to
		// zero (matches main.go's clamp on load_w going the other way).
		evW := gridW - batW - pvW - loadW
		if evW > 0 {
			out.EVWh += evW * float64(dtMs) / 3600000.0
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
				out.ExportRevenueOre += -wh * gridcost.ExportPriceOre(covering.SpotOreKwh, ep) / 1000.0
			}
		}
	}
	return out, rows.Err()
}

// avgSlotPricesForRange computes time-weighted import / export price metadata
// over price slots overlapping [sinceMs, untilMs), including variable slot
// lengths and partial edge slots.
func (s *Store) avgSlotPricesForRange(zone string, sinceMs, untilMs int64, ep ExportPricing) (avgImport, avgExport float64, count int, err error) {
	rows, err := s.db.Query(`
		SELECT slot_ts_ms, slot_len_min, spot_ore_kwh, total_ore_kwh
		FROM prices
		WHERE zone = ?
		  AND slot_ts_ms < ?
		  AND slot_ts_ms + slot_len_min * 60000 > ?
		ORDER BY slot_ts_ms ASC
	`, zone, untilMs, sinceMs)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()

	var (
		weightMs       float64
		sumImp, sumExp float64
	)
	for rows.Next() {
		var start int64
		var lenMin int
		var spot, total float64
		if err := rows.Scan(&start, &lenMin, &spot, &total); err != nil {
			return 0, 0, 0, err
		}
		if lenMin <= 0 {
			lenMin = 60
		}
		end := start + int64(lenMin)*60000
		overlapStart := maxInt64(start, sinceMs)
		overlapEnd := minInt64(end, untilMs)
		if overlapEnd <= overlapStart {
			continue
		}
		w := float64(overlapEnd - overlapStart)
		count++
		weightMs += w
		sumImp += total * w
		sumExp += gridcost.ExportPriceOre(spot, ep) * w
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, err
	}
	if weightMs == 0 {
		return 0, 0, 0, nil
	}
	return sumImp / weightMs, sumExp / weightMs, count, nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
