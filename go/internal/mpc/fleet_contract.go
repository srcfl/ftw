package mpc

import (
	"fmt"
	"math"
)

// Core DP has one aggregate battery state and one EV state. Refuse models it
// cannot replay instead of dropping a second asset when the worker fails.
func coreDPModelError(p Params) error {
	if p.CapacityWh == 0 {
		return fmt.Errorf("Core DP requires a battery; Energyplan plans this site without storage")
	}
	if len(p.Storages) > 1 || len(p.activeLoadpoints()) > 1 {
		return fmt.Errorf("Core DP cannot retain all %d storages and %d EVs; keeping the previous validated plan", len(p.Storages), len(p.activeLoadpoints()))
	}
	if requiresStorageMaps(p) {
		return fmt.Errorf("Core DP cannot replay the physical storage efficiencies")
	}
	return nil
}

func requiresStorageMaps(p Params) bool {
	if len(p.Storages) > 1 {
		return true
	}
	for _, b := range p.Storages {
		if b.ChargeEfficiency != p.ChargeEfficiency || b.DischargeEfficiency != p.DischargeEfficiency {
			return true
		}
	}
	return false
}

func validateAssetMaps(p Params, a Action) error {
	var charging, discharging bool
	for _, w := range a.StoragePowerW {
		charging = charging || w > 2
		discharging = discharging || w < -2
	}
	if charging && discharging {
		return fmt.Errorf("opposing physical storage directions cannot be dispatched")
	}
	if p.CapacityWh == 0 {
		if len(p.Storages) != 0 || len(a.StoragePowerW) != 0 || len(a.StorageEnergyWh) != 0 || a.BatteryW != 0 || a.SoC != 0 || p.InitialSoC != 0 {
			return fmt.Errorf("site without storage contains battery power or energy")
		}
	}
	if requiresStorageMaps(p) || (len(p.Storages) > 0 && len(a.StoragePowerW) > 0) {
		if len(a.StoragePowerW) != len(p.Storages) || len(a.StorageEnergyWh) != len(p.Storages) {
			return fmt.Errorf("plan must retain every physical storage")
		}
	}
	loads := p.activeLoadpoints()
	if len(loads) > 1 || len(a.LoadpointPowerW) > 0 {
		if len(a.LoadpointPowerW) != len(loads) || len(a.LoadpointSoCByID) != len(loads) {
			return fmt.Errorf("plan must retain every EV")
		}
		for _, e := range loads {
			_, power := a.LoadpointPowerW[e.ID]
			_, energy := a.LoadpointSoCByID[e.ID]
			if !power || !energy {
				return fmt.Errorf("plan omits EV %q", e.ID)
			}
		}
	}
	return nil
}

// Inspect the wire response before translation can fill defaults or discard
// unknown identities. Core DP keeps its own aggregate and soft-target contract.
func validateExternalAssets(req externalRequest, plan externalPlan) error {
	if len(plan.Actions) != len(req.Slots) || plan.HorizonSlots != len(req.Slots) || plan.Mode != Mode(req.Settings.Mode) {
		return fmt.Errorf("worker changed horizon or mode")
	}
	var capacity, initial float64
	for _, b := range req.Storages {
		capacity += b.CapacityWh
		initial += b.InitialEnergyWh
	}
	initialSoC := 0.0
	if capacity > 0 {
		initialSoC = 100 * initial / capacity
	}
	if !finite(plan.CapacityWh) || !finite(plan.InitialSoC) || math.Abs(plan.CapacityWh-capacity) > 1 || math.Abs(plan.InitialSoC-initialSoC) > .02 {
		return fmt.Errorf("worker changed initial storage state")
	}
	for i, a := range plan.Actions {
		if a.PVCurtailActive {
			if req.Settings.PVCurtailmentMinW == nil || req.Settings.PVCurtailmentMaxW == nil || a.PVLimitW > *req.Settings.PVCurtailmentMaxW+1e-6 || !finite(a.PVLimitW) || math.Abs(a.PVLimitW-math.Round(a.PVLimitW)) > 1e-6 || a.PVLimitW+1e-6 < *req.Settings.PVCurtailmentMinW {
				return fmt.Errorf("slot %d PV cap lacks executable capability or violates the minimum", i)
			}
		} else if a.PVLimitW != 0 {
			return fmt.Errorf("slot %d worker PV cap needs an active flag", i)
		}
		if a.SlotStartMs != req.Slots[i].StartMs || a.SlotLenMin != req.Slots[i].LenMin {
			return fmt.Errorf("slot %d changed the requested timeline", i)
		}
		if len(a.StoragePowerW) != len(req.Storages) || len(a.StorageEnergy) != len(req.Storages) || len(a.FlexPowerW) != len(req.FlexLoads) || len(a.FlexEnergyWh) != len(req.FlexLoads) || len(a.ThermalPowerW) != 0 || len(a.ThermalState) != 0 {
			return fmt.Errorf("slot %d changed the requested assets", i)
		}
		for _, b := range req.Storages {
			_, power := a.StoragePowerW[b.ID]
			_, energy := a.StorageEnergy[b.ID]
			if !power || !energy {
				return fmt.Errorf("slot %d omits storage %q", i, b.ID)
			}
		}
		for _, e := range req.FlexLoads {
			_, power := a.FlexPowerW[e.ID]
			wh, energy := a.FlexEnergyWh[e.ID]
			if !power || !energy {
				return fmt.Errorf("slot %d omits EV %q", i, e.ID)
			}
			deadline := min(e.TargetSlot, len(req.Slots)-1)
			if e.TargetSlot >= 0 && e.TargetEnergyWh > 0 && i >= deadline && (!finite(wh) || wh+1 < e.TargetEnergyWh) {
				return fmt.Errorf("slot %d misses EV %q deadline", i, e.ID)
			}
		}
	}
	return nil
}
