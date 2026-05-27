// Package savings reconstructs how much money the EMS optimizer saved on a
// given historical day, by re-pricing the measured grid flow with the same
// cost model the live MPC planner uses (mpc.SlotGridCostOre) and comparing
// it to a no-battery counter-factual.
//
// Pure functions only. Callers (api package) load the inputs from
// state.Store and pass them in. Testing is therefore arithmetic, not
// SQL — synthetic fixtures cover the algorithm.
//
// Site sign convention everywhere (see docs/site-convention.md):
//
//	grid_w  > 0 import   (consumer pays at PriceOre)
//	grid_w  < 0 export   (consumer earns at SpotOre + bonus − fee, clamped ≥ 0)
//	pv_w   ≤ 0 production (so load_w + pv_w is the no-battery grid flow)
//	bat_w  > 0 charging  (load on the AC bus)
//	bat_w  < 0 discharging
//
// Cost model is delegated entirely to mpc.SlotGridCostOre — keep the two
// agreeing or the savings number drifts from what the live plan claims.
package savings

import (
	"sort"

	"github.com/frahlg/forty-two-watts/go/internal/mpc"
	"github.com/frahlg/forty-two-watts/go/internal/state"
)

// DaySavings is the per-day result. All energies in kWh, all costs in öre.
// Negative SavingsOre means the optimizer cost us money vs the no-battery
// baseline (rare but possible — round-trip losses with low arbitrage).
type DaySavings struct {
	StartMs int64 `json:"start_ms"`
	EndMs   int64 `json:"end_ms"`

	// Whole-day totals.
	ActualOre    float64 `json:"actual_ore"`
	NoBatteryOre float64 `json:"no_battery_ore"`
	SavingsOre   float64 `json:"savings_ore"`

	// Energy totals (kWh, always ≥ 0).
	ImportKWh        float64 `json:"import_kwh"`
	ExportKWh        float64 `json:"export_kwh"`
	PVKWh            float64 `json:"pv_kwh"`
	LoadKWh          float64 `json:"load_kwh"`
	BatChargedKWh    float64 `json:"bat_charged_kwh"`
	BatDischargedKWh float64 `json:"bat_discharged_kwh"`

	// CoveragePct is the fraction of the day's expected slot-time that
	// had both a price and at least one history sample. 1.0 = full
	// coverage; lower values flag a partially-reconstructible day so the
	// UI can show "ofullständig" / data-quality warnings.
	CoveragePct float64 `json:"coverage_pct"`

	Slots []SlotSavings `json:"slots"`
}

// SlotSavings is one priced slot's worth of detail. The drill-down view
// renders one row per slot: "at 14:00 we used 0.42 kWh from the battery
// instead of grid, saving X öre".
type SlotSavings struct {
	StartMs    int64   `json:"start_ms"`
	LenMin     int     `json:"len_min"`
	PriceOre   float64 `json:"price_ore"`
	SpotOre    float64 `json:"spot_ore"`
	CoverageMs int64   `json:"coverage_ms"` // how many ms inside the slot had data

	// Measured energies.
	LoadKWh          float64 `json:"load_kwh"`
	PVKWh            float64 `json:"pv_kwh"`
	GridImportKWh    float64 `json:"grid_import_kwh"`
	GridExportKWh    float64 `json:"grid_export_kwh"`
	BatChargedKWh    float64 `json:"bat_charged_kwh"`
	BatDischargedKWh float64 `json:"bat_discharged_kwh"`

	// Counter-factual (what would have flowed at the meter without a battery).
	NoBatteryGridKWh float64 `json:"no_battery_grid_kwh"`

	// Costs.
	ActualOre    float64 `json:"actual_ore"`
	NoBatteryOre float64 `json:"no_battery_ore"`
	SavingsOre   float64 `json:"savings_ore"`

	// Decomposition — for the "HOW we saved" pedagogical view.
	Flows FlowBreakdown `json:"flows"`
}

// FlowBreakdown answers "where did each kWh come from / go to?" — purely
// bookkeeping, doesn't affect the cost numbers above. Identities (modulo
// measurement noise):
//
//	load_kwh = self_consumption + bat_to_home + grid_to_home
//	pv_kwh   = self_consumption + pv_to_bat   + direct_export
//	import   = grid_to_home + grid_to_bat
//	export   = direct_export + bat_to_grid
type FlowBreakdown struct {
	SelfConsumptionKWh float64 `json:"self_consumption_kwh"`
	DirectExportKWh    float64 `json:"direct_export_kwh"`
	PVToBatKWh         float64 `json:"pv_to_bat_kwh"`
	BatToHomeKWh       float64 `json:"bat_to_home_kwh"`
	BatToGridKWh       float64 `json:"bat_to_grid_kwh"`
	GridToHomeKWh      float64 `json:"grid_to_home_kwh"`
	GridToBatKWh       float64 `json:"grid_to_bat_kwh"`
}

// ComputeDay re-prices the day [startMs, endMs) by walking each price slot,
// integrating measured power channels over it, and scoring both the actual
// grid flow and the no-battery counter-factual with mpc.SlotGridCostOre.
//
// Inputs:
//
//   - history must be sorted by TsMs ascending. Points outside [startMs,
//     endMs) are tolerated (the caller may include leading/trailing rows
//     so first-slot integration can find a prev_ts; we use them).
//   - prices is the price table for the day's zone, also ascending. Slots
//     fully outside [startMs, endMs) are skipped.
//   - params supplies ExportOrePerKWh / ExportBonusOreKwh / ExportFeeOreKwh
//     so SlotGridCostOre prices export consistently with the live plan.
//
// Returns a fully-populated DaySavings; if there are no priced slots that
// overlap [startMs, endMs), CoveragePct is 0 and Slots is empty.
func ComputeDay(
	history []state.HistoryPoint,
	prices []state.PricePoint,
	params mpc.Params,
	startMs, endMs int64,
) DaySavings {
	out := DaySavings{StartMs: startMs, EndMs: endMs}
	if endMs <= startMs {
		return out
	}

	// Ensure ascending order — caller is contracted to provide this, but
	// a copy + sort is cheap and removes a footgun.
	if !ascending(history) {
		hh := make([]state.HistoryPoint, len(history))
		copy(hh, history)
		sort.Slice(hh, func(i, j int) bool { return hh[i].TsMs < hh[j].TsMs })
		history = hh
	}
	if !pricesAscending(prices) {
		pp := make([]state.PricePoint, len(prices))
		copy(pp, prices)
		sort.Slice(pp, func(i, j int) bool { return pp[i].SlotTsMs < pp[j].SlotTsMs })
		prices = pp
	}

	var coveredMs int64
	var totalSlotMs int64

	for _, p := range prices {
		slotLenMs := int64(p.SlotLenMin) * 60_000
		if slotLenMs <= 0 {
			continue
		}
		slotStart := p.SlotTsMs
		slotEnd := slotStart + slotLenMs
		// Skip slots that don't overlap the day window.
		if slotEnd <= startMs || slotStart >= endMs {
			continue
		}
		// Clip to day window so a 60-min slot straddling midnight
		// contributes only the portion inside [startMs, endMs).
		clipStart := slotStart
		if clipStart < startMs {
			clipStart = startMs
		}
		clipEnd := slotEnd
		if clipEnd > endMs {
			clipEnd = endMs
		}
		clippedMs := clipEnd - clipStart
		totalSlotMs += clippedMs

		integ := integrateSlot(history, clipStart, clipEnd)

		// Build the fake mpc.Slot used purely for SlotGridCostOre. The
		// DP-only fields (Confidence, Limits) don't matter for cost
		// reconstruction — SlotGridCostOre never reads them.
		slot := mpc.Slot{
			StartMs:  clipStart,
			LenMin:   int(clippedMs / 60_000),
			PriceOre: p.TotalOreKwh,
			SpotOre:  p.SpotOreKwh,
		}

		// Price each direction independently. SlotGridCostOre is sign-aware
		// (positive = import @ PriceOre, negative = export @ SpotOre−fee
		// clamped ≥ 0); calling it twice with +import then −export sums
		// the two half-flows correctly even when both occur in the same
		// slot. Netting them first would silently zero out a slot that
		// both bought and sold electricity.
		actualOre := mpc.SlotGridCostOre(slot, integ.GridImportKWh, params) +
			mpc.SlotGridCostOre(slot, -integ.GridExportKWh, params)
		noBatOre := mpc.SlotGridCostOre(slot, integ.NoBatImportKWh, params) +
			mpc.SlotGridCostOre(slot, -integ.NoBatExportKWh, params)
		noBatGridKWh := integ.NoBatImportKWh - integ.NoBatExportKWh

		ss := SlotSavings{
			StartMs:          clipStart,
			LenMin:           slot.LenMin,
			PriceOre:         p.TotalOreKwh,
			SpotOre:          p.SpotOreKwh,
			CoverageMs:       integ.CoveredMs,
			LoadKWh:          integ.LoadKWh,
			PVKWh:            integ.PVKWh,
			GridImportKWh:    integ.GridImportKWh,
			GridExportKWh:    integ.GridExportKWh,
			BatChargedKWh:    integ.BatChargedKWh,
			BatDischargedKWh: integ.BatDischargedKWh,
			NoBatteryGridKWh: noBatGridKWh,
			ActualOre:        actualOre,
			NoBatteryOre:     noBatOre,
			SavingsOre:       noBatOre - actualOre,
			Flows:            decomposeFlows(integ),
		}
		out.Slots = append(out.Slots, ss)

		out.ActualOre += actualOre
		out.NoBatteryOre += noBatOre
		out.ImportKWh += integ.GridImportKWh
		out.ExportKWh += integ.GridExportKWh
		out.PVKWh += integ.PVKWh
		out.LoadKWh += integ.LoadKWh
		out.BatChargedKWh += integ.BatChargedKWh
		out.BatDischargedKWh += integ.BatDischargedKWh
		coveredMs += integ.CoveredMs
	}
	out.SavingsOre = out.NoBatteryOre - out.ActualOre
	if totalSlotMs > 0 {
		out.CoveragePct = float64(coveredMs) / float64(totalSlotMs)
		if out.CoveragePct > 1 {
			out.CoveragePct = 1
		}
	}
	return out
}

func ascending(h []state.HistoryPoint) bool {
	for i := 1; i < len(h); i++ {
		if h[i].TsMs < h[i-1].TsMs {
			return false
		}
	}
	return true
}

func pricesAscending(p []state.PricePoint) bool {
	for i := 1; i < len(p); i++ {
		if p[i].SlotTsMs < p[i-1].SlotTsMs {
			return false
		}
	}
	return true
}
