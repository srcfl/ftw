package main

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// forecastMeasurementOptions declares known significant sources. FlowIDs remain
// unset: driver names and inverter groups do not identify physical measurements.
func forecastMeasurementOptions(cfg *config.Config, catalog []drivers.CatalogEntry) telemetry.ForecastOptions {
	opts, _ := forecastMeasurementTopology(cfg, catalog)
	return opts
}

// forecastMeasurementTopologyUnknown names sources whose presence config cannot
// establish. Callers must require fresh observations of these optional flows
// before qualifying a complete household balance; this is not only a log hint.
func forecastMeasurementTopologyUnknown(cfg *config.Config, catalog []drivers.CatalogEntry) []string {
	_, unknown := forecastMeasurementTopology(cfg, catalog)
	return unknown
}

func forecastMeasurementTopology(cfg *config.Config, catalog []drivers.CatalogEntry) (telemetry.ForecastOptions, []string) {
	opts := telemetry.ForecastOptions{}
	if cfg == nil {
		opts.HouseholdInvalidReason = "missing_config"
		return opts, []string{"missing_config"}
	}
	var unknown []string
	type configured struct {
		driver config.Driver
		entry  drivers.CatalogEntry
		found  bool
	}
	active := make([]configured, 0, len(cfg.Drivers))
	pvSources := 0
	for _, d := range cfg.Drivers {
		if d.Disabled {
			continue
		}
		entry, found := forecastCatalogEntry(catalog, d.Lua)
		active = append(active, configured{d, entry, found})
		if found && forecastCapability(entry, "pv") {
			pvSources++
		}
	}
	sitePV := cfg.Weather != nil && (cfg.Weather.PVRatedW > 0 || len(cfg.Weather.PVArrays) > 0)
	expectedPV := 0
	seen := make(map[telemetry.ForecastFlow]bool)
	add := func(d config.Driver, kind telemetry.DerType) {
		flow := telemetry.ForecastFlow{Driver: d.Name, DerType: kind}
		if seen[flow] {
			return
		}
		seen[flow] = true
		if kind == telemetry.DerPV {
			expectedPV++
		}
		opts.ExpectedFlows = append(opts.ExpectedFlows, flow)
	}
	for _, item := range active {
		d, e := item.driver, item.entry
		if d.IsSiteMeter {
			add(d, telemetry.DerMeter)
		}
		if !item.found {
			unknown = append(unknown, d.Name+":unknown_driver_capabilities")
			if d.BatteryCapacityWh > 0 || d.BatteryTelemetryOnly {
				add(d, telemetry.DerBattery)
			}
			continue
		}
		pv, bat, ev, v2x := forecastCapability(e, "pv"), forecastCapability(e, "battery"), forecastCapability(e, "ev"), forecastCapability(e, "v2x_charger")
		if v2x {
			add(d, telemetry.DerV2X)
		} else if ev {
			add(d, telemetry.DerEV)
		}
		if bat && (d.BatteryCapacityWh > 0 || d.BatteryTelemetryOnly) {
			add(d, telemetry.DerBattery)
		}
		if pv {
			readPV, hasReadPV := d.Config["read_pv"].(bool)
			dedicated := !bat && !ev && !v2x
			declared := readPV || d.SupportsPVCurtail || dedicated || (sitePV && pvSources == 1)
			if hasReadPV && !readPV {
				continue
			}
			// Optional does not mean absent. Require a fresh reading (zero is valid)
			// before using the household balance, while preserving why it was required.
			add(d, telemetry.DerPV)
			if !declared {
				unknown = append(unknown, d.Name+":optional_pv_requires_measurement")
			}
		}
	}
	// Adopted OCPP chargers publish EV power under driver_name without a Lua
	// registry entry. Their first missing reading is unknown, including at boot.
	// A Lua V2X charger already owns the one bidirectional measurement.
	for _, lp := range cfg.Loadpoints {
		name := lp.DriverName
		if strings.TrimSpace(name) == "" {
			continue
		}
		if seen[telemetry.ForecastFlow{Driver: name, DerType: telemetry.DerV2X}] {
			continue
		}
		add(config.Driver{Name: name}, telemetry.DerEV)
	}
	if sitePV && expectedPV == 0 {
		opts.HouseholdInvalidReason = "configured_pv_without_measurement_source"
		unknown = append(unknown, "site:configured_pv_without_measurement_source")
	}
	return opts, unknown
}

func forecastCapability(e drivers.CatalogEntry, kind string) bool {
	for _, capability := range e.Capabilities {
		if capability == kind {
			return true
		}
	}
	return false
}

func forecastCatalogEntry(catalog []drivers.CatalogEntry, luaPath string) (drivers.CatalogEntry, bool) {
	if strings.TrimSpace(luaPath) == "" {
		return drivers.CatalogEntry{}, false
	}
	want := filepath.ToSlash(filepath.Clean(luaPath))
	for _, e := range catalog {
		if e.Path != "" && strings.EqualFold(filepath.ToSlash(filepath.Clean(e.Path)), want) {
			return e, true
		}
	}
	// Ambiguous basenames can refer to different user and managed packages.
	var found drivers.CatalogEntry
	matches := 0
	for _, e := range catalog {
		name := e.Filename
		if name == "" {
			name = filepath.Base(e.Path)
		}
		if strings.EqualFold(name, filepath.Base(want)) {
			found = e
			matches++
		}
	}
	return found, matches == 1
}

// forecastCurtailmentActive only reads control intent. Caller holds ctrlMu.
// LastCurtailedDrivers does not prove release acknowledgment; callers must also
// retain unresolved failed releases from command results.
func forecastCurtailmentActive(ctrl *control.State, now time.Time) bool {
	if ctrl == nil {
		return false
	}
	if len(ctrl.LastCurtailedDrivers) > 0 {
		return true
	}
	hold := ctrl.ManualPVHold
	if !hold.ExpiresAt.IsZero() && now.Before(hold.ExpiresAt) {
		return true
	}
	if ctrl.SlotDirective != nil {
		if slot, ok := ctrl.SlotDirective(now); ok && slot.PVLimitW > 0 {
			if (slot.SlotStart.IsZero() || !now.Before(slot.SlotStart)) && (slot.SlotEnd.IsZero() || now.Before(slot.SlotEnd)) {
				return true
			}
		}
	}
	return false
}
