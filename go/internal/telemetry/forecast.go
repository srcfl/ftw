package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
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
	// HistoryRevision breaks receipt integration at a saved settings change.
	// It does not alter forecast qualification or live power.
	HistoryRevision int64 `json:",omitempty"`
	// HouseholdInvalidReason qualifies unresolved site topology without invalidating independent PV observations.
	HouseholdInvalidReason string
	PVInvalidReason        string
	ExpectedFlows          []ForecastFlow
	// OptionalFlows are flows History does not require but a training label
	// does, such as PV on a hybrid inverter that may have no panels. Omitted
	// when empty, so forecast revisions keep their stored identity.
	OptionalFlows      []ForecastFlow `json:",omitempty"`
	MaxAge             time.Duration
	MaxSkew            time.Duration
	UseWatchdogTimeout bool `json:",omitempty"`
	// Live display may use the driver's measured site-side control power
	// (for example Pixii AC instead of battery DC). Raw history stays separate.
	UseControlPower bool `json:",omitempty"`
	// PVDark says how dark it is at the site at a time. Live Home use counts
	// an unknown PV source as 0 W after sunset. Nil, or no site position,
	// infers nothing. Not part of a forecast revision's identity.
	PVDark func(time.Time) Darkness `json:"-"`
}

// Darkness is the sun's state at the site, for unknown PV.
type Darkness int

const (
	// DarknessUnknown: daylight, or no site position.
	DarknessUnknown Darkness = iota
	// DarknessTwilight: after sunset, before civil dusk. Diffuse light only.
	DarknessTwilight
	// DarknessNight: past civil dusk. PV produces nothing.
	DarknessNight
)

// Live Home use quality. Measured: every flow read, each by its driver's latest
// poll. Estimated: the site meter is read and a flow is left out, assumed or
// held after a failed read. Unknown: no usable site meter.
const (
	LoadMeasured  = "measured"
	LoadEstimated = "estimated"
	LoadUnknown   = "unknown"
)

// ForecastReading is one coherent snapshot, in site signs and raw watts.
// Valid describes the household balance. PVValid only describes the PV sources.
// Neither flag claims that the instantaneous sample covers a time interval.
type ForecastSource struct {
	Flow   ForecastFlow
	PowerW *float64
	SoC    *float64
	// Dark: an unread PV source after sunset, shown as 0 W. Not a reading.
	Dark bool
}

type ForecastReading struct {
	Known                                       map[DerType]bool
	Sources                                     map[string]ForecastSource
	At, Earliest, Latest, PVEarliest, PVLatest  time.Time
	GridW, PVW, BatteryW, EVW, V2XW, HouseholdW float64
	Valid, PVValid                              bool
	Reason, PVReason                            string
	// Live Home use, separate from history and forecast qualification
	// (Valid/Reason). Known whenever the site meter is read; flows that cannot
	// be read are left out and LoadQuality says so. PVLiveKnown: every PV
	// source is read, or dark.
	LoadW                   float64
	LoadKnown, PVLiveKnown  bool
	LoadQuality, LoadReason string
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

// LiveMeasurementNow qualifies the current site balance for every live client.
// Display power uses source-side AC/control observations and each driver's
// watchdog cadence. Current sources need not report at the same time. Forecast
// training and stored history keep their source-alignment policy.
func (s *Store) LiveMeasurementNow(siteMeter string, opts ForecastOptions) ForecastReading {
	opts.UseWatchdogTimeout = true
	opts.UseControlPower = true
	if s != nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
	}
	return s.measurementLocked(time.Now(), siteMeter, opts, false)
}

// SourceFailure reports whether a reading's Reason names one failed source.
// That source is already unknown in Known; the balance of the others still
// holds. Configuration and duplicate-flow reasons are not source failures.
// Keep in step with the reasons measurementLocked gives.
func SourceFailure(reason string) bool {
	for _, prefix := range []string{"missing:", "offline:", "unknown_power:", "nonfinite:", "stale:", "future:", "invalid_sign:"} {
		if strings.HasPrefix(reason, prefix) {
			return true
		}
	}
	return reason == "nonfinite_pv_sum"
}

func (s *Store) forecastMeasurementLocked(now time.Time, siteMeter string, opts ForecastOptions) ForecastReading {
	return s.measurementLocked(now, siteMeter, opts, true)
}

func (s *Store) measurementLocked(now time.Time, siteMeter string, opts ForecastOptions, alignSources bool) ForecastReading {
	out := ForecastReading{At: now, Valid: true, PVValid: true, Known: map[DerType]bool{DerMeter: true, DerPV: true, DerBattery: true, DerEV: true, DerV2X: true}, Sources: map[string]ForecastSource{}}
	if s == nil {
		for kind := range out.Known {
			out.Known[kind] = false
		}
		out.Valid = false
		out.PVValid = false
		out.Reason = "no_telemetry"
		out.PVReason = out.Reason
		out.LoadQuality, out.LoadReason = LoadUnknown, out.Reason
		return out
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = 90 * time.Second
	}
	if opts.MaxSkew <= 0 {
		opts.MaxSkew = 30 * time.Second
	}
	liveIssue := ""
	note := func(reason string) {
		if liveIssue == "" {
			liveIssue = reason
		}
	}
	pvLiveKnown := true
	darkness := DarknessUnknown
	if opts.PVDark != nil && !alignSources {
		darkness = opts.PVDark(now)
	}
	flows := map[string]ForecastFlow{}
	key := func(f ForecastFlow) string { return f.Driver + ":" + f.DerType.String() }
	failQuiet := func(reason string, pv bool) {
		if out.Valid {
			out.Valid = false
			out.Reason = reason
		}
		if pv && out.PVValid {
			out.PVValid = false
			out.PVReason = reason
		}
	}
	fail := func(reason string, pv bool) {
		note(reason)
		failQuiet(reason, pv)
	}
	if opts.PVInvalidReason != "" {
		out.PVValid = false
		out.PVReason = opts.PVInvalidReason
	}
	if opts.PVInvalidReason != "" || opts.HouseholdInvalidReason == "configured_pv_without_measurement_source" {
		out.Known[DerPV] = false
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
	// Kinds configured twice: live Home use leaves them out, never subtracts twice.
	duplicate := map[DerType]bool{}
	var duplicateW float64
	ids := map[string]ForecastFlow{}
	for _, f := range opts.ExpectedFlows {
		if f.DerType == DerVehicle || (f.DerType == DerMeter && f.Driver != siteMeter) {
			continue
		}
		if f.FlowID != "" {
			if old, ok := ids[f.FlowID]; ok {
				fail("duplicate_flow:"+f.FlowID, f.DerType == DerPV || old.DerType == DerPV)
				out.Known[f.DerType], out.Known[old.DerType] = false, false
				duplicate[f.DerType], duplicate[old.DerType] = true, true
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
				out.Known[DerEV], out.Known[DerV2X] = false, false
				duplicate[DerEV], duplicate[DerV2X] = true, true
			}
		}
	}
	// Configuration and duplicate flows leave PV unknown for live use too.
	if !out.Known[DerPV] {
		pvLiveKnown = false
	}
	meterIssue := ""
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
		source := ForecastSource{Flow: f}
		out.Sources[k] = source
		maxAge := opts.MaxAge
		if opts.UseWatchdogTimeout && s.health[f.Driver] != nil && s.health[f.Driver].WatchdogTimeoutOverride > 0 {
			maxAge = s.health[f.Driver].WatchdogTimeoutOverride
		}
		reason := ""
		var powerW float64
		var powerAt time.Time
		powerQualified := true
		if r != nil {
			powerW, powerAt = r.RawW, r.UpdatedAt
			if opts.UseControlPower {
				p, known := ControlPowerObservation(r.RawW, r.Data, r.UpdatedAt)
				powerQualified = known
				powerW, powerAt = p.PowerW, p.Seen()
			} else {
				var data struct {
					Power json.RawMessage `json:"forecast_power"`
				}
				if json.Unmarshal(r.Data, &data) == nil && len(data.Power) > 0 {
					var sample ForecastPowerSample
					powerQualified = json.Unmarshal(data.Power, &sample) == nil && sample.Version == 1 && sample.Known && sample.MeasuredAtMS > 0 && sample.ReceivedAtMS >= sample.MeasuredAtMS && sample.ReceivedAtMS <= now.UnixMilli()
					powerW, powerAt = sample.Watts, time.UnixMilli(sample.MeasuredAtMS)
				}
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
		case powerAt.IsZero() || now.Sub(powerAt) > maxAge:
			reason = "stale:"
		case powerAt.After(now):
			reason = "future:"
		case pv && powerW > 0:
			reason = "invalid_sign:"
		}
		if reason != "" {
			switch {
			case pv && darkness != DarknessUnknown && pvUnread(reason):
				// Asleep after sunset: 0 W for live use. In twilight Home
				// use is an estimate. History and forecasts keep their own
				// evidence through Valid/Known.
				if darkness == DarknessTwilight {
					note("pv_twilight:" + k)
				}
				failQuiet(reason+k, pv)
				out.Known[f.DerType] = false
				zero := 0.0
				source.PowerW, source.Dark = &zero, true
				out.Sources[k] = source
				continue
			case pv:
				pvLiveKnown = false
			case f.DerType == DerMeter:
				meterIssue = reason + k
			}
			fail(reason+k, pv)
			out.Known[f.DerType] = false
			continue
		}
		if s.health[f.Driver].LastReadFailed() {
			// Held from an earlier read until the age limit: shown, not
			// measured. History and forecasts keep their own policy.
			note("read_failed:" + k)
		}
		source.PowerW = &powerW
		if r.SoC != nil && !math.IsNaN(*r.SoC) && !math.IsInf(*r.SoC, 0) && *r.SoC >= 0 && *r.SoC <= 1 {
			soc := *r.SoC
			source.SoC = &soc
		}
		out.Sources[k] = source
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
		if duplicate[f.DerType] {
			duplicateW += powerW
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
	if alignSources && out.Latest.Sub(out.Earliest) > opts.MaxSkew {
		fail("time_skew", false)
	}
	out.PVEarliest, out.PVLatest = pvFirst, pvLast
	if alignSources && pvLast.Sub(pvFirst) > opts.MaxSkew {
		fail("pv_time_skew", true)
	}
	if pvCount == 0 {
		out.PVValid = false
		out.PVReason = "no_pv_source"
	}
	if math.IsNaN(out.PVW) || math.IsInf(out.PVW, 0) {
		fail("nonfinite_pv_sum", true)
	}
	for kind, w := range map[DerType]float64{DerMeter: out.GridW, DerPV: out.PVW, DerBattery: out.BatteryW, DerEV: out.EVW, DerV2X: out.V2XW} {
		if math.IsNaN(w) || math.IsInf(w, 0) {
			out.Known[kind] = false
		}
	}
	out.HouseholdW = out.GridW - out.PVW - out.BatteryW - out.EVW - out.V2XW
	if math.IsNaN(out.HouseholdW) || math.IsInf(out.HouseholdW, 0) || out.HouseholdW < 0 {
		fail("invalid_balance", false)
	}
	out.PVLiveKnown = pvLiveKnown && pvCount > 0 && !math.IsNaN(out.PVW) && !math.IsInf(out.PVW, 0)
	out.setLiveLoad(out.HouseholdW+duplicateW, liveIssue, meterIssue)
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
	// Quarter-hour boundaries use wall time. Use that clock for gaps and
	// interpolation too; a post-boot clock correction can leave monotonic
	// elapsed time short while wall time jumps across many quarters.
	r.At = r.At.Round(0)
	r.Latest = r.Latest.Round(0)
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

// setLiveLoad derives live Home use from the site meter and every flow that
// was read. A missing battery, EV, V2X or daytime PV is left out, as Core did
// before history qualification, and the result is an estimate. It is unknown
// only without a usable site meter. Quality is a hint and never hides it.
func (out *ForecastReading) setLiveLoad(w float64, issue, meterIssue string) {
	if !out.Known[DerMeter] || meterIssue != "" {
		if meterIssue == "" {
			meterIssue = "site_meter_unknown"
		}
		out.LoadKnown, out.LoadQuality, out.LoadReason = false, LoadUnknown, meterIssue
		return
	}
	if math.IsNaN(w) || math.IsInf(w, 0) {
		out.LoadKnown, out.LoadQuality, out.LoadReason = false, LoadUnknown, "nonfinite_balance"
		return
	}
	// Shown as is. A negative balance is a hint of production Core cannot
	// see; zero or blank would hide it.
	out.LoadKnown, out.LoadW = true, w
	if w < 0 && issue == "" {
		issue = "invalid_balance"
	}
	if issue == "" {
		out.LoadQuality = LoadMeasured
		return
	}
	out.LoadQuality, out.LoadReason = LoadEstimated, issue
}

// pvUnread: the PV source gave no usable reading. Only then can darkness stand
// in for it. A malformed reading (wrong sign, non-finite, future) stays a fault.
func pvUnread(reason string) bool {
	switch reason {
	case "missing:", "offline:", "stale:", "unknown_power:":
		return true
	}
	return false
}
