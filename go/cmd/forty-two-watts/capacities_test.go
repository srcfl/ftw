package main

import (
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

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
	got := driverCapacitiesFrom(drivers, loadpoints)
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
	got := driverCapacitiesFrom(drivers, nil)
	if got["ferroamp"] != 15200 {
		t.Errorf("lost ferroamp: %v", got)
	}
	if _, ok := got["meter"]; ok {
		t.Error("zero-capacity driver should not appear")
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
	got := driverCapacitiesFrom(drivers, loadpoints)
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

// TestDriverCapacitiesFromLuaFilenameFallback covers the case where
// an operator has not (yet) migrated to a `loadpoints:` config block
// but still has an EV driver with a vehicle-sized `battery_capacity_wh`.
// On Fredrik's Pi there are no loadpoints, yet the easee driver's
// 75000 Wh was inflating the MPC pool. Filename-based detection is
// the safety net.
func TestDriverCapacitiesFromLuaFilenameFallback(t *testing.T) {
	drivers := []config.Driver{
		{Name: "ferroamp", Lua: "drivers/ferroamp.lua", BatteryCapacityWh: 15200},
		{Name: "sungrow", Lua: "drivers/sungrow.lua", BatteryCapacityWh: 9600},
		{Name: "easee", Lua: "drivers/easee_cloud.lua", BatteryCapacityWh: 75000},
	}
	got := driverCapacitiesFrom(drivers, nil)
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
	got := driverCapacitiesFrom(drivers, loadpoints)
	if got["ferroamp"] != 15200 {
		t.Errorf("ferroamp dropped by invalid loadpoint row: %v", got)
	}
	if got["sungrow"] != 9600 {
		t.Errorf("sungrow missing: %v", got)
	}
}

func TestIsLikelyEVDriver(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// EV chargers — should match.
		{"drivers/easee_cloud.lua", true},
		{"drivers/easee.lua", true},
		{"drivers/ocpp_cp.lua", true},
		{"drivers/ocpp_csms.lua", true},
		{"drivers/ctek.lua", true},
		{"drivers/ctek_mqtt.lua", true},
		{"drivers/chargestorm.lua", true},
		{"/abs/path/to/drivers/EASEE.LUA", true}, // case-insensitive
		// Batteries / inverters / meters — must NOT match.
		{"drivers/ferroamp.lua", false},
		{"drivers/sungrow.lua", false},
		{"drivers/p1meter.lua", false},
		{"drivers/huawei_battery.lua", false},
		{"drivers/sim_ev.lua", false}, // "ev" substring not a prefix
		// Degenerate inputs.
		{"", false},
		{"easee_cloud.lua", true}, // bare filename also supported
	}
	for _, c := range cases {
		if got := isLikelyEVDriver(c.in); got != c.want {
			t.Errorf("isLikelyEVDriver(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
