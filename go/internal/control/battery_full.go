package control

import (
	"log/slog"
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
// battery's own device reports (telemetry.DerReading.SoCCeilingNow), as the
// last control tick saw it. A Pixii stops charging at its own limit, so an
// order above it goes undelivered.
func (s *State) chargeCeiling(driver string) float64 {
	ceiling := s.DriverLimits[driver].socCeiling()
	if device := s.batteryDeviceSoCMax[driver].value; device > 0 && device < ceiling {
		return device
	}
	return ceiling
}

// readingChargeCeiling is chargeCeiling for a caller that holds an online
// battery's reading.
func readingChargeCeiling(r *telemetry.DerReading, lim PowerLimits) float64 {
	ceiling := lim.socCeiling()
	if r != nil && r.SoCCeiling != nil {
		if device := *r.SoCCeiling; usableDeviceCeiling(device, lim) && device < ceiling {
			return device
		}
	}
	return ceiling
}

// usableDeviceCeiling: a device ceiling at or below the battery's SoC floor
// leaves no band to charge or discharge in, so it is not used. Telemetry has
// already dropped one below telemetry.MinBatterySoCCeiling.
func usableDeviceCeiling(device float64, lim PowerLimits) bool {
	return device > lim.socFloor()
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

// deviceSoCMax is the device ceiling a control tick saw for one battery.
type deviceSoCMax struct {
	value      float64 // 0 when none applies
	belowFloor float64 // last ceiling logged as at or below the SoC floor
}

// updateBatteryDeviceSoCMax takes each battery's own charge ceiling from its
// reading. Telemetry holds a ceiling across a few readings without it and
// drops it with a stale reading; here it must also sit above the battery's
// SoC floor, or it is logged once and not used.
func (s *State) updateBatteryDeviceSoCMax(store *telemetry.Store, capacities map[string]float64) {
	if s.batteryDeviceSoCMax == nil {
		s.batteryDeviceSoCMax = map[string]deviceSoCMax{}
	}
	for name := range s.batteryDeviceSoCMax {
		if _, ok := capacities[name]; !ok {
			delete(s.batteryDeviceSoCMax, name)
		}
	}
	now := s.now()
	for name := range capacities {
		seen := s.batteryDeviceSoCMax[name]
		seen.value = 0
		if device, ok := store.Get(name, telemetry.DerBattery).SoCCeilingNow(now); ok {
			lim := s.DriverLimits[name]
			switch {
			case usableDeviceCeiling(device, lim):
				seen.value, seen.belowFloor = device, 0
			case seen.belowFloor != device:
				slog.Warn("control: ignoring a battery's own charge ceiling at or below its SoC floor",
					"driver", name, "max_soc_fract", device, "soc_floor", lim.socFloor())
				seen.belowFloor = device
			}
		}
		s.batteryDeviceSoCMax[name] = seen
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
