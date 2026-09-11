package mpc

import (
	"context"
	"log/slog"
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
type DemandCharge struct {
	ID         string
	PricePerKW float64
	TopN       int
	AlreadyKW  []float64
	Hours      []DemandHour
}

// DemandHour is one local clock hour inside the planning horizon.
type DemandHour struct {
	StartMs          int64
	EndMs            int64
	ElapsedImportKWh float64
	Weight           float64
	Group            string
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

func bindDemandCharges(slots []Slot, pricePerKW float64, topN int, vatPercent, nightWeight float64, loc *time.Location, now time.Time, importWh func([][2]int64) ([]float64, []int64)) []DemandCharge {
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

	type monthAcc struct {
		id       string
		already  []float64
		hours    []DemandHour
		elapsed  [][2]int64
		elapsedI []int
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
		inside := startMs >= horizonStart && endMs <= horizonEnd
		overlapsPlan := endMs > execStart && startMs < horizonEnd
		if inside && overlapsPlan {
			m := acc(hour)
			if len(m.hours) >= maxDemandHours {
				continue
			}
			idx := len(m.hours)
			m.hours = append(m.hours, DemandHour{
				StartMs: startMs, EndMs: endMs,
				Weight: hour.weight,
				Group:  hour.start.Format("2006-01-02"),
			})
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

	if importWh != nil {
		for i := range order {
			m := months[order[i]]
			for _, window := range m.elapsed {
				look = append(look, window)
				lookKind = append(lookKind, "elapsed")
				lookMonth = append(lookMonth, order[i])
			}
		}
		wh, covered := importWh(look)
		if len(wh) == len(look) && len(covered) == len(look) {
			elapsedAt := make(map[string]int)
			for i, kind := range lookKind {
				key := lookMonth[i]
				if kind == "already" {
					m := months[key]
					if m == nil {
						if i >= len(lookHour) {
							continue
						}
						m = acc(lookHour[i])
					}
					duration := look[i][1] - look[i][0]
					if duration <= 0 || float64(covered[i]) < demandCoverageFloor*float64(duration) {
						continue
					}
					if len(m.already) >= maxDemandAlreadyKW {
						continue
					}
					m.already = append(m.already, wh[i]*3600/float64(duration))
					continue
				}
				m := months[key]
				n := elapsedAt[key]
				if m == nil || n >= len(m.elapsedI) {
					continue
				}
				m.hours[m.elapsedI[n]].ElapsedImportKWh = wh[i] / 1000
				elapsedAt[key] = n + 1
			}
		}
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
		out = append(out, DemandCharge{
			ID: m.id, PricePerKW: wirePrice, TopN: topN,
			AlreadyKW: m.already, Hours: m.hours,
		})
	}
	if len(out) > 16 {
		out = out[:16]
	}
	return out
}

func slotEndMs(slot Slot) (int64, bool) {
	durationMs := int64(slot.LenMin) * 60_000
	end := slot.StartMs + durationMs
	if slot.LenMin <= 0 || durationMs/60_000 != int64(slot.LenMin) || end < slot.StartMs {
		return 0, false
	}
	return end, true
}

func (s *Service) demandChargesFor(slots []Slot, now time.Time) []DemandCharge {
	if s == nil || s.DemandPricePerKW <= 0 {
		return nil
	}
	loc := siteLocation(s.Timezone)
	return bindDemandCharges(slots, s.DemandPricePerKW, s.DemandTopN, s.VATPercent, s.DemandNightWeight, loc, now, s.importEnergy)
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
