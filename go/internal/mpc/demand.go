package mpc

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"time"
)

const (
	weekdayPeakStartHour = 6
	weekdayPeakEndHour   = 20
	nightStartHour       = 22
	nightEndHour         = 6
	defaultDemandTopN    = 3
	demandChargeID       = "weekday-high"
	demandCoverageFloor  = 0.5
	maxDemandAlreadyKW   = 744
	maxDemandHours       = 512
)

// DemandCharge is one monthly peak-power tariff on the Energyplan wire.
// PricePerKW uses the same minor currency units as slot prices per kWh.
// Each local day is one sample: a day the plan reaches is the group of its
// hours, the clock hour in progress and its completed peak among them, and
// every other completed day is a value in AlreadyKW.
type DemandCharge struct {
	ID         string
	PricePerKW float64
	TopN       int
	AlreadyKW  []float64
	Hours      []DemandHour
	// LegacyAlreadyKW, when set, is AlreadyKW for a worker without
	// demand_elapsed_hours: every completed day, the planned ones too.
	LegacyAlreadyKW []float64
}

// DemandHour is one local clock hour that ends inside the planning horizon.
// ElapsedImportKWh is its import before the first slot's execution start.
type DemandHour struct {
	StartMs          int64
	EndMs            int64
	ElapsedImportKWh float64
	Weight           float64
	Group            string
	// Elapsed marks an hour only a worker that announces
	// demand_elapsed_hours takes: the hour in progress when it began before
	// the first slot, or a planned day's completed peak.
	Elapsed bool
}

// HourImport is the clock hour in progress as Core's live import counter
// measured it from the site meter: its import and the time the readings
// cover.
type HourImport struct {
	Start      time.Time
	ImportedWh float64
	Covered    time.Duration
}

type clockHour struct {
	start, end time.Time
	weight     float64
}

func nightHour(h int) bool {
	return h >= nightStartHour || h < nightEndHour
}

func siteLocation(zone string) *time.Location {
	if zone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.UTC
	}
	return loc
}

func weekdayPeakClockHours(loc *time.Location, from, to time.Time) []clockHour {
	if loc == nil || !to.After(from) {
		return nil
	}
	from, to = from.In(loc), to.In(loc)
	day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc)
	var out []clockHour
	for !day.After(to) {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			start := time.Date(day.Year(), day.Month(), day.Day(), weekdayPeakStartHour, 0, 0, 0, loc)
			end := time.Date(day.Year(), day.Month(), day.Day(), weekdayPeakEndHour, 0, 0, 0, loc)
			for hour := start; hour.Before(end); hour = hour.Add(time.Hour) {
				closeHour := hour.Add(time.Hour)
				if !closeHour.After(from) || !hour.Before(to) {
					continue
				}
				out = append(out, clockHour{hour, closeHour, 0})
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return out
}

func ellevioClockHours(loc *time.Location, from, to time.Time, nightWeight float64) []clockHour {
	if loc == nil || !to.After(from) || nightWeight <= 0 {
		return nil
	}
	from, to = from.In(loc), to.In(loc)
	hour := time.Date(from.Year(), from.Month(), from.Day(), from.Hour(), 0, 0, 0, loc)
	var out []clockHour
	for hour.Before(to) {
		closeHour := hour.Add(time.Hour)
		if closeHour.After(from) && hour.Before(to) {
			weight := 0.0
			if nightHour(hour.Hour()) {
				weight = nightWeight
			}
			out = append(out, clockHour{hour, closeHour, weight})
		}
		hour = closeHour
	}
	return out
}

// bindDemandCharges expands the tariff to the month's clock hours. Hours
// that end inside the horizon and reach the plan go out, the hour in
// progress too when it began before the first slot. A local day the plan
// reaches sends its completed peak as an hour of its group; other completed
// days go in AlreadyKW. One history read serves both request shapes
// (legacyDemandCharges). live is Core's own count of the hour in progress.
func bindDemandCharges(slots []Slot, pricePerKW float64, topN int, vatPercent, nightWeight float64, loc *time.Location, now time.Time, importWh func([][2]int64) ([]float64, []int64), live HourImport) []DemandCharge {
	if pricePerKW <= 0 || !finite(pricePerKW) || len(slots) == 0 {
		return nil
	}
	if topN <= 0 {
		topN = defaultDemandTopN
	}
	if loc == nil {
		loc = time.UTC
	}
	horizonStart := slots[0].StartMs
	horizonEnd, ok := slotEndMs(slots[len(slots)-1])
	if !ok {
		return nil
	}
	execStart := slots[0].StartMs
	if slots[0].ExecutionStartMs > execStart {
		execStart = slots[0].ExecutionStartMs
	}
	monthStart := time.Date(now.In(loc).Year(), now.In(loc).Month(), 1, 0, 0, 0, 0, loc)
	until := time.UnixMilli(horizonEnd).In(loc)
	hours := weekdayPeakClockHours(loc, monthStart, until)
	if nightWeight > 0 {
		hours = ellevioClockHours(loc, monthStart, until, nightWeight)
	}
	if len(hours) == 0 {
		return nil
	}

	type dayPeak struct {
		kw, wh float64
		hour   clockHour
	}
	type monthAcc struct {
		id         string
		already    []float64
		alreadyDay map[string]int // local day → index in already
		legacy     []float64      // already, with the planned days' peaks
		legacyDay  map[string]int
		peaks      map[string]dayPeak // a planned day's completed peak
		planned    map[string]bool    // local days with planned hours
		hours      []DemandHour
		elapsed    [][2]int64
		elapsedI   []int
	}
	months := make(map[string]*monthAcc)
	var order []string
	acc := func(hour clockHour) *monthAcc {
		key := hour.start.Format("2006-01")
		m := months[key]
		if m == nil {
			m = &monthAcc{id: demandChargeID + "-" + key}
			months[key] = m
			order = append(order, key)
		}
		return m
	}

	var look [][2]int64
	var lookKind []string
	var lookMonth []string
	var lookHour []clockHour
	for _, hour := range hours {
		startMs, endMs := hour.start.UnixMilli(), hour.end.UnixMilli()
		inside := endMs <= horizonEnd
		overlapsPlan := endMs > execStart && startMs < horizonEnd
		if inside && overlapsPlan {
			m := acc(hour)
			if len(m.hours) >= maxDemandHours {
				continue
			}
			idx := len(m.hours)
			day := hour.start.Format("2006-01-02")
			m.hours = append(m.hours, DemandHour{
				StartMs: startMs, EndMs: endMs,
				Weight: hour.weight,
				Group:  day,
				// The hour in progress, begun before the first slot.
				Elapsed: startMs < horizonStart,
			})
			if m.planned == nil {
				m.planned = make(map[string]bool)
			}
			m.planned[day] = true
			if execStart > startMs && execStart < endMs {
				m.elapsed = append(m.elapsed, [2]int64{startMs, execStart})
				m.elapsedI = append(m.elapsedI, idx)
			}
			continue
		}
		if endMs <= execStart {
			look = append(look, [2]int64{startMs, endMs})
			lookKind = append(lookKind, "already")
			lookMonth = append(lookMonth, hour.start.Format("2006-01"))
			lookHour = append(lookHour, hour)
		}
	}

	for i := range order {
		m := months[order[i]]
		for _, window := range m.elapsed {
			look = append(look, window)
			lookKind = append(lookKind, "elapsed")
			lookMonth = append(lookMonth, order[i])
		}
	}
	// The history of each window, or none: wh and covered stay zero.
	wh, covered := make([]float64, len(look)), make([]int64, len(look))
	if importWh != nil {
		if gotWh, gotCovered := importWh(look); len(gotWh) == len(look) && len(gotCovered) == len(look) {
			wh, covered = gotWh, gotCovered
		}
	}
	slot0W := max(0, slots[0].LoadW+slots[0].PVW)
	if importWh != nil {
		if len(wh) == len(look) && len(covered) == len(look) {
			elapsedAt := make(map[string]int)
			for i, kind := range lookKind {
				key := lookMonth[i]
				if kind == "already" {
					if i >= len(lookHour) {
						continue
					}
					hour := lookHour[i]
					m := months[key]
					if m == nil {
						m = acc(hour)
					}
					duration := look[i][1] - look[i][0]
					if duration <= 0 || float64(covered[i]) < demandCoverageFloor*float64(duration) {
						continue
					}
					// History follows the rule the planned hours carry: a
					// night hour counts at its weight, and each local day
					// contributes one peak.
					kw := wh[i] * 3600 / float64(duration)
					if hour.weight > 0 {
						kw *= hour.weight
					}
					day := hour.start.Format("2006-01-02")
					if j, ok := m.legacyDay[day]; ok {
						m.legacy[j] = max(m.legacy[j], kw)
					} else if len(m.legacy) < maxDemandAlreadyKW {
						if m.legacyDay == nil {
							m.legacyDay = make(map[string]int)
						}
						m.legacyDay[day] = len(m.legacy)
						m.legacy = append(m.legacy, kw)
					}
					if m.planned[day] {
						// The day is one sample: its peak joins its group.
						if peak, ok := m.peaks[day]; !ok || kw > peak.kw {
							if m.peaks == nil {
								m.peaks = make(map[string]dayPeak)
							}
							m.peaks[day] = dayPeak{kw: kw, wh: wh[i], hour: hour}
						}
						continue
					}
					if j, ok := m.alreadyDay[day]; ok {
						m.already[j] = max(m.already[j], kw)
						continue
					}
					if len(m.already) >= maxDemandAlreadyKW {
						continue
					}
					if m.alreadyDay == nil {
						m.alreadyDay = make(map[string]int)
					}
					m.alreadyDay[day] = len(m.already)
					m.already = append(m.already, kw)
					continue
				}
				m := months[key]
				n := elapsedAt[key]
				if m == nil || n >= len(m.elapsedI) {
					continue
				}
				if hour := &m.hours[m.elapsedI[n]]; !hour.Elapsed {
					hour.ElapsedImportKWh = wh[i] / 1000
				}
				elapsedAt[key] = n + 1
			}
		}
	}
	// The hour in progress that began before the first slot, with or
	// without history.
	for i, kind := range lookKind {
		if kind != "elapsed" {
			continue
		}
		m := months[lookMonth[i]]
		for _, idx := range m.elapsedI {
			hour := &m.hours[idx]
			if hour.Elapsed && hour.StartMs == look[i][0] {
				hour.ElapsedImportKWh = runningHourImportWh(look[i], wh[i], covered[i], live, slot0W) / 1000
			}
		}
	}
	for _, key := range order {
		m := months[key]
		if len(m.peaks) == 0 {
			continue
		}
		days := make([]string, 0, len(m.peaks))
		for day := range m.peaks {
			days = append(days, day)
		}
		slices.Sort(days)
		for _, day := range days {
			peak := m.peaks[day]
			if len(m.hours) >= maxDemandHours {
				m.already = append(m.already, peak.kw)
				continue
			}
			m.hours = append(m.hours, DemandHour{
				StartMs: peak.hour.start.UnixMilli(), EndMs: peak.hour.end.UnixMilli(),
				ElapsedImportKWh: peak.wh / 1000, Weight: peak.hour.weight, Group: day, Elapsed: true,
			})
		}
		slices.SortStableFunc(m.hours, func(a, b DemandHour) int { return cmp.Compare(a.StartMs, b.StartMs) })
	}

	wirePrice := pricePerKW * (1 + vatPercent/100)
	if !finite(wirePrice) || wirePrice < 0 {
		return nil
	}
	out := make([]DemandCharge, 0, len(order))
	for _, key := range order {
		m := months[key]
		if len(m.hours) == 0 && len(m.already) == 0 {
			continue
		}
		charge := DemandCharge{
			ID: m.id, PricePerKW: wirePrice, TopN: topN,
			AlreadyKW: m.already, Hours: m.hours,
		}
		if len(m.peaks) > 0 {
			charge.LegacyAlreadyKW = m.legacy
		}
		out = append(out, charge)
	}
	if len(out) > 16 {
		out = out[:16]
	}
	return out
}

// runningHourImportWh is the import of the clock hour in progress before
// the first slot's execution start, window. Core's live counter answers when
// it measures that hour, else the import history. Time neither covers counts
// at the larger of the covered mean and slot 0's forecast import, never at
// zero: an undercount would let the plan set a new peak in the hour.
func runningHourImportWh(window [2]int64, historyWh float64, historyCoveredMs int64, live HourImport, slot0W float64) float64 {
	spanMs := window[1] - window[0]
	wh, coveredMs := historyWh, historyCoveredMs
	if !live.Start.IsZero() && live.Start.UnixMilli() == window[0] && live.Covered > 0 {
		wh, coveredMs = live.ImportedWh, live.Covered.Milliseconds()
	}
	if !finite(wh) || wh < 0 {
		wh, coveredMs = 0, 0
	}
	coveredMs = min(max(coveredMs, 0), spanMs)
	rateW := max(0, slot0W)
	if coveredMs > 0 {
		rateW = max(rateW, wh*3_600_000/float64(coveredMs))
	}
	return wh + rateW*float64(spanMs-coveredMs)/3_600_000
}

// legacyDemandCharges is the shape a worker without demand_elapsed_hours
// takes: no hour that began before the first slot, each completed day in
// AlreadyKW. A charge left with neither goes, and a month left with
// completed days only goes after the months with planned hours, as before.
func legacyDemandCharges(charges []DemandCharge) []DemandCharge {
	var out []DemandCharge
	for _, c := range charges {
		legacy := DemandCharge{ID: c.ID, PricePerKW: c.PricePerKW, TopN: c.TopN, AlreadyKW: c.AlreadyKW}
		if c.LegacyAlreadyKW != nil {
			legacy.AlreadyKW = c.LegacyAlreadyKW
		}
		for _, h := range c.Hours {
			if !h.Elapsed {
				legacy.Hours = append(legacy.Hours, h)
			}
		}
		if len(legacy.Hours) > 0 || len(legacy.AlreadyKW) > 0 {
			out = append(out, legacy)
		}
	}
	historyOnly := func(c DemandCharge) int {
		if len(c.Hours) == 0 {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(out, func(a, b DemandCharge) int { return cmp.Compare(historyOnly(a), historyOnly(b)) })
	return out
}

// hasElapsedDemand reports whether a charge holds an hour only a worker
// that announces demand_elapsed_hours takes.
func hasElapsedDemand(charges []DemandCharge) bool {
	for _, c := range charges {
		if c.LegacyAlreadyKW != nil || slices.ContainsFunc(c.Hours, func(h DemandHour) bool { return h.Elapsed }) {
			return true
		}
	}
	return false
}

func slotEndMs(slot Slot) (int64, bool) {
	durationMs := int64(slot.LenMin) * 60_000
	end := slot.StartMs + durationMs
	if slot.LenMin <= 0 || durationMs/60_000 != int64(slot.LenMin) || end < slot.StartMs {
		return 0, false
	}
	return end, true
}

func (s *Service) demandChargesFor(site SiteEconomics, slots []Slot, now time.Time) []DemandCharge {
	if s == nil || site.DemandPricePerKW <= 0 {
		return nil
	}
	loc := siteLocation(site.Timezone)
	var live HourImport
	if s.HourImport != nil {
		live = s.HourImport()
	}
	return bindDemandCharges(slots, site.DemandPricePerKW, site.DemandTopN, site.VATPercent, site.DemandNightWeight, loc, now, s.importEnergy, live)
}

func (s *Service) importEnergy(intervals [][2]int64) ([]float64, []int64) {
	empty := func() ([]float64, []int64) {
		wh := make([]float64, len(intervals))
		cov := make([]int64, len(intervals))
		return wh, cov
	}
	if s.Store == nil || len(intervals) == 0 {
		return empty()
	}
	wh, cov, err := s.Store.ImportWhIntervals(context.Background(), intervals)
	if err != nil {
		slog.Warn("mpc: demand-charge import history failed", "err", err)
		return empty()
	}
	if len(wh) != len(intervals) || len(cov) != len(intervals) {
		return empty()
	}
	return wh, cov
}
