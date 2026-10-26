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
	return math.Max(0, s.DriverLimits[driver].socCeiling()-batteryChargeHysteresisSoC)
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
		if soc >= s.DriverLimits[name].socCeiling() {
			s.batteryChargeFull[name] = true
		} else if soc <= s.BatteryChargeResumeSoC(name) {
			delete(s.batteryChargeFull, name)
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
