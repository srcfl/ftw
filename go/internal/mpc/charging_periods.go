package mpc

import (
	"context"
	"math"
)

// ChargingPeriods describes a preference, after deadline energy and physical
// reserves. It never delays Core safety, manual stop, or charge completion.
// StartCostOre is part of the optimizer preference, not the electricity bill.
type ChargingPeriods struct {
	MinChargeSeconds     float64 `json:"min_charge_seconds"`
	StartCostOre         float64 `json:"start_cost_ore"`
	InitialCharging      bool    `json:"initial_charging"`
	InitialChargeSeconds float64 `json:"initial_charge_seconds"`
}

// DefaultChargingPeriods favors runs of at least five minutes and avoids an
// extra start for less than five ore. A final top-up may be shorter.
func DefaultChargingPeriods(charging bool, seconds float64) ChargingPeriods {
	return ChargingPeriods{MinChargeSeconds: 300, StartCostOre: 5,
		InitialCharging: charging, InitialChargeSeconds: min(300, max(0, seconds))}
}

// departureMissWh is the least missing energy that counts as a missed
// departure. It follows Energyplan's need rounding (worker/src/fleet.rs,
// model_target): a need of at most 1 Wh, or of at most half of one minimum
// charging run, counts as met. One run stores the smallest allowed step for
// MinChargeSeconds at the charge efficiency Core sends. A car charging at
// plan start keeps the 1 Wh rule: the worker finishes its run exactly.
func (l *LoadpointSpec) departureMissWh() float64 {
	if l.Charging.InitialCharging || l.Charging.MinChargeSeconds <= 0 {
		return 1
	}
	efficiency := l.ChargeEfficiency
	if efficiency <= 0 {
		efficiency = 0.9
	}
	for _, step := range l.normalizedSteps() {
		if step > 0 {
			return max(1, step*l.Charging.MinChargeSeconds/3600*efficiency/2)
		}
	}
	return 1
}

func (o *EnergyplanOptimizer) prepareEnergyplanRequest(ctx context.Context, request *externalRequest, slots []Slot, p Params) error {
	// Negotiate against the running worker on every request. Restart or a
	// binary rollback must not inherit a feature learned from another process.
	probeCtx, cancel := context.WithTimeout(ctx, o.cfg.Timeout)
	defer cancel()
	info, err := o.Health(probeCtx)
	if err != nil {
		return err
	}
	if optimizerHasFeature(info, "published_prices") {
		for i := range request.Slots {
			request.Slots[i].Confidence = 0
		}
	}
	// A worker without demand_elapsed_hours gets the demand charges as
	// before: no hour that began before the first slot, each completed day
	// in already_kw. Its budget follows the charges it gets, so a charge with
	// only the hour in progress leaves with its fleet budget.
	if !optimizerHasFeature(info, "demand_elapsed_hours") && hasElapsedDemand(p.DemandCharges) {
		p.DemandCharges = legacyDemandCharges(p.DemandCharges)
		request.DemandCharges = externalDemandCharges(p.DemandCharges)
		if o.timeBudget != nil {
			request.Settings.TimeLimitS = math.Min(o.cfg.Timeout.Seconds()*0.8, o.timeBudget(slots, p).Seconds())
		}
	}
	// A worker without the hours plans past them; Core's check rejects
	// such a plan and uses its own. The worker plans the hours in its fleet
	// model, with the demand charges' budget.
	if optimizerHasFeature(info, "hourly_import_caps") && len(slots) > 0 {
		for _, h := range p.ImportHours {
			if importHourPlannedH(slots, h) > 0 {
				request.HourlyImportCaps = append(request.HourlyImportCaps, externalImportCap{
					StartMs: h.StartMs, EndMs: h.EndMs, MaxMeanW: p.ImportMeanW, ElapsedImportWh: h.ElapsedImportWh})
			}
		}
		if len(request.HourlyImportCaps) > 0 {
			request.Settings.TimeLimitS = math.Min(o.cfg.Timeout.Seconds()*0.8, energyplanBudget(slots, p, true).Seconds())
		}
	}
	loads := p.activeLoadpoints()
	needed := false
	for _, lp := range loads {
		needed = needed || lp.Charging.MinChargeSeconds > 0 || lp.Charging.StartCostOre > 0
	}
	if !needed {
		return nil
	}
	if !optimizerHasFeature(info, "charging_periods") {
		return nil
	}
	byID := make(map[string]ChargingPeriods, len(loads))
	for _, lp := range loads {
		byID[lp.ID] = lp.Charging
	}
	for i := range request.FlexLoads {
		c := byID[request.FlexLoads[i].ID]
		request.FlexLoads[i].Charging = &c
	}
	return nil
}
