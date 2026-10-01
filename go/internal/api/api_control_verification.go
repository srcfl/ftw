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
	BeforeW         float64                `json:"-"`
	AfterW          float64                `json:"-"`
	GridBeforeW     *float64               `json:"grid_before_w,omitempty"`
	GridAfterW      *float64               `json:"grid_after_w,omitempty"`
	BeforeAt        time.Time              `json:"-"`
	AfterAt         time.Time              `json:"-"`
	Reason          string                 `json:"-"`
	DeviceDeltaW    *float64               `json:"device_change_w"`
	SiteDeltaW      *float64               `json:"grid_change_w"`
	OtherDeltaW     *float64               `json:"other_change_w"`
	AdjustedDeltaW  *float64               `json:"adjusted_site_change_w"`
	ResidualW       *float64               `json:"unexplained_change_w"`
	ToleranceW      float64                `json:"tolerance_w"`
	Samples         int                    `json:"samples"`
	WindowS         float64                `json:"window_s"`
	MaxSkewMS       *int64                 `json:"max_skew_ms"`
	UnmeasuredFlows []string               `json:"unmeasured_flows,omitempty"`
	Trace           []ControlResponsePoint `json:"trace,omitempty"`
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
	// Only the commanded function and site meter are required measurements.
	// Every other flow is a correction when both windows support it. Otherwise
	// its real effect stays in the residual; missing data never means zero power.
	for _, k := range []string{target, gridKey} {
		current, ok := after[k]
		if !ok || !current.Window.Usable(now) {
			return stop("waiting_for_meter")
		}
		if current.Window.First.Before(cmd.Since) {
			return stop("waiting_for_meter")
		}
	}
	keys := []string{target}
	before := alignControlPoints(cmd.Baseline, gridKey, target, keys)
	current := alignControlPoints(after, gridKey, target, keys)
	if len(before) < 3 || len(current) < 3 {
		return stop("readings_not_aligned")
	}
	if !controlSpanReady(before, cmd.Since) || !controlSpanReady(current, now) {
		return stop("waiting_for_meter")
	}
	candidates := map[string]bool{}
	for _, flows := range []map[string]telemetry.ControlBaseline{cmd.Baseline, after} {
		for k, flow := range flows {
			if k != target && flow.Kind != telemetry.DerMeter && flow.Kind != telemetry.DerVehicle {
				candidates[k] = true
			}
		}
	}
	ordered := make([]string, 0, len(candidates))
	for k := range candidates {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	for _, k := range ordered {
		old, next := cmd.Baseline[k], after[k]
		trial := append(append([]string(nil), keys...), k)
		b := alignControlPoints(cmd.Baseline, gridKey, target, trial)
		a := alignControlPoints(after, gridKey, target, trial)
		// Select by sample availability and timing only, never by whether a
		// correction makes the power curves agree. Use one set on both sides.
		if !old.Window.Usable(cmd.Since) || !next.Window.Usable(now) || next.Window.First.Before(cmd.Since) || !controlSpanReady(b, cmd.Since) || !controlSpanReady(a, now) {
			out.UnmeasuredFlows = append(out.UnmeasuredFlows, k)
			continue
		}
		keys, before, current = trial, b, a
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
	out.GridBeforeW, out.GridAfterW = watts(out.BeforeW), watts(out.AfterW)
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
	// Unmeasured background may include a disconnected battery or solar source,
	// so its absolute sign cannot establish an impossible energy balance. Its
	// change must still agree at every point; a load pulse cannot be averaged out.
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

// Source inventory helps avoid double counting; it does not require every
// device to be online. Control proof belongs to one function at a time.
func (s *Server) controlResponse(cmd telemetry.CommandEvidence, meter string, after map[string]telemetry.ControlBaseline, now time.Time) ControlComparison {
	opts := telemetry.ForecastOptions{}
	if s.deps.SiteMeasurementSources != nil {
		opts = s.deps.SiteMeasurementSources()
	}
	counts := map[string]int{}
	for _, f := range opts.ExpectedFlows {
		if f.FlowID != "" {
			counts[f.FlowID]++
		}
		k := f.Driver + ":" + f.DerType.String()
		if _, exists := after[k]; !exists {
			after[k] = telemetry.ControlBaseline{Driver: f.Driver, Kind: f.DerType}
		}
	}
	duplicates := map[string]bool{}
	for _, f := range opts.ExpectedFlows {
		if f.FlowID != "" && counts[f.FlowID] > 1 {
			duplicates[f.Driver+":"+f.DerType.String()] = true
		}
	}
	target := cmd.Driver + ":" + cmd.Kind
	for k, f := range after {
		if k == target || f.Kind == telemetry.DerMeter || f.Kind == telemetry.DerVehicle {
			continue
		}
		_, ev := after[f.Driver+":ev"]
		_, v2x := after[f.Driver+":v2x_charger"]
		ambiguous := duplicates[k] || (ev && v2x && (f.Kind == telemetry.DerEV || f.Kind == telemetry.DerV2X))
		unidentified := opts.HouseholdInvalidReason != "" || f.Kind == telemetry.DerPV && opts.PVInvalidReason != ""
		h := s.deps.Tel.DriverHealth(f.Driver)
		if h == nil || !h.TelemetryLive() || h.DeviceFault || ambiguous || unidentified {
			f.Window, f.Points = telemetry.ControlWindow{}, nil
			after[k] = f
		}
	}
	return independentResponse(cmd, meter, s.separateMeterSource(cmd.Driver, meter), after, now)
}

func controlSpanReady(points []alignedControlPoint, now time.Time) bool {
	return len(points) >= 3 && points[len(points)-1].at.Sub(points[0].at) >= 8*time.Second && !points[len(points)-1].at.After(now) && now.Sub(points[len(points)-1].at) <= 10*time.Second
}
