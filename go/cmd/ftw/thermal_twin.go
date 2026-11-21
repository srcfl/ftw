package main

import (
	"sort"
	"strings"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/thermal"
	"github.com/srcfl/ftw/go/internal/thermaltwin"
)

// thermalTwinZones lists one shadow twin zone per room sensor an enabled
// driver declares. Only space heat for now: a tank or buffer has the same
// form but loses heat to the room it stands in, not to outdoors, and the
// service has no such temperature yet.
//
// complete is false when an enabled driver has no catalog entry, so its
// sensors are unknown rather than absent. The service then forgets no twin.
//
// A sensor the owner names in heat.zones is learned even when its driver
// does not declare it, as a room sensor on an underfloor gateway, and takes
// the heat input the owner named for it.
func thermalTwinZones(cfgDrivers []config.Driver, catalog []drivers.CatalogEntry, heat *config.Heat) (zones []thermaltwin.Zone, complete bool) {
	var out []thermaltwin.Zone
	complete = true
	for _, d := range cfgDrivers {
		if d.Disabled || d.Lua == "" {
			continue
		}
		entry := drivers.EntryForDriver(catalog, d.Lua)
		if entry == nil {
			complete = false
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
	if heat != nil {
		enabled := map[string]bool{}
		for _, d := range cfgDrivers {
			if !d.Disabled && d.Lua != "" {
				enabled[d.Name] = true
			}
		}
		index := map[string]int{}
		for i, z := range out {
			index[z.Key] = i
		}
		for _, hz := range heat.Zones {
			driver, metric, ok := strings.Cut(hz.Sensor, "/")
			if !ok || !enabled[driver] {
				continue
			}
			if i, ok := index[hz.Sensor]; ok {
				out[i].HeatInput = hz.HeatInput
				continue
			}
			index[hz.Sensor] = len(out)
			out = append(out, thermaltwin.Zone{Key: hz.Sensor, SensorDriver: driver, SensorMetric: metric, HeatInput: hz.HeatInput})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, complete
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
