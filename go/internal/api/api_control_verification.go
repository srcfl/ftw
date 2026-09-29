package api

import (
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Tier 2 compares a measured change, never a command with an absolute grid
// reading. Derived house load would make the check circular and is not used.
// This is corroboration under steady monitored flows, not proof of exclusive
// ownership or of what every unmetered household appliance did.
func independentResponse(cmd telemetry.CommandEvidence, meter string, separate bool, after map[string]telemetry.ControlBaseline, now time.Time) (string, *float64, *float64) {
	if meter == "" {
		return "no_site_meter", nil, nil
	}
	if cmd.Driver == meter || !separate {
		return "independent_source_unknown", nil, nil
	}
	beforeDevice, okD := cmd.Baseline[cmd.Driver+":"+cmd.Kind]
	beforeMeter, okM := cmd.Baseline[meter+":meter"]
	device, okAD := after[cmd.Driver+":"+cmd.Kind]
	grid, okAM := after[meter+":meter"]
	if !okD || !okM {
		return "no_baseline", nil, nil
	}
	if !okAD || !okAM || !device.Window.Usable(now) || !grid.Window.Usable(now) {
		return "waiting_for_meter", nil, nil
	}
	if grid.Window.First.Before(cmd.Since) || device.Window.First.Before(cmd.Since) {
		return "waiting_for_meter", nil, nil
	}
	if math.Abs(grid.Window.Last.Sub(device.Window.Last).Seconds()) > 5 {
		return "readings_not_aligned", nil, nil
	}
	stable := func(w telemetry.ControlWindow) bool { return w.MaxW-w.MinW <= 200 }
	if !stable(beforeDevice.Window) || !stable(beforeMeter.Window) || !stable(device.Window) || !stable(grid.Window) {
		return "flows_changing", nil, nil
	}
	deltaDevice := device.Window.MeanW - beforeDevice.Window.MeanW
	deltaGrid := grid.Window.MeanW - beforeMeter.Window.MeanW
	if math.Abs(deltaDevice) < 500 {
		return "no_clear_change", watts(deltaDevice), watts(deltaGrid)
	}
	// Every other monitored flow needs a stable before/after window. A missing
	// stream, a new device or a changing PV/battery/EV invalidates attribution.
	otherChange := 0.0
	for k, other := range after {
		if other.Kind == telemetry.DerMeter || other.Kind == telemetry.DerVehicle || k == cmd.Driver+":"+cmd.Kind {
			continue
		}
		base, ok := cmd.Baseline[k]
		if !ok || !other.Window.Usable(now) || !stable(base.Window) || !stable(other.Window) || math.Abs(other.Window.MeanW-base.Window.MeanW) > 100 {
			return "other_flows_changed", watts(deltaDevice), watts(deltaGrid)
		}
		otherChange += math.Abs(other.Window.MeanW - base.Window.MeanW)
	}
	if otherChange > 150 {
		return "other_flows_changed", watts(deltaDevice), watts(deltaGrid)
	}
	for k, base := range cmd.Baseline {
		if base.Kind == telemetry.DerMeter || base.Kind == telemetry.DerVehicle {
			continue
		}
		if _, ok := after[k]; !ok {
			return "other_flows_changed", watts(deltaDevice), watts(deltaGrid)
		}
	}
	if math.Abs(deltaGrid-deltaDevice) > math.Max(250, math.Abs(deltaDevice)*0.15) {
		return "site_change_differs", watts(deltaDevice), watts(deltaGrid)
	}
	return "confirmed", watts(deltaDevice), watts(deltaGrid)
}

func (s *Server) separateMeterSource(driver, meter string) bool {
	if driver == meter || s.deps.Registry == nil {
		return false
	}
	a, b := s.runningDriverDevice(driver), s.runningDriverDevice(meter)
	if a.Serial == "" && s.deps.OCPPChargers != nil {
		if c, ok := s.deps.OCPPChargers()[driver]; ok {
			a.Serial = c.Serial
			a.DeviceID = c.Vendor + ":" + c.Serial
		}
	}
	// Endpoint or configured name alone does not establish two physical meters.
	return a.Serial != "" && b.Serial != "" && a.Serial != b.Serial && a.DeviceID != "" && b.DeviceID != "" && a.DeviceID != b.DeviceID
}
