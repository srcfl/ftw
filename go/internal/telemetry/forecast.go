package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

// ForecastPowerSample qualifies source power independently of status updates.
// OCPP publishes this in reading data under forecast_power; Lua readings without
// this marker keep their existing power receipt semantics.
type ForecastPowerSample struct {
	Version      int     `json:"version"`
	Known        bool    `json:"known"`
	Watts        float64 `json:"watts"`
	MeasuredAtMS int64   `json:"measured_at_ms"`
	ReceivedAtMS int64   `json:"received_at_ms"`
}

// ForecastFlow identifies one electrical flow. FlowID joins duplicate sources
// for the same physical flow; callers must resolve those sources before training.
type ForecastFlow struct {
	Driver  string
	DerType DerType
	FlowID  string
}
type ForecastOptions struct {
	// HouseholdInvalidReason qualifies unresolved site topology without invalidating independent PV observations.
	HouseholdInvalidReason string
	PVInvalidReason        string
	ExpectedFlows          []ForecastFlow
	MaxAge                 time.Duration
	MaxSkew                time.Duration
}

// ForecastReading is one coherent snapshot, in site signs and raw watts.
// Valid describes the household balance. PVValid only describes the PV sources.
// Neither flag claims that the instantaneous sample covers a time interval.
type ForecastReading struct {
	At, Earliest, Latest, PVEarliest, PVLatest  time.Time
	GridW, PVW, BatteryW, EVW, V2XW, HouseholdW float64
	Valid, PVValid                              bool
	Reason, PVReason                            string
}

// ForecastMeasurement reads power and health under one lock. A command fault
// does not invalidate a fresh meter. Missing configured flows invalidate the
// balance, including devices that have never emitted. No fuse limits house W.
func (s *Store) ForecastMeasurement(now time.Time, siteMeter string, opts ForecastOptions) ForecastReading {
	if s != nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
	}
	return s.forecastMeasurementLocked(now, siteMeter, opts)
}

// ForecastMeasurementNow captures the time after acquiring the telemetry lock.
// A poll already in progress can finish before the snapshot without appearing
// to come from the future. Explicit forecast origins still use ForecastMeasurement.
func (s *Store) ForecastMeasurementNow(siteMeter string, opts ForecastOptions) ForecastReading {
	if s != nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
	}
	return s.forecastMeasurementLocked(time.Now(), siteMeter, opts)
}

func (s *Store) forecastMeasurementLocked(now time.Time, siteMeter string, opts ForecastOptions) ForecastReading {
	out := ForecastReading{At: now, Valid: true, PVValid: true}
	if s == nil {
		out.Valid = false
		out.PVValid = false
		out.Reason = "no_telemetry"
		out.PVReason = out.Reason
		return out
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = 90 * time.Second
	}
	if opts.MaxSkew <= 0 {
		opts.MaxSkew = 30 * time.Second
	}
	flows := map[string]ForecastFlow{}
	key := func(f ForecastFlow) string { return f.Driver + ":" + f.DerType.String() }
	fail := func(reason string, pv bool) {
		if out.Valid {
			out.Valid = false
			out.Reason = reason
		}
		if pv && out.PVValid {
			out.PVValid = false
			out.PVReason = reason
		}
	}
	if opts.PVInvalidReason != "" {
		out.PVValid = false
		out.PVReason = opts.PVInvalidReason
	}
	if opts.HouseholdInvalidReason != "" {
		fail(opts.HouseholdInvalidReason, false)
	}
	flows[siteMeter+":meter"] = ForecastFlow{Driver: siteMeter, DerType: DerMeter}
	for _, r := range s.readings {
		if r.DerType == DerPV || r.DerType == DerBattery || r.DerType == DerEV || r.DerType == DerV2X {
			f := ForecastFlow{Driver: r.Driver, DerType: r.DerType}
			flows[key(f)] = f
		}
	}
	ids := map[string]ForecastFlow{}
	for _, f := range opts.ExpectedFlows {
		if f.DerType == DerVehicle || (f.DerType == DerMeter && f.Driver != siteMeter) {
			continue
		}
		if f.FlowID != "" {
			if old, ok := ids[f.FlowID]; ok {
				fail("duplicate_flow:"+f.FlowID, f.DerType == DerPV || old.DerType == DerPV)
			}
			ids[f.FlowID] = f
		}
		flows[key(f)] = f
	}
	// Same charger represented as both EV and V2X must not be subtracted twice.
	for _, f := range flows {
		if f.DerType == DerEV {
			if _, ok := flows[f.Driver+":"+DerV2X.String()]; ok {
				fail("duplicate_charger:"+f.Driver, false)
			}
		}
	}
	keys := make([]string, 0, len(flows))
	for k := range flows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var pvFirst, pvLast time.Time
	pvCount := 0
	for _, k := range keys {
		f := flows[k]
		pv := f.DerType == DerPV
		if pv {
			pvCount++
		}
		r := s.readings[k]
		reason := ""
		var powerW float64
		var powerAt time.Time
		powerQualified := true
		if r != nil {
			powerW, powerAt = r.RawW, r.UpdatedAt
			var data struct {
				Power json.RawMessage `json:"forecast_power"`
			}
			if json.Unmarshal(r.Data, &data) == nil && len(data.Power) > 0 {
				var sample ForecastPowerSample
				powerQualified = json.Unmarshal(data.Power, &sample) == nil && sample.Version == 1 && sample.Known && sample.MeasuredAtMS > 0 && sample.ReceivedAtMS >= sample.MeasuredAtMS && sample.ReceivedAtMS <= now.UnixMilli()
				powerW, powerAt = sample.Watts, time.UnixMilli(sample.MeasuredAtMS)
			}
		}
		switch {
		case r == nil:
			reason = "missing:"
		case !s.health[f.Driver].TelemetryLive():
			reason = "offline:"
		case !powerQualified:
			reason = "unknown_power:"
		case math.IsNaN(powerW) || math.IsInf(powerW, 0):
			reason = "nonfinite:"
		case powerAt.IsZero() || now.Sub(powerAt) > opts.MaxAge:
			reason = "stale:"
		case powerAt.After(now):
			reason = "future:"
		case pv && powerW > 0:
			reason = "invalid_sign:"
		}
		if reason != "" {
			fail(reason+k, pv)
			continue
		}
		if out.Earliest.IsZero() || powerAt.Before(out.Earliest) {
			out.Earliest = powerAt
		}
		if powerAt.After(out.Latest) {
			out.Latest = powerAt
		}
		if pv {
			if pvFirst.IsZero() || powerAt.Before(pvFirst) {
				pvFirst = powerAt
			}
			if powerAt.After(pvLast) {
				pvLast = powerAt
			}
		}
		switch f.DerType {
		case DerMeter:
			out.GridW = powerW
		case DerPV:
			out.PVW += powerW
		case DerBattery:
			out.BatteryW += powerW
		case DerEV:
			out.EVW += powerW
		case DerV2X:
			out.V2XW += powerW
		}
	}
	if out.Latest.Sub(out.Earliest) > opts.MaxSkew {
		fail("time_skew", false)
	}
	out.PVEarliest, out.PVLatest = pvFirst, pvLast
	if pvLast.Sub(pvFirst) > opts.MaxSkew {
		fail("pv_time_skew", true)
	}
	if pvCount == 0 {
		out.PVValid = false
		out.PVReason = "no_pv_source"
	}
	if math.IsNaN(out.PVW) || math.IsInf(out.PVW, 0) {
		fail("nonfinite_pv_sum", true)
	}
	out.HouseholdW = out.GridW - out.PVW - out.BatteryW - out.EVW - out.V2XW
	if math.IsNaN(out.HouseholdW) || math.IsInf(out.HouseholdW, 0) || out.HouseholdW < 0 {
		fail("invalid_balance", false)
	}
	return out
}

// ForecastInterval is a fully covered quarter-hour mean. Quality is versioned
// so old history without this evidence cannot silently become training labels.
type ForecastInterval struct {
	Start, End      time.Time
	HouseholdW, PVW float64
	Quality         string
	PVValid         bool
	LatestInputMs   int64
}

const ForecastIntervalQuality = "complete_balance_v1"

// ForecastAccumulator integrates consecutive valid snapshots by a trapezoid.
// Gaps over MaxGap and invalid snapshots discard the unfinished interval.
// This is interval evidence from sampled power, not a revenue-grade energy meter.
type ForecastAccumulator struct {
	MaxGap                 time.Duration
	previous               *ForecastReading
	start                  time.Time
	covered, houseWS, pvWS float64
	pvValid                bool
}

func (a *ForecastAccumulator) Observe(r ForecastReading) []ForecastInterval {
	if !r.Valid || r.Latest.IsZero() || r.Latest.After(r.At) {
		a.previous = nil
		a.covered = 0
		a.start = time.Time{}
		return nil
	}
	maxGap := a.MaxGap
	if maxGap <= 0 {
		maxGap = 2 * time.Minute
	}
	p := a.previous
	if p != nil && (!r.At.After(p.At) || !r.Latest.After(p.Latest)) {
		a.previous = nil
		a.covered = 0
		a.start = time.Time{}
		return nil
	}
	a.previous = &r
	if p == nil || r.At.Sub(p.At) > maxGap {
		a.start = r.At.UTC().Truncate(15 * time.Minute)
		a.covered = 0
		a.houseWS = 0
		a.pvWS = 0
		a.pvValid = r.PVValid
		return nil
	}
	var out []ForecastInterval
	t := p.At
	span := r.At.Sub(p.At).Seconds()
	for t.Before(r.At) {
		start := t.UTC().Truncate(15 * time.Minute)
		end := start.Add(15 * time.Minute)
		if a.start != start {
			a.start = start
			a.covered = 0
			a.houseWS = 0
			a.pvWS = 0
			a.pvValid = r.PVValid
		}
		until := r.At
		if end.Before(until) {
			until = end
		}
		f0 := t.Sub(p.At).Seconds() / span
		f1 := until.Sub(p.At).Seconds() / span
		seconds := until.Sub(t).Seconds()
		a.houseWS += (p.HouseholdW + (r.HouseholdW-p.HouseholdW)*(f0+f1)/2) * seconds
		a.pvWS += (p.PVW + (r.PVW-p.PVW)*(f0+f1)/2) * seconds
		a.covered += seconds
		a.pvValid = a.pvValid && p.PVValid && r.PVValid
		if until.Equal(end) {
			if math.Abs(a.covered-900) < 0.001 {
				out = append(out, ForecastInterval{Start: start, End: end, HouseholdW: a.houseWS / 900, PVW: a.pvWS / 900, Quality: ForecastIntervalQuality, PVValid: a.pvValid, LatestInputMs: r.Latest.UnixMilli()})
			}
			a.start = end
			a.covered = 0
			a.houseWS = 0
			a.pvWS = 0
			a.pvValid = r.PVValid
		}
		t = until
	}
	return out
}

func (r ForecastReading) Error() error {
	if r.Valid {
		return nil
	}
	return fmt.Errorf("forecast measurement: %s", r.Reason)
}
