---
"forty-two-watts": minor
---

**Savings: baseline now includes EV charging priced at the day's average,
so EMS-scheduled EV laddning shows up as savings instead of zeroing out.**
Previously the `BaselineCostOre` returned by `state.DailyCostBreakdown`
(and surfaced by `/api/savings/daily` as `baseline_cost_ore`) was
`Σ slot ( house_load_w × spot_total )`, where `house_load_w` was
explicitly the meter reading minus EV (see
`main.go`'s `loadW := gridW − batW − pvW − evW`). Two consequences:

1. When the EMS scheduled EV charging onto a near-zero spot hour, that
   energy contributed ~0 to baseline but the matching grid import still
   went into `ActualCostOre`. Saved-tal looked flat or even negative.
2. When the EV was charged on a higher-priced hour (cold-start, no
   override), actual rose while baseline didn't move — the metric was
   systematically biased toward "lost" whenever the EV was active.

The breakdown now treats EV separately:

- `BaselineHouseOre` keeps the slot-by-slot house pricing (unchanged
  behaviour for the EV-less case).
- `BaselineEvOre = EVWh × AvgImportOreKwh / 1000` prices the day's EV
  energy at the day's time-weighted average spot. Interpretation: "a
  dumb charger with no timing awareness would have paid the day's avg
  per kWh". Smart scheduling onto cheap hours then surfaces as savings;
  charging on a peak shows up as a penalty. Symmetric.
- `BaselineCostOre = BaselineHouseOre + BaselineEvOre` (sum exposed for
  back-compat).
- `EVWh` is derived per history sample as
  `grid_w − bat_w − pv_w − load_w` (clamped non-negative), the inverse
  of `main.go`'s identity. No schema change.

The `/api/savings/daily` response gains `ev_wh`, `baseline_house_ore`,
and `baseline_ev_ore` fields so the UI can render the EV share of
savings separately. Existing fields (`baseline_cost_ore`, `saved_ore`,
`flat_cost_ore`) keep their names; their values now incorporate the EV
term.

Historical days will re-render with the new baseline once a process
restart clears the savings cache; volume columns are unchanged.
