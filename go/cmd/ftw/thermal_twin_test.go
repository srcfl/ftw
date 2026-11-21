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

	got, complete := thermalTwinZones(cfg, catalog, nil)
	if !complete {
		t.Error("complete = false with every enabled driver in the catalog")
	}
	want := []thermaltwin.Zone{
		{Key: "wiser/room_bed_temp_c", SensorDriver: "wiser", SensorMetric: "room_bed_temp_c"},
		{Key: "wiser/room_living_temp_c", SensorDriver: "wiser", SensorMetric: "room_living_temp_c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("zones = %+v, want %+v (no tank, no disabled driver)", got, want)
	}
	// A driver the catalog cannot describe has unknown sensors, not none:
	// the list is incomplete and no twin may be forgotten on its strength.
	missing := append(cfg, config.Driver{Name: "gone", Lua: filepath.Join(dir, "gone.lua")})
	if got, complete := thermalTwinZones(missing, catalog, nil); complete || !reflect.DeepEqual(got, want) {
		t.Errorf("with an unreadable driver: zones = %+v, complete = %v; want the same zones, incomplete", got, complete)
	}
	// A sensor the owner names in heat.zones is learned though its driver
	// declares nothing, with the heat input the owner named; a declared one
	// takes the named input too. A disabled driver's sensor is not.
	heat := &config.Heat{Zones: []config.HeatZone{
		{Sensor: "grid/zone_1_temp_c", HeatInput: "ctc/space_heat_w"},
		{Sensor: "wiser/room_bed_temp_c", HeatInput: "wiser/floor_heat_w"},
		{Sensor: "spare/room_bed_temp_c"},
	}}
	withHeat, _ := thermalTwinZones(cfg, catalog, heat)
	wantHeat := []thermaltwin.Zone{
		{Key: "grid/zone_1_temp_c", SensorDriver: "grid", SensorMetric: "zone_1_temp_c", HeatInput: "ctc/space_heat_w"},
		{Key: "wiser/room_bed_temp_c", SensorDriver: "wiser", SensorMetric: "room_bed_temp_c", HeatInput: "wiser/floor_heat_w"},
		{Key: "wiser/room_living_temp_c", SensorDriver: "wiser", SensorMetric: "room_living_temp_c"},
	}
	if !reflect.DeepEqual(withHeat, wantHeat) {
		t.Errorf("with heat zones = %+v, want %+v", withHeat, wantHeat)
	}
	if got, complete := thermalTwinZones(cfg, nil, nil); complete || len(got) != 0 {
		t.Errorf("without a catalog: zones = %+v, complete = %v; want none, incomplete", got, complete)
	}
	if p := thermalTwinPowerDrivers(cfg); !reflect.DeepEqual(p, []string{"wiser", "grid"}) {
		t.Errorf("power drivers = %v", p)
	}
}
