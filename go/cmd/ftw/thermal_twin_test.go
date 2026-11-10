package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/thermaltwin"
)

func TestThermalTwinZonesFromDeclaredSensors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("thermo.lua", `DRIVER = {
  id = "thermo",
  sensors = {
    { metric = "room_living_temp_c", asset = "space_heat", zone = "living" },
    { metric = "room_bed_temp_c", asset = "space_heat" },
    { metric = "tank_temp_c", asset = "dhw" },
  },
}
`)
	write("meter.lua", `DRIVER = { id = "meter" }
`)
	catalog, err := drivers.LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := []config.Driver{
		{Name: "wiser", Lua: filepath.Join(dir, "thermo.lua")},
		{Name: "spare", Lua: filepath.Join(dir, "thermo.lua"), Disabled: true},
		{Name: "grid", Lua: filepath.Join(dir, "meter.lua")},
	}

	got := thermalTwinZones(cfg, catalog)
	want := []thermaltwin.Zone{
		{Key: "wiser/room_bed_temp_c", SensorDriver: "wiser", SensorMetric: "room_bed_temp_c"},
		{Key: "wiser/room_living_temp_c", SensorDriver: "wiser", SensorMetric: "room_living_temp_c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("zones = %+v, want %+v (no tank, no disabled driver)", got, want)
	}
	if p := thermalTwinPowerDrivers(cfg); !reflect.DeepEqual(p, []string{"wiser", "grid"}) {
		t.Errorf("power drivers = %v", p)
	}
}
