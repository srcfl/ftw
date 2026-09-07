package main

import (
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"reflect"
	"testing"
	"time"
)

func measurementCatalog() []drivers.CatalogEntry {
	return []drivers.CatalogEntry{
		{Path: "drivers/meter.lua", Filename: "meter.lua", Capabilities: []string{"meter"}},
		{Path: "drivers/hybrid.lua", Filename: "hybrid.lua", Capabilities: []string{"meter", "pv", "battery"}},
		{Path: "drivers/solar.lua", Filename: "solar.lua", Capabilities: []string{"pv"}},
		{Path: "drivers/charger.lua", Filename: "charger.lua", Capabilities: []string{"ev"}},
		{Path: "drivers/v2x.lua", Filename: "v2x.lua", Capabilities: []string{"v2x_charger", "ev"}},
		{Path: "drivers/vehicle.lua", Filename: "vehicle.lua", ReadOnly: true, Capabilities: []string{"vehicle"}},
	}
}
func TestForecastMeasurementOptionsInstalledFlows(t *testing.T) {
	cfg := &config.Config{Drivers: []config.Driver{
		{Name: "site", Lua: "drivers/meter.lua", IsSiteMeter: true},
		{Name: "hybrid-no-pack", Lua: "drivers/hybrid.lua", Config: map[string]any{"read_pv": false}},
		{Name: "battery", Lua: "drivers/hybrid.lua", BatteryCapacityWh: 10000, Config: map[string]any{"read_pv": false}},
		{Name: "observed-battery", Lua: "drivers/hybrid.lua", BatteryTelemetryOnly: true, Config: map[string]any{"read_pv": false}},
		{Name: "solar", Lua: "/install/drivers/solar.lua"},
		{Name: "charger", Lua: "drivers/charger.lua", BatteryCapacityWh: 70000},
		{Name: "vehicle", Lua: "drivers/vehicle.lua", BatteryCapacityWh: 70000},
		{Name: "v2x", Lua: "drivers/v2x.lua"},
		{Name: "disabled", Lua: "drivers/solar.lua", Disabled: true},
	}}
	opts := forecastMeasurementOptions(cfg, measurementCatalog())
	want := []telemetry.ForecastFlow{
		{Driver: "site", DerType: telemetry.DerMeter},
		{Driver: "battery", DerType: telemetry.DerBattery},
		{Driver: "observed-battery", DerType: telemetry.DerBattery},
		{Driver: "solar", DerType: telemetry.DerPV},
		{Driver: "charger", DerType: telemetry.DerEV},
		{Driver: "v2x", DerType: telemetry.DerV2X},
	}
	if !reflect.DeepEqual(opts.ExpectedFlows, want) {
		t.Fatalf("flows=%+v want=%+v", opts.ExpectedFlows, want)
	}
}
func TestForecastMeasurementNoPVSiteAndUnknownHybrid(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("site")
	cfg := &config.Config{Drivers: []config.Driver{{Name: "site", Lua: "drivers/meter.lua", IsSiteMeter: true}}}
	r := tel.ForecastMeasurement(time.Now(), "site", forecastMeasurementOptions(cfg, measurementCatalog()))
	if !r.Valid || r.PVValid {
		t.Fatalf("no-PV site: %+v", r)
	}
	cfg.Drivers = append(cfg.Drivers, config.Driver{Name: "hybrid", Lua: "drivers/hybrid.lua"})
	opts := forecastMeasurementOptions(cfg, measurementCatalog())
	r = tel.ForecastMeasurement(time.Now(), "site", opts)
	if r.Valid || r.PVValid || len(forecastMeasurementTopologyUnknown(cfg, measurementCatalog())) != 1 {
		t.Fatalf("implicit hybrid PV absence became zero: %+v", r)
	}
	tel.Update("hybrid", telemetry.DerPV, 0, nil, nil)
	tel.RecordDriverSuccess("hybrid")
	r = tel.ForecastMeasurement(time.Now(), "site", opts)
	if !r.Valid || !r.PVValid || r.HouseholdW != 1000 {
		t.Fatalf("fresh optional zero did not qualify: %+v", r)
	}
}
func TestForecastMeasurementKnownNeverEmittedSources(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 4000, nil, nil)
	tel.RecordDriverSuccess("site")
	for _, d := range []config.Driver{
		{Name: "missing", Lua: "drivers/hybrid.lua", BatteryCapacityWh: 10000, Config: map[string]any{"read_pv": false}},
		{Name: "missing", Lua: "drivers/solar.lua"},
	} {
		cfg := &config.Config{Drivers: []config.Driver{{Name: "site", Lua: "drivers/meter.lua", IsSiteMeter: true}, d}}
		if r := tel.ForecastMeasurement(time.Now(), "site", forecastMeasurementOptions(cfg, measurementCatalog())); r.Valid {
			t.Fatalf("known missing source accepted: %+v", d)
		}
	}
}
func TestForecastCatalogPathAndAmbiguousFallback(t *testing.T) {
	cat := []drivers.CatalogEntry{{Path: "managed/same.lua", Filename: "same.lua", Capabilities: []string{"pv"}}, {Path: "user/same.lua", Filename: "same.lua", Capabilities: []string{"vehicle"}}}
	if e, ok := forecastCatalogEntry(cat, "user/same.lua"); !ok || !forecastCapability(e, "vehicle") {
		t.Fatal("exact path lost")
	}
	if _, ok := forecastCatalogEntry(cat, "other/same.lua"); ok {
		t.Fatal("ambiguous fallback invented source")
	}
}
func TestForecastDeclaredPVWithoutSourceInvalidatesOnlyHouse(t *testing.T) {
	cfg := &config.Config{Drivers: []config.Driver{{Name: "site", Lua: "drivers/meter.lua", IsSiteMeter: true}}}
	cfg.Weather = &config.Weather{PVRatedW: 8000}
	opts := forecastMeasurementOptions(cfg, measurementCatalog())
	if opts.HouseholdInvalidReason == "" {
		t.Fatal("declared PV missing without qualification")
	}
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("site")
	tel.Update("independent", telemetry.DerPV, -3000, nil, nil)
	tel.RecordDriverSuccess("independent")
	r := tel.ForecastMeasurement(time.Now(), "site", opts)
	if r.Valid || !r.PVValid {
		t.Fatalf("house topology ambiguity affected independent PV: %+v", r)
	}
}
func TestForecastCatalogFailureRetainsProvisionalObservedFlows(t *testing.T) {
	cfg := &config.Config{Drivers: []config.Driver{{Name: "site", Lua: "custom/meter.lua", IsSiteMeter: true}}}
	opts := forecastMeasurementOptions(cfg, nil)
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("site")
	if r := tel.ForecastMeasurement(time.Now(), "site", opts); !r.Valid {
		t.Fatalf("catalog failure blocked coherent observed site: %+v", r)
	}
	if len(forecastMeasurementTopologyUnknown(cfg, nil)) == 0 {
		t.Fatal("unknown capabilities concealed")
	}
}
func TestForecastCurtailmentIntentAndRelease(t *testing.T) {
	now := time.Now()
	ctrl := &control.State{}
	if forecastCurtailmentActive(ctrl, now) {
		t.Fatal("empty state curtailed")
	}
	ctrl.ManualPVHold = control.PVManualHold{LimitW: 0, ExpiresAt: now.Add(time.Minute)}
	if !forecastCurtailmentActive(ctrl, now) {
		t.Fatal("manual zero not curtailed")
	}
	before := ctrl.ManualPVHold
	if forecastCurtailmentActive(ctrl, now.Add(time.Minute)) {
		t.Fatal("expired hold active")
	}
	if ctrl.ManualPVHold != before {
		t.Fatal("read mutated hold")
	}
	ctrl.ManualPVHold = control.PVManualHold{}
	ctrl.LastCurtailedDrivers = map[string]bool{"pv": true}
	if !forecastCurtailmentActive(ctrl, now) {
		t.Fatal("tracked curtailment lost")
	}
	ctrl.LastCurtailedDrivers = nil
	ctrl.SlotDirective = func(time.Time) (control.SlotDirective, bool) {
		return control.SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), PVLimitW: 100}, true
	}
	if !forecastCurtailmentActive(ctrl, now) || forecastCurtailmentActive(ctrl, now.Add(15*time.Minute)) {
		t.Fatal("slot interval boundary wrong")
	}
	ctrl.SlotDirective = func(time.Time) (control.SlotDirective, bool) { return control.SlotDirective{PVLimitW: 0}, true }
	if forecastCurtailmentActive(ctrl, now) {
		t.Fatal("planner zero should release")
	}
}

func TestForecastMeasurementConfiguredOCPPRequiresFirstPower(t *testing.T) {
	cfg := &config.Config{Drivers: []config.Driver{{Name: "site", Lua: "drivers/meter.lua", IsSiteMeter: true}, {Name: "pv", Lua: "drivers/solar.lua"}}, Loadpoints: []config.Loadpoint{{ID: "garage", DriverName: "ocpp-charger"}}}
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("site")
	tel.Update("pv", telemetry.DerPV, -2000, nil, nil)
	tel.RecordDriverSuccess("pv")
	opts := forecastMeasurementOptions(cfg, measurementCatalog())
	first := tel.ForecastMeasurement(time.Now(), "site", opts)
	if first.Valid || !first.PVValid {
		t.Fatalf("never-emitted EV treated as zero or blocked independent PV: %+v", first)
	}
	tel.Update("ocpp-charger", telemetry.DerEV, 0, nil, nil)
	tel.RecordDriverSuccess("ocpp-charger")
	zero := tel.ForecastMeasurement(time.Now(), "site", opts)
	if !zero.Valid || !zero.PVValid || zero.HouseholdW != 3000 {
		t.Fatalf("fresh known EV zero did not qualify: %+v", zero)
	}
	tel.Update("ocpp-charger", telemetry.DerEV, 600, nil, nil)
	charging := tel.ForecastMeasurement(time.Now(), "site", opts)
	if !charging.Valid || charging.HouseholdW != 2400 {
		t.Fatalf("charger load did not leave household balance: %+v", charging)
	}
}

func TestForecastMeasurementLoadpointDoesNotDuplicateLuaEVOrV2X(t *testing.T) {
	cfg := &config.Config{Drivers: []config.Driver{{Name: "lua-ev", Lua: "drivers/charger.lua"}, {Name: "lua-v2x", Lua: "drivers/v2x.lua"}}, Loadpoints: []config.Loadpoint{{ID: "one", DriverName: "lua-ev"}, {ID: "two", DriverName: "lua-v2x"}, {ID: "three", DriverName: "ocpp"}, {ID: "duplicate", DriverName: "ocpp"}, {ID: "empty"}}}
	opts := forecastMeasurementOptions(cfg, measurementCatalog())
	want := []telemetry.ForecastFlow{{Driver: "lua-ev", DerType: telemetry.DerEV}, {Driver: "lua-v2x", DerType: telemetry.DerV2X}, {Driver: "ocpp", DerType: telemetry.DerEV}}
	if !reflect.DeepEqual(opts.ExpectedFlows, want) {
		t.Fatalf("EV topology duplicated a physical stream: %+v", opts.ExpectedFlows)
	}
}

func TestForecastMeasurementLoadpointChangeChangesModelBinding(t *testing.T) {
	st := hostForecastDB(t)
	cfg := &config.Config{Drivers: []config.Driver{{Name: "site", Lua: "drivers/meter.lua", IsSiteMeter: true}}, Loadpoints: []config.Loadpoint{{ID: "garage", DriverName: "ocpp-first"}}}
	site := newForecastSiteConfig(st)
	site.Configure(cfg, measurementCatalog())
	before := site.Snapshot()
	cfg.Loadpoints[0].DriverName = "ocpp-replacement"
	site.Configure(cfg, measurementCatalog())
	after := site.Snapshot()
	if before.LearningRevision == after.LearningRevision || before.Revision == after.Revision {
		t.Fatal("adopted EV source changed without changing model/evaluation binding")
	}
}
