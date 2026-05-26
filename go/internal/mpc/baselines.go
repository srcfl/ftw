package mpc

// ComputeBaselines returns counter-factual dispatch costs over the
// given horizon + params so the UI can show "savings vs X" numbers.
//
// Three baselines are computed:
//
//   - NoBatteryOre: each slot's grid flow = load + pv (pretend the
//     battery doesn't exist). Costed with the same import/export model
//     the DP uses.
//
//   - SelfConsumptionOre: re-runs Optimize with Mode=SelfConsumption
//     over the same slots and params. Using the optimizer itself (vs a
//     hand-rolled simulation) means we inherit the real efficiency,
//     power, SoC-bound, and grid-policy constraints — and the cost is
//     computed by the DP's own per-slot loop, so it's directly
//     comparable to plan.TotalCostOre.
//
//   - FlatAvgOre: no-battery import volume × horizon mean import price,
//     minus no-battery export volume × horizon mean export revenue.
//     Shows the value of *when* energy is moved — if the optimizer
//     saves more vs FlatAvg than vs NoBattery, most of the win is
//     timing (shifting load into cheap hours) rather than PV
//     self-consumption. A diagnostic, not an operational baseline.
//
//     Import and export are priced separately (and at their respective
//     means) because the consumer total PriceOre includes grid tariffs
//     that aren't earned on export. Netting energy at a single mean
//     would credit each exported kWh at the import-tariff price — and
//     so any horizon with even partial export would show an artificially
//     low FlatAvg, hiding timing value behind the asymmetry.
//
// Cheap to call — the SC re-optimize is one extra Optimize pass
// (~10ms for the default 193 slots × 51 SoC × 21 actions).
func ComputeBaselines(slots []Slot, p Params) Baselines {
	b := Baselines{}
	if len(slots) == 0 {
		return b
	}

	// ---- No-battery baseline + flat-average inputs ----
	// One pass: integrate no-battery grid flow per slot, bucket into
	// import / export volumes, and accumulate time-weighted means of
	// the import price and the export revenue (using the same per-slot
	// formula SlotGridCostOre uses). FlatAvgOre then re-scores the
	// no-battery flows at those flat means.
	var importKWh, exportKWh float64
	var importPriceWtMin, exportPriceWtMin float64
	var lenMinSum float64
	for _, s := range slots {
		dt := float64(s.LenMin) / 60.0
		gridKWh := (s.LoadW + s.PVW) * dt / 1000.0
		b.NoBatteryOre += SlotGridCostOre(s, gridKWh, p)
		if gridKWh > 0 {
			importKWh += gridKWh
		} else {
			exportKWh += -gridKWh
		}
		lm := float64(s.LenMin)
		importPriceWtMin += s.PriceOre * lm
		exportPriceWtMin += SlotExportPriceOre(s, p) * lm
		lenMinSum += lm
	}
	var avgImportOre, avgExportOre float64
	if lenMinSum > 0 {
		avgImportOre = importPriceWtMin / lenMinSum
		avgExportOre = exportPriceWtMin / lenMinSum
	}
	// AvgPriceOre keeps its meaning as the horizon's mean import price —
	// that's what the planner UI's "vs flat avg" tooltip references.
	b.AvgPriceOre = avgImportOre
	b.NetKWh = importKWh - exportKWh
	b.FlatAvgOre = importKWh*avgImportOre - exportKWh*avgExportOre

	// ---- Self-consumption baseline ----
	// Re-run Optimize with the SC policy. Drop the loadpoint from the
	// SC baseline — SC mode wouldn't normally schedule an EV charge,
	// and including its mandatory SoC target would distort the "what if
	// we just did SC" comparison.
	pSC := p
	pSC.Mode = ModeSelfConsumption
	pSC.Loadpoint = nil
	scPlan := Optimize(slots, pSC)
	b.SelfConsumptionOre = scPlan.TotalCostOre
	return b
}
