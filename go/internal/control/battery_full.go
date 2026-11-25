package control

import (
	"math"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A one percentage point gap prevents repeated starts at the charge ceiling.
const batteryChargeHysteresisSoC = 0.01

// BatteryChargeResumeSoC uses the same ceiling as dispatch. Callers hold the
// control lock, as they do for BatteryChargePaused.
func (s *State) BatteryChargeResumeSoC(driver string) float64 {
	return math.Max(0, s.chargeCeiling(driver)-batteryChargeHysteresisSoC)
}

// chargeCeiling is the lower of the configured ceiling and the one the
// battery's own device last reported (telemetry.BatterySoCCeiling). A Pixii
// stops charging at its own limit, so an order above it goes undelivered.
func (s *State) chargeCeiling(driver string) float64 {
	ceiling := s.DriverLimits[driver].socCeiling()
	if device, ok := s.batteryDeviceSoCMax[driver]; ok && device < ceiling {
		return device
	}
	return ceiling
}

// readingChargeCeiling is chargeCeiling for a caller that holds the reading.
func readingChargeCeiling(r *telemetry.DerReading, lim PowerLimits) float64 {
	ceiling := lim.socCeiling()
	if r == nil {
		return ceiling
	}
	if device, ok := telemetry.BatterySoCCeiling(r.Data); ok && device < ceiling {
		return device
	}
	return ceiling
}

// BatteryChargePaused reports the dispatcher's full-battery stop. Callers use
// the same lock as ComputeDispatch; this state is local to the running device.
func (s *State) BatteryChargePaused(driver string) bool {
	return s != nil && s.batteryChargeFull[driver]
}

func (s *State) updateBatteryChargeFull(store *telemetry.Store, capacities map[string]float64) {
	if s == nil || store == nil {
		return
	}
	if s.batteryChargeFull == nil {
		s.batteryChargeFull = map[string]bool{}
	}
	for name := range s.batteryChargeFull {
		if _, ok := capacities[name]; !ok {
			delete(s.batteryChargeFull, name)
		}
	}
	s.updateBatteryDeviceSoCMax(store, capacities)
	for name := range capacities {
		r := store.Get(name, telemetry.DerBattery)
		h := store.DriverHealth(name)
		if r == nil || r.SoC == nil || h == nil || !h.IsOnline() {
			continue
		}
		age := s.now().Sub(r.SoCUpdatedAt)
		soc := *r.SoC
		if age < 0 || age > telemetry.BatterySoCMaxAge || math.IsNaN(soc) || math.IsInf(soc, 0) || soc < 0 || soc > 1 {
			continue
		}
		if soc >= s.chargeCeiling(name) {
			s.batteryChargeFull[name] = true
		} else if soc <= s.BatteryChargeResumeSoC(name) {
			delete(s.batteryChargeFull, name)
		}
	}
}

// updateBatteryDeviceSoCMax keeps the charge ceiling each battery's device
// reports in its latest reading. A reading without one clears it, so a driver
// that stops reporting falls back to the configured ceiling.
func (s *State) updateBatteryDeviceSoCMax(store *telemetry.Store, capacities map[string]float64) {
	if s.batteryDeviceSoCMax == nil {
		s.batteryDeviceSoCMax = map[string]float64{}
	}
	for name := range s.batteryDeviceSoCMax {
		if _, ok := capacities[name]; !ok {
			delete(s.batteryDeviceSoCMax, name)
		}
	}
	for name := range capacities {
		r := store.Get(name, telemetry.DerBattery)
		if r == nil {
			delete(s.batteryDeviceSoCMax, name)
			continue
		}
		if device, ok := telemetry.BatterySoCCeiling(r.Data); ok {
			s.batteryDeviceSoCMax[name] = device
		} else {
			delete(s.batteryDeviceSoCMax, name)
		}
	}
}

func (s *State) floorFullBatteryCharge(targets []DispatchTarget) []DispatchTarget {
	for i := range targets {
		if targets[i].TargetW > 0 && s.BatteryChargePaused(targets[i].Driver) {
			targets[i].TargetW = 0
			targets[i].Clamped = true
		}
	}
	return targets
}

// A holdoff or quiet meter must not leave an earlier charging command active.
// Preserve any fuse-relief discharge and stop only online, controlled batteries.
func (s *State) stopFullBatteries(targets []DispatchTarget, store *telemetry.Store, capacities map[string]float64) []DispatchTarget {
	if s == nil {
		return targets
	}
	seen := map[string]bool{}
	for _, target := range targets {
		seen[target.Driver] = true
	}
	for name := range capacities {
		if seen[name] || !s.BatteryChargePaused(name) {
			continue
		}
		r, h := store.Get(name, telemetry.DerBattery), store.DriverHealth(name)
		if r == nil || h == nil || !h.IsOnline() || s.PrevTargets[name] <= 0 && r.RawW <= 100 {
			continue
		}
		targets = append(targets, DispatchTarget{Driver: name, TargetW: 0, Clamped: true})
		if s.PrevTargets != nil {
			s.PrevTargets[name] = 0
		}
	}
	return s.floorFullBatteryCharge(targets)
}
