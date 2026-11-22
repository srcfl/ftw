package api

import (
	"encoding/json"
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/battery"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A battery that gives 4.3 kW of a 5 kW order because that is all it can do
// now is limited, not out of control. This file judges a battery's shortfall
// against what it can do now: what its driver reports, and the limits it has
// shown before (battery.Plateaus). It changes the verdict and nothing else;
// dispatch, fuse protection, clamps and commands never read it.

// batteryNow is what a battery's driver reports it can do now
// (srcfl/device-drivers docs/WRITING-A-DRIVER.md, "What a battery can do
// now"). A read that failed leaves its field out, so a missing field
// changes nothing.
type batteryNow struct {
	AvailableChargeW    *float64 `json:"available_charge_W"`
	AvailableDischargeW *float64 `json:"available_discharge_W"`
	TemperatureC        *float64 `json:"temperature_C"`
	LegacyTempC         *float64 `json:"temp_c"`
	DeviceState         string   `json:"device_state"`
	A                   *float64 `json:"A"`
	LegacyA             *float64 `json:"a"`
	V                   *float64 `json:"V"`
	LegacyV             *float64 `json:"v"`
}

func readBatteryNow(data json.RawMessage) batteryNow {
	var b batteryNow
	if len(data) > 0 {
		_ = json.Unmarshal(data, &b)
	}
	return b
}

// The driver guide drops a temperature outside -40 to 100 °C and a power
// outside 0 to 100 kW; Core does the same.
func (b batteryNow) tempC() *float64 {
	t := b.TemperatureC
	if t == nil {
		t = b.LegacyTempC
	}
	if t == nil || !finiteW(*t) || *t < -40 || *t > 100 {
		return nil
	}
	return t
}

func (b batteryNow) available(charge bool) *float64 {
	w := b.AvailableDischargeW
	if charge {
		w = b.AvailableChargeW
	}
	if w == nil || !finiteW(*w) || *w < 0 || *w > 100_000 {
		return nil
	}
	return w
}

func (b batteryNow) currentA() *float64 {
	a := b.A
	if a == nil {
		a = b.LegacyA
	}
	if a == nil || !finiteW(*a) || *a == 0 {
		return nil
	}
	return watts(math.Abs(*a))
}

func (b batteryNow) voltageV() *float64 {
	v := b.V
	if v == nil {
		v = b.LegacyV
	}
	if v == nil || !finiteW(*v) || *v <= 0 {
		return nil
	}
	return v
}

// The device says it is not running, so it cannot follow an order.
func (b batteryNow) standby() bool { return b.DeviceState == "standby" || b.DeviceState == "starting" }

// explainBatteryShortfall runs after every other verdict step. It relabels
// only a battery's settled shortfall in the right direction
// (power_below_target) or its zero response (no_power_response), from fresh
// readings of an accepted order:
//   - device_state standby or starting keeps the warning as device_standby,
//     so the words can say why;
//   - a reported available_*_W below the order that the battery delivers,
//     within the control tolerance, is limited / battery_limit;
//   - a trusted learned plateau the shortfall sits on is limited /
//     learned_limit.
//
// It also leaves the sample the limit learner takes on the journal's cadence.
// Wrong direction, power above the order or while idle, a changed setpoint,
// stale readings, faults and every Core stop keep their own reasons and are
// never sampled.
func (s *Server) explainBatteryShortfall(f *ControlFeedback, data json.RawMessage, meter, blocked string, now time.Time) {
	if f.Kind != "battery" || f.SentW == nil || f.ActualW == nil || f.Reason != "power_below_target" && f.Reason != "no_power_response" {
		return
	}
	b := readBatteryNow(data)
	sent, actual := *f.SentW, *f.ActualW
	charge := sent > 0
	if b.standby() {
		f.State, f.Reason, f.Severity = "warning", "device_standby", "warning"
		return
	}
	if f.Reason == "power_below_target" && math.Abs(actual) >= battery.PlateauMinShortfallW {
		grid, gridAt := s.siteGridW(f.Driver, meter, blocked, charge, now)
		f.shortfall = &battery.ShortfallSample{At: now, Charge: charge, OrderW: math.Abs(sent), BatteryW: math.Abs(actual),
			BatteryA: b.currentA(), GridW: grid, GridAt: gridAt, TempC: b.tempC()}
	}
	// 0 means the device allows nothing now, as when charging below 0 °C.
	if limit := b.available(charge); limit != nil && *limit < math.Abs(sent) &&
		math.Abs(math.Abs(actual)-*limit) <= telemetry.ControlToleranceW(*limit) {
		f.State, f.Reason, f.Severity = "limited", "battery_limit", "info"
		f.LimitW, f.LimitSource, f.LimitKind = watts(*limit), "device", "battery_reported"
		return
	}
	if f.shortfall == nil {
		return
	}
	known, ok := s.learnedLimit(f.Driver, *f.shortfall)
	if !ok {
		return
	}
	f.State, f.Reason, f.Severity = "limited", "learned_limit", "info"
	f.LimitSource, f.LimitKind, f.LimitSeen, f.LimitSinceMs = "learned", known.Kind, known.Seen, known.SinceMs
	// Limits are positive amounts in the order's direction. A current limit
	// becomes watts at the pack's voltage now; without one it has no watts.
	inW := func(value float64) *float64 {
		if known.Kind != battery.LimitBatteryCurrent {
			return watts(value)
		}
		if v := b.voltageV(); v != nil {
			return watts(value * *v)
		}
		return nil
	}
	f.LimitW = inW(known.Value)
	if known.Kind == battery.LimitBatteryCurrent {
		f.LimitA = watts(known.Value)
	}
	if known.Earlier != nil {
		f.LimitEarlierW = inW(known.Earlier.Value)
	}
	// Under a grid limit the battery has the limit less what the rest of
	// the site takes or gives now.
	if g := f.shortfall.GridW; g != nil && (known.Kind == battery.LimitGridImport || known.Kind == battery.LimitGridExport) {
		f.LimitRoomW = watts(math.Max(0, known.Value-*g+math.Abs(actual)))
	}
}

func (s *Server) learnedLimit(driver string, sample battery.ShortfallSample) (battery.Plateau, bool) {
	if s.deps.Models == nil || s.deps.ModelsMu == nil {
		return battery.Plateau{}, false
	}
	s.deps.ModelsMu.Lock()
	defer s.deps.ModelsMu.Unlock()
	m := s.deps.Models[driver]
	if m == nil {
		return battery.Plateau{}, false
	}
	return m.Plateaus.Explain(sample)
}

// siteGridMaxAge is how old a site meter reading may be for a shortfall
// sample: three journal samples.
const siteGridMaxAge = 15 * time.Second

// siteGridW is the grid power a battery's shortfall is judged against:
// import while charging, export while discharging, as positive W. It comes
// only from a site meter Core trusts for site evidence: a sensor separate
// from the battery, live, without a fault, fresh, and while dispatch is free.
// It also gives when the meter measured it, by its own clock where it
// reports one; the learner asks a run's readings to show a live meter.
func (s *Server) siteGridW(driver, meter, blocked string, charge bool, now time.Time) (*float64, time.Time) {
	if meter == "" || blocked != "" || !s.separateMeterSource(driver, meter) {
		return nil, time.Time{}
	}
	if h := s.deps.Tel.DriverHealth(meter); h == nil || !h.TelemetryLive() || h.DeviceFault {
		return nil, time.Time{}
	}
	rd := s.deps.Tel.Get(meter, telemetry.DerMeter)
	if rd == nil {
		return nil, time.Time{}
	}
	obs, ok := telemetry.ControlPowerObservation(rd.RawW, rd.Data, rd.UpdatedAt)
	if !ok || now.Sub(obs.Seen()) > siteGridMaxAge {
		return nil, time.Time{}
	}
	w := obs.PowerW
	if !charge {
		w = -w
	}
	if w <= 0 {
		return nil, time.Time{}
	}
	return &w, obs.At
}

// learnBatteryLimits feeds each battery's limit learner one sample. The
// control journal calls it every 5 s, so how often a client reads the status
// never changes what is learned.
func (s *Server) learnBatteryLimits(rows []ControlFeedback, now time.Time) {
	if s.deps.Models == nil || s.deps.ModelsMu == nil {
		return
	}
	// Self-tune sends its own test steps; what a battery does under them
	// is not its normal.
	tuning := s.deps.SelfTune != nil && s.deps.SelfTune.Status().Active
	samples := map[string]*battery.ShortfallSample{}
	for i := range rows {
		if rows[i].Kind == "battery" && rows[i].shortfall != nil && !tuning {
			samples[rows[i].Driver] = rows[i].shortfall
		}
	}
	s.deps.ModelsMu.Lock()
	defer s.deps.ModelsMu.Unlock()
	for name, m := range s.deps.Models {
		if m == nil {
			continue
		}
		if m.Plateaus == nil {
			if samples[name] == nil {
				continue
			}
			m.Plateaus = &battery.Plateaus{}
		}
		m.Plateaus.Tick(now, samples[name])
	}
}

func finiteW(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
