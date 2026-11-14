package main

import (
	"sort"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/thermal"
	"github.com/srcfl/ftw/go/internal/thermaltwin"
)

// thermalTwinZones lists one shadow twin zone per room sensor an enabled
// driver declares. Only space heat for now: a tank or buffer has the same
// form but loses heat to the room it stands in, not to outdoors, and the
// service has no such temperature yet.
func thermalTwinZones(cfgDrivers []config.Driver, catalog []drivers.CatalogEntry) []thermaltwin.Zone {
	var out []thermaltwin.Zone
	for _, d := range cfgDrivers {
		if d.Disabled || d.Lua == "" {
			continue
		}
		entry := drivers.EntryForDriver(catalog, d.Lua)
		if entry == nil {
			continue
		}
		for _, s := range entry.Sensors {
			if s.Asset != thermal.AssetSpaceHeat {
				continue
			}
			out = append(out, thermaltwin.Zone{
				Key:          d.Name + "/" + s.Metric,
				SensorDriver: d.Name,
				SensorMetric: s.Metric,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// thermalTwinPowerDrivers lists the enabled drivers that may report heat
// pump power.
func thermalTwinPowerDrivers(cfgDrivers []config.Driver) []string {
	var out []string
	for _, d := range cfgDrivers {
		if !d.Disabled {
			out = append(out, d.Name)
		}
	}
	return out
}
