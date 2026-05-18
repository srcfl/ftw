package state


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

// DailyCostBreakdown integrates grid flow across [sinceMs, untilMs] using all
// three history tiers and prices it slot-by-slot against the prices table for
// zone. The price for each integration step is the slot whose [slot_ts_ms,
// slot_ts_ms+slot_len_min*60000) window contains the midpoint of (prev_ts,
// ts_ms] — using the midpoint instead of either edge keeps the result stable
// across slot boundaries (no off-by-one based on which side of the boundary a
// sample landed on).
//
// Two SQL round-trips: one for slot-weighted actuals, one for unweighted
// averages over slots in the range. SetMaxOpenConns(1) makes interleaved
// queries deadlock, so they're sequential by design.
//
// Returns zeroes (not an error) when the history is empty over the range —
// callers can render that as "no data" without special-casing nil.
func (s *Store) DailyCostBreakdown(sinceMs, untilMs int64, zone string, ep ExportPricing) (DayCostBreakdown, error) {
	// Positional `?` params (named params with numeric or leading-digit
	// names are rejected by the SQLite driver). Args repeat where the SQL
	// uses the same logical value more than once — keep the order in lock
	// step with the `?`s below.
	const integrate = `
WITH all_rows AS (
	SELECT ts_ms, COALESCE(grid_w, 0) AS grid_w FROM history_hot  WHERE ts_ms BETWEEN ? AND ?
	UNION ALL
	SELECT ts_ms, COALESCE(grid_w, 0)            FROM history_warm WHERE ts_ms BETWEEN ? AND ?
	UNION ALL
	SELECT ts_ms, COALESCE(grid_w, 0)            FROM history_cold WHERE ts_ms BETWEEN ? AND ?
),
lagged AS (
	SELECT ts_ms, grid_w, LAG(ts_ms) OVER (ORDER BY ts_ms) AS prev_ts FROM all_rows
),
priced AS (
	SELECT
		l.grid_w,
		(l.ts_ms - l.prev_ts)              AS dt_ms,
		(l.ts_ms + l.prev_ts) / 2          AS mid_ts,
		(SELECT p.total_ore_kwh FROM prices p
		 WHERE p.zone = ?
		   AND p.slot_ts_ms <= (l.ts_ms + l.prev_ts) / 2
		   AND p.slot_ts_ms + p.slot_len_min * 60000 > (l.ts_ms + l.prev_ts) / 2
		 LIMIT 1)                         AS total_ore,
		(SELECT p.spot_ore_kwh FROM prices p
		 WHERE p.zone = ?
		   AND p.slot_ts_ms <= (l.ts_ms + l.prev_ts) / 2
		   AND p.slot_ts_ms + p.slot_len_min * 60000 > (l.ts_ms + l.prev_ts) / 2
		 LIMIT 1)                         AS spot_ore
	FROM lagged l
	WHERE l.prev_ts IS NOT NULL
)
SELECT
	COALESCE(SUM(CASE WHEN grid_w > 0 THEN  grid_w * dt_ms / 3600000.0 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN grid_w < 0 THEN -grid_w * dt_ms / 3600000.0 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN grid_w > 0 AND total_ore IS NOT NULL
	             THEN  grid_w * dt_ms / 3600000.0 * total_ore / 1000.0 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN grid_w < 0 AND spot_ore IS NOT NULL THEN
		-grid_w * dt_ms / 3600000.0 *
		(CASE WHEN ? > 0 THEN ?
		      WHEN (spot_ore + ? - ?) > 0 THEN (spot_ore + ? - ?)
		      ELSE 0
		 END) / 1000.0
	ELSE 0 END), 0)
FROM priced
`

	var out DayCostBreakdown
	if err := s.db.QueryRow(integrate,
		sinceMs, untilMs, // history_hot
		sinceMs, untilMs, // history_warm
		sinceMs, untilMs, // history_cold
		zone, // import-price lookup
		zone, // spot-price lookup
		ep.FlatOreKwh, ep.FlatOreKwh, // CASE WHEN flat > 0 THEN flat
		ep.BonusOreKwh, ep.FeeOreKwh, // CASE WHEN (spot + bonus - fee) > 0
		ep.BonusOreKwh, ep.FeeOreKwh, // THEN (spot + bonus - fee)
	).Scan(&out.ImportWh, &out.ExportWh, &out.ImportCostOre, &out.ExportRevenueOre); err != nil {
		return DayCostBreakdown{}, err
	}

	// Unweighted averages over slots in range — the flat-rate baseline.
	const avgQ = `
SELECT
	COALESCE(AVG(total_ore_kwh), 0),
	COALESCE(AVG(
		CASE WHEN ? > 0 THEN ?
		     WHEN (spot_ore_kwh + ? - ?) > 0 THEN (spot_ore_kwh + ? - ?)
		     ELSE 0
		END
	), 0)
FROM prices
WHERE zone = ? AND slot_ts_ms BETWEEN ? AND ?
`
	if err := s.db.QueryRow(avgQ,
		ep.FlatOreKwh, ep.FlatOreKwh,
		ep.BonusOreKwh, ep.FeeOreKwh,
		ep.BonusOreKwh, ep.FeeOreKwh,
		zone, sinceMs, untilMs,
	).Scan(&out.AvgImportOreKwh, &out.AvgExportOreKwh); err != nil {
		return DayCostBreakdown{}, err
	}

	return out, nil
}
