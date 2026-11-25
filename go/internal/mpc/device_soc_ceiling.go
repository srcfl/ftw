package mpc

import (
	"log/slog"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// withDeviceSoCCeilings plans each battery no higher than the charge level
// its own device reports it will accept (telemetry.BatterySoCCeiling). A
// Pixii stops charging at its own limit, 90 % on the site in
// srcfl/device-drivers#175, while the plan booked energy up to the planner's
// 95 %. The fleet's SoCMax becomes the capacity-weighted ceiling, so the Go
// DP and Energyplan plan to the same limit and the storage totals still
// match. A device ceiling at or below the planner's SoC floor cannot be
// planned with and is ignored; dispatch still stops charge at it.
func withDeviceSoCCeilings(p Params, tele *telemetry.Store) Params {
	if tele == nil || len(p.Storages) == 0 || p.CapacityWh <= 0 {
		return p
	}
	storages := append([]StorageAssetSpec(nil), p.Storages...)
	lowered := false
	var maxWh float64
	for i := range storages {
		storage := &storages[i]
		if r := tele.Get(storage.ID, telemetry.DerBattery); r != nil {
			if ceiling, ok := telemetry.BatterySoCCeiling(r.Data); ok {
				switch wh := storage.CapacityWh * ceiling; {
				case ceiling <= p.SoCMin:
					slog.Warn("mpc: ignoring a battery's own charge ceiling at or below the planner's SoC floor",
						"battery", storage.ID, "device_soc_max", ceiling, "soc_min", p.SoCMin)
				case wh < storage.MaxEnergyWh:
					storage.MaxEnergyWh = wh
					lowered = true
				}
			}
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
