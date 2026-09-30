package control

import (
	"math"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A one percentage point gap prevents repeated starts at a rounded 100%.
// This limits charging only; the plan can discharge a full battery at once.
const BatteryChargeResumeSoC = 0.99

// BatteryChargePaused reports the dispatcher's full-battery stop. Callers use
// the same lock as ComputeDispatch; this state is local to the running device.
func (s *State) BatteryChargePaused(driver string) bool {
	return s != nil && s.batteryChargeFull[driver]
}

func (s *State) updateBatteryChargeFull(store *telemetry.Store, capacities map[string]float64) {
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
		if soc == 1 {
			s.batteryChargeFull[name] = true
		} else if soc <= BatteryChargeResumeSoC {
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
