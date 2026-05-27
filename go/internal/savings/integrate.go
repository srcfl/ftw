package savings

import "github.com/frahlg/forty-two-watts/go/internal/state"

// MaxGapMs caps the per-row integration interval. The history schema
// produces samples ~5 s apart in hot, ~15 min in warm, ~1 day in cold.
// 20 minutes accommodates warm-tier rows (and a missed hot-tier tick or
// two) while still rejecting genuine outages — a 6-hour gap between two
// rows shouldn't make the optimizer look like it ran flawlessly through
// an outage. Rows further apart than this contribute zero energy and
// zero coverage to the slot.
const MaxGapMs int64 = 20 * 60 * 1000

// slotIntegrals is the per-channel energy total over one priced slot,
// plus the wall-clock coverage that produced it. Import / export pairs are
// accumulated by sign at sample resolution — netting them at slot
// boundaries would mis-price a slot where both directions occurred (e.g.
// the sun came up partway through and a no-battery house would have
// imported before then and exported after).
type slotIntegrals struct {
	LoadKWh          float64
	PVKWh            float64 // ≥ 0 (sign flipped from site convention)
	GridImportKWh    float64 // ≥ 0
	GridExportKWh    float64 // ≥ 0
	BatChargedKWh    float64 // ≥ 0
	BatDischargedKWh float64 // ≥ 0

	// No-battery counter-factual: load_w + pv_w per sample, split on sign.
	// Computed alongside the others so the price re-scoring honours
	// sub-slot direction changes, not just the slot net.
	NoBatImportKWh float64 // ≥ 0
	NoBatExportKWh float64 // ≥ 0

	CoveredMs int64
}

// integrateSlot accumulates kWh totals for [clipStart, clipEnd) using the
// same left-Riemann convention as state.DailyEnergy: each history row
// contributes W × (ts − prev_ts), capped at MaxGapMs and clipped to the
// slot boundary so a row whose interval straddles the slot edge attributes
// only the in-slot portion. Coverage tracks the wall-clock ms actually
// integrated; slots with sparse data report less than full coverage.
//
// History points outside the slot are still consulted — the row at index 0
// in the slot needs the row at index −1 to know how long its interval was.
func integrateSlot(history []state.HistoryPoint, clipStart, clipEnd int64) slotIntegrals {
	var out slotIntegrals
	if clipEnd <= clipStart || len(history) < 2 {
		return out
	}
	for i := 1; i < len(history); i++ {
		prevTs := history[i-1].TsMs
		curTs := history[i].TsMs
		if curTs <= prevTs {
			continue
		}
		// Reject huge gaps regardless of overlap — a stale row across
		// an outage shouldn't paint over the missing time.
		if curTs-prevTs > MaxGapMs {
			continue
		}
		// Active sub-interval of this row that lies inside the slot.
		a := prevTs
		if a < clipStart {
			a = clipStart
		}
		b := curTs
		if b > clipEnd {
			b = clipEnd
		}
		if b <= a {
			// Either fully before clipStart or fully past clipEnd.
			// Once we're past, every subsequent row is too — break.
			if prevTs >= clipEnd {
				break
			}
			continue
		}
		dtMs := b - a
		// Wh per (W × hour); divide by 1000 once more to land in kWh.
		dtKHours := float64(dtMs) / 3_600_000_000.0

		p := history[i]
		out.LoadKWh += p.LoadW * dtKHours
		// PV is negative in site-sign; flip so PVKWh is a positive accumulation.
		out.PVKWh += -p.PVW * dtKHours
		if p.GridW > 0 {
			out.GridImportKWh += p.GridW * dtKHours
		} else {
			out.GridExportKWh += -p.GridW * dtKHours
		}
		if p.BatW > 0 {
			out.BatChargedKWh += p.BatW * dtKHours
		} else {
			out.BatDischargedKWh += -p.BatW * dtKHours
		}
		nb := p.LoadW + p.PVW // site sign — pos = no-bat house imports, neg = exports
		if nb > 0 {
			out.NoBatImportKWh += nb * dtKHours
		} else {
			out.NoBatExportKWh += -nb * dtKHours
		}
		out.CoveredMs += dtMs
	}
	return out
}
