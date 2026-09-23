package mpc

import (
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// Load rain-check: the coming day's forecast must not collapse far below
// what the house actually used on recent days. Days can differ a lot, so
// this only lifts a collapsed forecast — it never force-fits the shape.
const (
	loadRainCheckDays         = 3
	loadRainCheckMinFraction  = 0.2 // lift only when forecast Wh < 20% of recent mean
	loadRainCheckMaxScale     = 1.5
	loadRecentMeanFloorFrac   = 0.2 // no slot below 20% of recent mean watts
	loadRecentDayMinIntervals = 50
	loadRecentDayMinWh        = 500
)

func clampLoadW(w, minW, maxW float64) float64 {
	if math.IsNaN(w) || math.IsInf(w, 0) {
		if minW > 0 {
			return minW
		}
		return 0
	}
	if w < minW {
		return minW
	}
	if maxW > 0 && w > maxW {
		return maxW
	}
	return w
}

func capSlotsLoad(slots []Slot, minW, maxW float64) []Slot {
	for i := range slots {
		slots[i].LoadW = clampLoadW(slots[i].LoadW, minW, maxW)
	}
	return slots
}

func capPlanLoad(plan *Plan, minW, maxW float64) {
	if plan == nil {
		return
	}
	if maxW > 0 {
		plan.LoadMaxW = maxW
	}
	for i := range plan.Actions {
		plan.Actions[i].LoadW = clampLoadW(plan.Actions[i].LoadW, minW, maxW)
	}
}

func forecastLoadWh(slots []Slot) float64 {
	var wh float64
	for _, s := range slots {
		wh += math.Max(0, s.LoadW) * math.Max(0, s.DurationHours())
	}
	return wh
}

// rainCheckLoadSlots floors each slot against a fraction of the recent
// daily mean watts, then scales the whole horizon up if the integrated
// forecast is still far below recent days. Shape is preserved; the fuse
// (maxW) still wins.
func rainCheckLoadSlots(slots []Slot, recentDayWh, maxW float64) []Slot {
	if recentDayWh <= 0 || len(slots) == 0 {
		return slots
	}
	minW := (recentDayWh / 24.0) * loadRecentMeanFloorFrac
	slots = capSlotsLoad(slots, minW, maxW)

	got := forecastLoadWh(slots)
	floorWh := recentDayWh * loadRainCheckMinFraction
	if got >= floorWh || got <= 0 {
		return slots
	}
	scale := floorWh / got
	if scale > loadRainCheckMaxScale {
		scale = loadRainCheckMaxScale
	}
	for i := range slots {
		slots[i].LoadW = clampLoadW(slots[i].LoadW*scale, minW, maxW)
	}
	return slots
}

func recentDailyLoadWh(st *state.Store, now time.Time, days int) float64 {
	if st == nil || days <= 0 {
		return 0
	}
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	var sum float64
	var n int
	for i := 1; i <= days; i++ {
		start := today.AddDate(0, 0, -i)
		end := start.AddDate(0, 0, 1)
		de, err := st.DailyEnergy(start.UnixMilli(), end.UnixMilli())
		if err != nil || de.Intervals < loadRecentDayMinIntervals || de.LoadWh < loadRecentDayMinWh {
			continue
		}
		sum += de.LoadWh
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}
