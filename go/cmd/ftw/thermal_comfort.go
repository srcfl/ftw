package main

import (
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/heatplan"
)

// thermalGateKeepShare keeps an open savings gate open down to this share
// of the owner's limit, so a saving near the limit does not start and stop
// the planning every quarter. It is not a setting: it changes no outcome
// the owner would choose between.
const thermalGateKeepShare = 0.8

// thermalComfort is the owner's comfort rule for one zone, by its sensor
// key. A zone the owner did not list, or a site with no thermal section,
// gets the zero value, which moves nothing.
func thermalComfort(cfg *config.Thermal, sensor string) heatplan.PriceComfort {
	z, ok := cfg.Zone(sensor)
	if !ok {
		return heatplan.PriceComfort{}
	}
	return heatplan.PriceComfort{
		CheapPrice: cfg.CheapDayOreKWh, NormalPrice: cfg.NormalDayOreKWh, DearPrice: cfg.DearDayOreKWh,
		RaiseK: z.CheapDayRaiseK, LowerK: z.DearDayLowerK, StepK: z.StepK,
	}
}

// thermalSavingsGate is the owner's least daily saving, in öre. No limit
// set is no gate.
func thermalSavingsGate(cfg *config.Thermal) heatplan.SavingsGate {
	if cfg == nil || cfg.MinSavingOrePerDay <= 0 {
		return heatplan.SavingsGate{}
	}
	return heatplan.SavingsGate{MinPerDay: cfg.MinSavingOrePerDay, KeepShare: thermalGateKeepShare}
}
