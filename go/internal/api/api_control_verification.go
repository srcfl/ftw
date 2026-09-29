package api

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type ControlResponsePoint struct {
	AtMS    int64   `json:"at_ms"`
	DeviceW float64 `json:"device_change_w"`
	SiteW   float64 `json:"adjusted_site_change_w"`
}

// ControlComparison keeps the measured correction and residual visible. It does
// not estimate which household appliance changed or assign a confidence score.
type ControlComparison struct {
	BeforeW        float64                `json:"-"`
	AfterW         float64                `json:"-"`
	BeforeAt       time.Time              `json:"-"`
	AfterAt        time.Time              `json:"-"`
	Reason         string                 `json:"-"`
	DeviceDeltaW   *float64               `json:"-"`
	SiteDeltaW     *float64               `json:"-"`
	OtherDeltaW    *float64               `json:"other_change_w"`
	AdjustedDeltaW *float64               `json:"adjusted_site_change_w"`
	ResidualW      *float64               `json:"unexplained_change_w"`
	ToleranceW     float64                `json:"tolerance_w"`
	Samples        int                    `json:"samples"`
	WindowS        float64                `json:"window_s"`
	MaxSkewMS      *int64                 `json:"max_skew_ms"`
	Trace          []ControlResponsePoint `json:"trace,omitempty"`
}

type alignedControlPoint struct {
	at                  time.Time
	device, grid, other float64
	skew                time.Duration
}

// Compare the shape of grid-minus-other-measured-flows with the device's own
// curve. The command never supplies a measured watt. A before-command baseline
// is the hypothesis to test, not a derived house-load value reused as evidence.
func independentResponse(cmd telemetry.CommandEvidence, meter string, separate bool, after map[string]telemetry.ControlBaseline, now time.Time) ControlComparison {
	out := ControlComparison{}
	stop := func(reason string) ControlComparison { out.Reason = reason; return out }
	if meter == "" {
		return stop("no_site_meter")
	}
	if !separate {
		return stop("independent_source_unknown")
	}
	target := cmd.Driver + ":" + cmd.Kind
	gridKey := meter + ":meter"
	if _, ok := cmd.Baseline[target]; !ok {
		return stop("no_baseline")
	}
	if _, ok := cmd.Baseline[gridKey]; !ok {
		return stop("no_baseline")
	}
	keys := []string{target}
	for k, flow := range after {
		if flow.Kind == telemetry.DerMeter || flow.Kind == telemetry.DerVehicle {
			continue
		}
		if flow.Kind == telemetry.DerEV {
			if _, duplicate := after[flow.Driver+":"+telemetry.DerV2X.String()]; duplicate {
				return stop("measurement_sources_unclear")
			}
		}
		if _, ok := cmd.Baseline[k]; !ok {
			return stop("other_flows_missing")
		}
		if k != target {
			keys = append(keys, k)
		}
	}
	for k, flow := range cmd.Baseline {
		if flow.Kind == telemetry.DerVehicle || (flow.Kind == telemetry.DerMeter && k != gridKey) {
			continue
		}
		current, ok := after[k]
		if !ok || !current.Window.Usable(now) {
			return stop("other_flows_missing")
		}
		if current.Window.First.Before(cmd.Since) {
			return stop("waiting_for_meter")
		}
	}
	sort.Strings(keys)
	before := alignControlPoints(cmd.Baseline, gridKey, target, keys)
	current := alignControlPoints(after, gridKey, target, keys)
	if len(before) < 3 || len(current) < 3 {
		return stop("readings_not_aligned")
	}
	if before[len(before)-1].at.Sub(before[0].at) < 8*time.Second || current[len(current)-1].at.Sub(current[0].at) < 8*time.Second {
		return stop("waiting_for_meter")
	}
	if now.Sub(current[len(current)-1].at) > 10*time.Second {
		return stop("waiting_for_meter")
	}
	values := func(points []alignedControlPoint, f func(alignedControlPoint) float64) []float64 {
		v := make([]float64, len(points))
		for i, p := range points {
			v[i] = f(p)
		}
		return v
	}
	device := func(p alignedControlPoint) float64 { return p.device }
	grid := func(p alignedControlPoint) float64 { return p.grid }
	other := func(p alignedControlPoint) float64 { return p.other }
	adjusted := func(p alignedControlPoint) float64 { return p.grid - p.other }
	residual := func(p alignedControlPoint) float64 { return p.grid - p.other - p.device }
	baseDevice := meanControlPower(values(before, device))
	baseAdjusted := meanControlPower(values(before, adjusted))
	baseResidual := meanControlPower(values(before, residual))
	// A moving household baseline or moving actuator cannot identify this step.
	if spread(values(before, residual)) > 200 || spread(values(before, device)) > 200 {
		return stop("flows_changing")
	}
	deltaDevice := meanControlPower(values(current, device)) - baseDevice
	out.BeforeW, out.AfterW = meanControlPower(values(before, grid)), meanControlPower(values(current, grid))
	out.BeforeAt, out.AfterAt = before[len(before)-1].at, current[len(current)-1].at
	out.DeviceDeltaW = watts(deltaDevice)
	out.SiteDeltaW = watts(out.AfterW - out.BeforeW)
	out.OtherDeltaW = watts(meanControlPower(values(current, other)) - meanControlPower(values(before, other)))
	out.AdjustedDeltaW = watts(meanControlPower(values(current, adjusted)) - baseAdjusted)
	out.ResidualW = watts(meanControlPower(values(current, residual)) - baseResidual)
	out.ToleranceW = math.Max(150, math.Abs(deltaDevice)*0.15)
	out.Samples = len(current)
	out.WindowS = current[len(current)-1].at.Sub(current[0].at).Seconds()
	maxSkewMS := int64(0)
	out.MaxSkewMS = &maxSkewMS
	for _, p := range before {
		if p.skew.Milliseconds() > maxSkewMS {
			maxSkewMS = p.skew.Milliseconds()
		}
	}
	// All points must support the same change. Means alone could hide a household
	// load starting and stopping inside the window. Do not widen tolerance to fit it.
	matches := true
	for _, p := range current {
		out.Trace = append(out.Trace, ControlResponsePoint{p.at.UnixMilli(), p.device - baseDevice, p.grid - p.other - baseAdjusted})
		if p.skew.Milliseconds() > maxSkewMS {
			maxSkewMS = p.skew.Milliseconds()
		}
		if math.Abs(residual(p)-baseResidual) > out.ToleranceW {
			matches = false
		}
	}
	// Negative unmetered consumption beyond measurement tolerance exposes a
	// missing/duplicated source or incompatible boundaries; it cannot prove control.
	if baseResidual < -200 {
		return stop("energy_balance_conflict")
	}
	for _, p := range current {
		if residual(p) < -200 {
			return stop("energy_balance_conflict")
		}
	}
	if math.Abs(deltaDevice) < 500 {
		return stop("no_clear_change")
	}
	if !matches {
		return stop("site_change_differs")
	}
	return stop("confirmed")
}

// Use only distinct real samples. Never interpolate a missing transition or
// reuse a slow source to manufacture a dense curve. Bound skew across ALL flows,
// not just target and grid, because moving solar makes stale alignment unsafe.
func alignControlPoints(series map[string]telemetry.ControlBaseline, gridKey, target string, keys []string) []alignedControlPoint {
	var out []alignedControlPoint
	used := map[string]time.Time{}
	for _, g := range series[gridKey].Points {
		p := alignedControlPoint{at: g.At, grid: g.PowerW}
		first, last := g.At, g.At
		chosen := map[string]telemetry.ControlObservation{}
		for _, k := range keys {
			var nearest telemetry.ControlObservation
			distance := 3 * time.Second
			for _, v := range series[k].Points {
				d := v.At.Sub(g.At)
				if d < 0 {
					d = -d
				}
				if d < distance {
					nearest, distance = v, d
				}
			}
			if nearest.At.IsZero() || distance > 2*time.Second || !nearest.At.After(used[k]) {
				break
			}
			chosen[k] = nearest
			if nearest.At.Before(first) {
				first = nearest.At
			}
			if nearest.At.After(last) {
				last = nearest.At
			}
			if k == target {
				p.device = nearest.PowerW
			} else {
				p.other += nearest.PowerW
			}
		}
		if len(chosen) != len(keys) || last.Sub(first) > 2*time.Second {
			continue
		}
		p.skew = last.Sub(first)
		out = append(out, p)
		for k, v := range chosen {
			used[k] = v.At
		}
	}
	return out
}
func meanControlPower(values []float64) float64 {
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func spread(values []float64) float64 {
	lo, hi := values[0], values[0]
	for _, v := range values {
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	return hi - lo
}

func (s *Server) separateMeterSource(driver, meter string) bool {
	if s.deps.Tel == nil {
		return false
	}
	reading := s.deps.Tel.Get(meter, telemetry.DerMeter)
	if reading == nil {
		return false
	}
	var data struct {
		PowerOrigin string `json:"power_origin"`
	}
	if json.Unmarshal(reading.Data, &data) != nil || data.PowerOrigin == "derived" {
		return false
	}
	// A driver may relay an external physical meter. Its documented sensor
	// origin matters here, not whether it shares the inverter's connection.
	if driver == meter {
		return data.PowerOrigin == "external_meter"
	}
	if s.deps.Registry == nil {
		return false
	}
	a, b := s.runningDriverDevice(driver), s.runningDriverDevice(meter)
	if a.Serial == "" && s.deps.OCPPChargers != nil {
		if c, ok := s.deps.OCPPChargers()[driver]; ok {
			a.Serial = c.Serial
			a.DeviceID = c.Vendor + ":" + c.Serial
		}
	}
	return a.Serial != "" && b.Serial != "" && a.Serial != b.Serial && a.DeviceID != "" && b.DeviceID != "" && a.DeviceID != b.DeviceID
}

func (s *Server) controlSourcesComplete(flows map[string]telemetry.ControlBaseline, now time.Time) bool {
	return s.controlSourceIssue(flows, now) == ""
}

func (s *Server) controlSourceIssue(flows map[string]telemetry.ControlBaseline, now time.Time) string {
	if s.deps.SiteMeasurementSources == nil {
		return "site_inventory_unavailable"
	}
	opts := s.deps.SiteMeasurementSources()
	if opts.HouseholdInvalidReason != "" {
		return opts.HouseholdInvalidReason
	}
	if opts.PVInvalidReason != "" {
		return opts.PVInvalidReason
	}
	seen := map[string]bool{}
	for _, f := range opts.ExpectedFlows {
		if f.DerType == telemetry.DerVehicle || f.DerType == telemetry.DerMeter {
			continue
		}
		if f.FlowID != "" {
			if seen[f.FlowID] {
				return "duplicate:" + f.Driver + ":" + f.DerType.String()
			}
			seen[f.FlowID] = true
		}
		r, ok := flows[f.Driver+":"+f.DerType.String()]
		if !ok || !r.Window.Usable(now) {
			return "missing_fresh_power:" + f.Driver + ":" + f.DerType.String()
		}
	}
	for _, f := range flows {
		if f.Kind == telemetry.DerMeter || f.Kind == telemetry.DerVehicle {
			continue
		}
		if !s.deps.Tel.DriverHealth(f.Driver).TelemetryLive() {
			return "offline:" + f.Driver + ":" + f.Kind.String()
		}
	}
	return ""
}
