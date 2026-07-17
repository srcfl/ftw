package main

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
)

// evCatalog returns a fixture catalog that mirrors the production
// drivers/ directory's self-declarations: EV chargers expose "ev",
// the Tesla vehicle driver exposes "vehicle", everything else
// declares site capabilities. Used by tests below in place of an on-
// disk scan.
func evCatalog() []drivers.CatalogEntry {
	return []drivers.CatalogEntry{
		{Path: "drivers/ferroamp.lua", Filename: "ferroamp.lua", Capabilities: []string{"meter", "pv", "battery"}},
		{Path: "drivers/sungrow.lua", Filename: "sungrow.lua", Capabilities: []string{"pv", "battery"}},
		{Path: "drivers/easee_cloud.lua", Filename: "easee_cloud.lua", Capabilities: []string{"ev"}},
		{Path: "drivers/ctek_hybrid.lua", Filename: "ctek_hybrid.lua", Capabilities: []string{"ev"}},
		{Path: "drivers/tesla_vehicle.lua", Filename: "tesla_vehicle.lua", Capabilities: []string{"vehicle"}},
		{Path: "drivers/zap.lua", Filename: "zap.lua", Capabilities: []string{"meter", "pv", "battery", "v2x_charger"}, ReadOnly: true},
		{Path: "drivers/p1meter.lua", Filename: "p1meter.lua", Capabilities: []string{"meter"}},
		{Path: "drivers/huawei_battery.lua", Filename: "huawei_battery.lua", Capabilities: []string{"battery"}},
		{Path: "drivers/sim_ev.lua", Filename: "sim_ev.lua", Capabilities: []string{"meter"}},
	}
}

// TestDriverCapacitiesFromExcludesEV is the regression test for the
// bug where an Easee cloud driver's YAML entry with
// `battery_capacity_wh: 75000` inflated the MPC battery pool from
// ~24 kWh to ~100 kWh. Any driver referenced by a loadpoint is an EV
// charger — its capacity is VEHICLE capacity, not site battery.
func TestDriverCapacitiesFromExcludesEV(t *testing.T) {
	drivers := []config.Driver{
		{Name: "ferroamp", BatteryCapacityWh: 15200},
		{Name: "sungrow", BatteryCapacityWh: 9600},
		{Name: "easee", BatteryCapacityWh: 75000},
	}
	loadpoints := []config.Loadpoint{
		{ID: "garage", DriverName: "easee"},
	}
	got := driverCapacitiesFrom(drivers, loadpoints, evCatalog())
	if _, ok := got["easee"]; ok {
		t.Errorf("easee should be filtered out; got %v", got)
	}
	if got["ferroamp"] != 15200 {
		t.Errorf("ferroamp kept wrong value: %v", got["ferroamp"])
	}
	if got["sungrow"] != 9600 {
		t.Errorf("sungrow kept wrong value: %v", got["sungrow"])
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(got), got)
	}
}

func TestDriverCapacitiesFromNoLoadpointsBehavesAsBefore(t *testing.T) {
	drivers := []config.Driver{
		{Name: "ferroamp", BatteryCapacityWh: 15200},
		{Name: "meter", BatteryCapacityWh: 0},
	}
	got := driverCapacitiesFrom(drivers, nil, evCatalog())
	if got["ferroamp"] != 15200 {
		t.Errorf("lost ferroamp: %v", got)
	}
	if _, ok := got["meter"]; ok {
		t.Error("zero-capacity driver should not appear")
	}
}

func TestDriverCapacitiesFromExcludesTelemetryOnlyAndReadOnly(t *testing.T) {
	driverConfigs := []config.Driver{
		{Name: "ferroamp", Lua: "drivers/ferroamp.lua", BatteryCapacityWh: 15200},
		// Old or hand-written Zap configs may carry a capacity but lack the
		// new explicit flag. Catalog read_only must still be authoritative.
		{Name: "zap", Lua: "drivers/zap.lua", BatteryCapacityWh: 60000},
		{Name: "custom-gateway", Lua: "drivers/custom.lua", BatteryCapacityWh: 10000, BatteryTelemetryOnly: true},
	}
	got := driverCapacitiesFrom(driverConfigs, nil, evCatalog())
	if len(got) != 1 || got["ferroamp"] != 15200 {
		t.Fatalf("telemetry-only gateways entered battery control pool: %v", got)
	}
	if !drivers.IsReadOnlyDriver(evCatalog(), "zap.lua") {
		t.Fatal("Zap read_only catalog declaration was not matched by filename")
	}
}

func TestDriverCapacitiesFromMultipleLoadpoints(t *testing.T) {
	drivers := []config.Driver{
		{Name: "ferroamp", BatteryCapacityWh: 15200},
		{Name: "easee", BatteryCapacityWh: 75000},
		{Name: "zap", BatteryCapacityWh: 60000},
	}
	loadpoints := []config.Loadpoint{
		{ID: "garage", DriverName: "easee"},
		{ID: "street", DriverName: "zap"},
	}
	got := driverCapacitiesFrom(drivers, loadpoints, evCatalog())
	if len(got) != 1 || got["ferroamp"] != 15200 {
		t.Errorf("expected only ferroamp, got %v", got)
	}
}

func TestBuildLoadpointConfigsPreservesSurplusOnly(t *testing.T) {
	got := buildLoadpointConfigs([]config.Loadpoint{{
		ID:          "garage",
		DriverName:  "easee",
		SurplusOnly: true,
	}})
	if len(got) != 1 {
		t.Fatalf("loadpoints = %d, want 1", len(got))
	}
	if !got[0].SurplusOnly {
		t.Fatal("SurplusOnly was dropped by buildLoadpointConfigs")
	}
}

// TestDriverCapacitiesFromCatalogFallback covers the case where an
// operator has not (yet) migrated to a `loadpoints:` config block but
// still has an EV driver with a vehicle-sized `battery_capacity_wh`.
// On Fredrik's Pi there are no loadpoints, yet the easee driver's
// 75000 Wh was inflating the MPC pool. The driver catalog's
// self-declared "ev" capability is the safety net.
func TestDriverCapacitiesFromCatalogFallback(t *testing.T) {
	drivers := []config.Driver{
		{Name: "ferroamp", Lua: "drivers/ferroamp.lua", BatteryCapacityWh: 15200},
		{Name: "sungrow", Lua: "drivers/sungrow.lua", BatteryCapacityWh: 9600},
		{Name: "easee", Lua: "drivers/easee_cloud.lua", BatteryCapacityWh: 75000},
	}
	got := driverCapacitiesFrom(drivers, nil, evCatalog())
	if _, ok := got["easee"]; ok {
		t.Errorf("easee should be filtered out by filename fallback; got %v", got)
	}
	if got["ferroamp"] != 15200 {
		t.Errorf("ferroamp kept wrong value: %v", got["ferroamp"])
	}
	if got["sungrow"] != 9600 {
		t.Errorf("sungrow kept wrong value: %v", got["sungrow"])
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(got), got)
	}
}

// TestDriverCapacitiesFromIgnoresInvalidLoadpoints covers the
// Codex P2 on PR #121: loadpoint entries with missing id are
// rejected by loadpoint.Manager but were silently excluding their
// driver from the MPC pool anyway. A malformed config row should
// never cause a real battery to vanish from the planner.
func TestDriverCapacitiesFromIgnoresInvalidLoadpoints(t *testing.T) {
	drivers := []config.Driver{
		{Name: "ferroamp", BatteryCapacityWh: 15200},
		{Name: "sungrow", BatteryCapacityWh: 9600},
	}
	// Malformed loadpoint row: no id. Manager would reject this.
	// Must NOT cause ferroamp to be excluded from MPC capacity.
	loadpoints := []config.Loadpoint{
		{ID: "", DriverName: "ferroamp"},
	}
	got := driverCapacitiesFrom(drivers, loadpoints, evCatalog())
	if got["ferroamp"] != 15200 {
		t.Errorf("ferroamp dropped by invalid loadpoint row: %v", got)
	}
	if got["sungrow"] != 9600 {
		t.Errorf("sungrow missing: %v", got)
	}
}

// TestIsEVOrVehicleDriverByCapabilities exercises the catalog-backed
// classifier that replaced the filename sniffer. The contract: a
// driver counts as "EV or vehicle" iff its Lua DRIVER block self-
// declares the corresponding capability, regardless of filename or
// vendor name. Drivers absent from the catalog are conservatively
// classified as non-EV (so a missing entry never silently excludes a
// real battery from the MPC pool).
func TestIsEVOrVehicleDriverByCapabilities(t *testing.T) {
	cat := evCatalog()
	cases := []struct {
		in   string
		want bool
	}{
		// EV chargers — match via capability, not filename.
		{"drivers/easee_cloud.lua", true},
		{"DRIVERS/EASEE_CLOUD.LUA", true},
		{"drivers/ctek_hybrid.lua", true},
		// Vehicle telemetry — match via capability.
		{"drivers/tesla_vehicle.lua", true},
		// Stationary site assets — must NOT match.
		{"drivers/ferroamp.lua", false},
		{"drivers/sungrow.lua", false},
		{"drivers/p1meter.lua", false},
		{"drivers/huawei_battery.lua", false},
		{"drivers/zap.lua", false}, // read-only battery gateway, not ev
		// A filename that LOOKS EV-ish but the driver self-declares
		// otherwise — the previous filename-prefix heuristic would
		// have over-matched anything starting with "sim_ev", and the
		// allowlist-style sniffer was the workaround. Capability-based
		// classification has no such hazard.
		{"drivers/sim_ev.lua", false},
		// Drivers missing from the catalog (third-party plugin not
		// yet scanned) are conservatively NOT classified as EV.
		{"drivers/unknown.lua", false},
		// Bare filename works as well — operators reference drivers
		// either way in YAML.
		{"easee_cloud.lua", true},
		// Degenerate input.
		{"", false},
	}
	for _, c := range cases {
		if got := drivers.IsEVOrVehicleDriver(cat, c.in); got != c.want {
			t.Errorf("IsEVOrVehicleDriver(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
