package main

import (
	"fmt"
	"slices"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
)

// Until a loadpoint has an explicit vehicle binding, one configured vehicle
// and one connected charger are the only unambiguous telemetry recipient.
// Freshness remains a requirement for planning and charge_start, not for wake.
func configuredVehicleRefreshDriver(configured []config.Driver, connected int) (string, error) {
	if connected != 1 {
		return "", fmt.Errorf("vehicle refresh requires one connected loadpoint")
	}
	var candidate *config.Driver
	var entry drivers.CatalogEntry
	for i := range configured {
		d := &configured[i]
		if d.Disabled {
			continue
		}
		e, err := drivers.ParseCatalogFile(d.Lua)
		if err != nil {
			return "", fmt.Errorf("vehicle refresh: cannot read driver metadata")
		}
		if !slices.Contains(e.Capabilities, "vehicle") {
			continue
		}
		if candidate != nil {
			return "", fmt.Errorf("vehicle refresh is ambiguous with multiple configured vehicles")
		}
		candidate, entry = d, e
	}
	if candidate == nil {
		return "", nil
	}
	if candidate.ObserveOnly || entry.ReadOnly || !entry.TelemetryWake {
		return "", nil
	}
	return candidate.Name, nil
}
