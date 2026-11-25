package mpc

import (
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// withDeviceSoCCeilings plans each battery no higher than the charge level
// its own device reports it will accept (telemetry.DerReading.SoCCeilingNow).
// A Pixii stops charging at its own limit, 90 % on the site in
// srcfl/device-drivers#175, while the plan booked energy up to the planner's
// 95 %. Only MaxEnergyWh changes; the fleet's SoCMax becomes the
// capacity-weighted ceiling, so the Go DP and Energyplan plan to the same
// limit and the storage totals still match.
//
// Telemetry drops a ceiling below telemetry.MinBatterySoCCeiling. One at or
// below the planner's SoC floor leaves nothing to plan in and is not used;
// dispatch logs that case.
func withDeviceSoCCeilings(p Params, tele *telemetry.Store) Params {
	if tele == nil || len(p.Storages) == 0 || p.CapacityWh <= 0 {
		return p
	}
	now := time.Now()
	storages := append([]StorageAssetSpec(nil), p.Storages...)
	lowered := false
	var maxWh float64
	for i := range storages {
		storage := &storages[i]
		ceiling, ok := tele.Get(storage.ID, telemetry.DerBattery).SoCCeilingNow(now)
		// The plan has one SoC floor for the fleet, p.SoCMin; dispatch uses each
		// battery's own (control.PowerLimits.socFloor), which can be higher.
		if wh := storage.CapacityWh * ceiling; ok && ceiling > p.SoCMin && wh < storage.MaxEnergyWh {
			storage.MaxEnergyWh = wh
			lowered = true
		}
		maxWh += storage.MaxEnergyWh
	}
	if !lowered {
		return p
	}
	p.Storages = storages
	p.SoCMax = maxWh / p.CapacityWh
	return p
}
