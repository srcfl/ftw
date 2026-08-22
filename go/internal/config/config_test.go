package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimalYAML = `
site:
  name: Test
fuse:
  max_amps: 16
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    capabilities:
      mqtt:
        host: 192.168.1.153
api:
  port: 8080
`

func TestPlannerPVSafetyKResolution(t *testing.T) {
	var nilPlanner *Planner
	if got := nilPlanner.PVSafetyK(); got != 1.0 {
		t.Errorf("nil Planner → 1.0, got %v", got)
	}
	if got := (&Planner{}).PVSafetyK(); got != 1.0 {
		t.Errorf("nil field → 1.0, got %v", got)
	}
	zero := 0.0
	if got := (&Planner{PVForecastSafetyK: &zero}).PVSafetyK(); got != 0 {
		t.Errorf("explicit 0 → 0 (no hedge), got %v", got)
	}
	two := 2.0
	if got := (&Planner{PVForecastSafetyK: &two}).PVSafetyK(); got != 2.0 {
		t.Errorf("explicit 2.0 → 2.0, got %v", got)
	}
}

func TestPlannerPVForecastSafetyKParsing(t *testing.T) {
	base := `
site:
  name: Test
fuse:
  max_amps: 16
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    capabilities:
      mqtt:
        host: 192.168.1.153
planner:
  mode: passive_arbitrage
`
	// Unset → nil pointer → default 1.0.
	c, err := Parse([]byte(base), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if c.Planner == nil {
		t.Fatal("planner block should parse")
	}
	if c.Planner.PVForecastSafetyK != nil {
		t.Errorf("unset pv_forecast_safety_k must stay nil, got %v", *c.Planner.PVForecastSafetyK)
	}
	if got := c.Planner.PVSafetyK(); got != 1.0 {
		t.Errorf("unset → PVSafetyK 1.0, got %v", got)
	}
	// Explicit 0 must parse to *0 (distinct from unset) and be honored.
	c0, err := Parse([]byte(base+"  pv_forecast_safety_k: 0\n"), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if c0.Planner.PVForecastSafetyK == nil || *c0.Planner.PVForecastSafetyK != 0 {
		t.Errorf("explicit 0 must parse to *0, got %v", c0.Planner.PVForecastSafetyK)
	}
	if got := c0.Planner.PVSafetyK(); got != 0 {
		t.Errorf("explicit 0 → PVSafetyK 0 (no hedge), got %v", got)
	}
	// Explicit non-default value.
	c25, err := Parse([]byte(base+"  pv_forecast_safety_k: 2.5\n"), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if got := c25.Planner.PVSafetyK(); got != 2.5 {
		t.Errorf("explicit 2.5 → PVSafetyK 2.5, got %v", got)
	}
}

func TestPlannerForecastTrustAndExportValidate(t *testing.T) {
	base := `
site:
  name: Test
fuse:
  max_amps: 16
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    capabilities:
      mqtt:
        host: 192.168.1.153
planner:
  mode: passive_arbitrage
`
	if _, err := Parse([]byte(base+"  forecast_trust: spicy\n"), "/tmp"); err == nil {
		t.Fatal("expected error for junk forecast_trust")
	}
	if _, err := Parse([]byte(base+"  battery_export: maybe\n"), "/tmp"); err == nil {
		t.Fatal("expected error for junk battery_export")
	}
	c, err := Parse([]byte(base+"  forecast_trust: cautious\n  battery_export: not_allowed\n"), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if c.Planner.ForecastTrust != "cautious" || c.Planner.BatteryExport != "not_allowed" {
		t.Fatalf("got trust=%q export=%q", c.Planner.ForecastTrust, c.Planner.BatteryExport)
	}
}

func TestLoadMinimalYAML(t *testing.T) {
	c, err := Parse([]byte(minimalYAML), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if c.Site.Name != "Test" {
		t.Errorf("site name: got %q", c.Site.Name)
	}
	// Defaults applied
	if c.Site.ControlIntervalS != 2 {
		t.Errorf("default control_interval_s: got %d, want 2", c.Site.ControlIntervalS)
	}
	if c.Site.GridToleranceW != 42 {
		t.Errorf("default grid_tolerance_w: got %f", c.Site.GridToleranceW)
	}
	if c.Fuse.Phases != 3 {
		t.Errorf("default fuse phases: got %d", c.Fuse.Phases)
	}
	if c.API.Port != 8080 {
		t.Errorf("api port: got %d", c.API.Port)
	}
	if c.Drivers[0].Capabilities.MQTT.Port != 1883 {
		t.Errorf("mqtt default port: got %d", c.Drivers[0].Capabilities.MQTT.Port)
	}
}

func TestAllowUnverifiedLocalDefaultsDenyAndParsesExplicitOptIn(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Drivers[0].Capabilities.AllowUnverifiedLocal {
		t.Fatal("allow_unverified_local must default to false")
	}

	withOptIn := strings.Replace(minimalYAML,
		"capabilities:\n      mqtt:",
		"capabilities:\n      allow_unverified_local: true\n      mqtt:", 1)
	optedIn, err := Parse([]byte(withOptIn), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if !optedIn.Drivers[0].Capabilities.AllowUnverifiedLocal {
		t.Fatal("explicit allow_unverified_local=true was not retained")
	}
}

func TestHomeAssistantAllowUnverifiedLocalDefaultsDeny(t *testing.T) {
	base := minimalYAML + `
homeassistant:
  enabled: true
  broker: broker.local
`
	cfg, err := Parse([]byte(base), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HomeAssistant.AllowUnverifiedLocal {
		t.Fatal("homeassistant allow_unverified_local must default to false")
	}

	optedIn, err := Parse([]byte(base+"  allow_unverified_local: true\n"), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if !optedIn.HomeAssistant.AllowUnverifiedLocal {
		t.Fatal("homeassistant explicit local opt-in was not retained")
	}
}

func TestParseIgnoresRetiredRemoteAccessKeys(t *testing.T) {
	legacy := minimalYAML + `
remote_access:
  enabled: true
  turn:
    enabled: true
    url: turn:turn.example.test
fleet_statistics:
  enabled: true
  endpoint: https://relay.example.test/fleet/heartbeat
  interval_h: 24
home_link:
  enabled: true
`
	if _, err := Parse([]byte(legacy), "/tmp"); err != nil {
		t.Fatalf("existing config with retired remote keys must keep loading: %v", err)
	}
}

func TestSiteTroubleshootingModeParses(t *testing.T) {
	raw := strings.Replace(minimalYAML, "name: Test", "name: Test\n  troubleshooting_mode: true", 1)
	c, err := Parse([]byte(raw), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Site.TroubleshootingMode {
		t.Fatal("expected troubleshooting_mode=true")
	}
}

// TestDeprecatedUseEnergyDispatchParsesAsPointer covers the
// Codex P1 on PR #124: an operator who explicitly set
// `use_energy_dispatch: false` to pick legacy dispatch pre-v0.27
// must not be silently flipped to the energy path on upgrade. The
// field lives on as a deprecated *bool so main.go can distinguish
// "unset" (nil) from "explicitly false" and honor prior intent.
func TestDeprecatedUseEnergyDispatchParsesAsPointer(t *testing.T) {
	yaml := minimalYAML + `
planner:
  enabled: true
  use_energy_dispatch: false
`
	c, err := Parse([]byte(yaml), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if c.Planner == nil {
		t.Fatal("planner not parsed")
	}
	if c.Planner.UseEnergyDispatch == nil {
		t.Fatal("UseEnergyDispatch should be non-nil when key is present")
	}
	if *c.Planner.UseEnergyDispatch != false {
		t.Errorf("UseEnergyDispatch = %v, want false", *c.Planner.UseEnergyDispatch)
	}
}

func TestUseEnergyDispatchNilWhenUnset(t *testing.T) {
	yaml := minimalYAML + `
planner:
  enabled: true
`
	c, err := Parse([]byte(yaml), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if c.Planner.UseEnergyDispatch != nil {
		t.Errorf("UseEnergyDispatch should be nil when YAML omits the key, got %v", *c.Planner.UseEnergyDispatch)
	}
}

func TestV2XPolicyParses(t *testing.T) {
	yaml := minimalYAML + `
v2x:
  enabled: true
  driver_name: ferroamp
  vehicle_capacity_wh: 77000
  min_reserve_soc_pct: 35
  departure_target_soc_pct: 80
  departure_time: "07:30"
  max_charge_w: 7000
  max_discharge_w: 5000
  export_allowed: false
  grid_charging_allowed: false
  cycle_cost_ore_kwh: 12
`
	c, err := Parse([]byte(yaml), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if c.V2X == nil {
		t.Fatal("v2x policy not parsed")
	}
	if !c.V2X.Enabled || c.V2X.DriverName != "ferroamp" || c.V2X.MinReserveSoC != 0.35 {
		t.Fatalf("unexpected v2x policy: %+v", c.V2X)
	}
}

func TestV2XPolicyValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "enabled without reserve",
			body: `
v2x:
  enabled: true
`,
		},
		{
			name: "unknown driver",
			body: `
v2x:
  enabled: false
  driver_name: missing
`,
		},
		{
			name: "bad departure time",
			body: `
v2x:
  enabled: false
  departure_target_soc_pct: 80
  departure_time: soon
`,
		},
		{
			name: "target without departure",
			body: `
v2x:
  enabled: false
  departure_target_soc_pct: 80
`,
		},
		{
			name: "negative discharge limit",
			body: `
v2x:
  enabled: false
  max_discharge_w: -1
`,
		},
		{
			name: "departure target below reserve",
			body: `
v2x:
  enabled: true
  min_reserve_soc_pct: 50
  departure_target_soc_pct: 40
  departure_time: "08:00"
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(minimalYAML+tc.body), "/tmp"); err == nil {
				t.Fatal("expected v2x validation error")
			}
		})
	}
}

func TestV2XPolicyDepartureTargetAtOrAboveReserveOK(t *testing.T) {
	body := `
v2x:
  enabled: true
  min_reserve_soc_pct: 40
  departure_target_soc_pct: 40
  departure_time: "08:00"
`
	if _, err := Parse([]byte(minimalYAML+body), "/tmp"); err != nil {
		t.Fatalf("target == reserve should pass, got: %v", err)
	}
}

func TestLoadpointSurplusOnlyParses(t *testing.T) {
	yaml := minimalYAML + `
loadpoints:
  - id: garage
    driver_name: easee
    surplus_only: true
`
	c, err := Parse([]byte(yaml), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Loadpoints) != 1 {
		t.Fatalf("loadpoints = %d, want 1", len(c.Loadpoints))
	}
	if !c.Loadpoints[0].SurplusOnly {
		t.Fatal("loadpoint surplus_only was not parsed")
	}
}

func TestRelativeDriverPathResolved(t *testing.T) {
	c, err := Parse([]byte(minimalYAML), "/base/dir")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/base/dir", "drivers/ferroamp.lua")
	if c.Drivers[0].Lua != want {
		t.Errorf("lua path: got %s want %s", c.Drivers[0].Lua, want)
	}
}

func TestAcceptsNoDrivers(t *testing.T) {
	// EV-only sites (cloud charger configured via the setup wizard Step 7
	// without any local hardware) ship an empty drivers list; validator
	// must accept it. Control loop becomes a no-op at runtime.
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers: []
api: { port: 8080 }
`
	if _, err := Parse([]byte(yaml), "."); err != nil {
		t.Fatalf("expected empty drivers to be accepted, got: %v", err)
	}
}

func TestRejectsNoSiteMeter(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers:
  - name: a
    lua: a.lua
    capabilities:
      mqtt: { host: 1.1.1.1 }
api: { port: 8080 }
`
	_, err := Parse([]byte(yaml), ".")
	if err == nil {
		t.Fatal("expected error for no site meter")
	}
}

func TestRejectsDuplicateDriverNames(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers:
  - name: a
    lua: a.lua
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
  - name: a
    lua: b.lua
    capabilities: { mqtt: { host: 2.2.2.2 } }
api: { port: 8080 }
`
	if _, err := Parse([]byte(yaml), "."); err == nil {
		t.Fatal("expected error for duplicate names")
	}
}

func TestRejectsDriverWithoutProtocol(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers:
  - name: a
    lua: a.lua
    is_site_meter: true
api: { port: 8080 }
`
	if _, err := Parse([]byte(yaml), "."); err == nil {
		t.Fatal("expected error for driver without protocol")
	}
}

func TestRejectsDriverWithoutLua(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers:
  - name: a
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
api: { port: 8080 }
`
	if _, err := Parse([]byte(yaml), "."); err == nil {
		t.Fatal("expected error for driver without lua")
	}
}

func TestLegacyMqttFallsBackToCapabilities(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers:
  - name: a
    lua: a.lua
    is_site_meter: true
    mqtt: { host: 192.168.1.100, username: ext }
api: { port: 8080 }
`
	c, err := Parse([]byte(yaml), ".")
	if err != nil {
		t.Fatal(err)
	}
	mq := c.Drivers[0].EffectiveMQTT()
	if mq == nil || mq.Host != "192.168.1.100" || mq.Username != "ext" {
		t.Errorf("legacy mqtt fallback failed: %+v", mq)
	}
}

func TestFuseMaxPower(t *testing.T) {
	f := Fuse{MaxAmps: 16, Phases: 3, Voltage: 230}
	want := 16.0 * 230 * 3
	if f.MaxPowerW() != want {
		t.Errorf("fuse power: got %f want %f", f.MaxPowerW(), want)
	}
}

func TestRejectsInvalidFusePowerInputs(t *testing.T) {
	cases := []struct {
		field string
		value string
	}{
		{"max_amps", "-16"},
		{"phases", "-3"},
		{"voltage", "-230"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			yaml := fmt.Sprintf(`
site: { name: x }
fuse: { max_amps: 16, phases: 3, voltage: 230, %s: %s }
drivers:
  - name: a
    lua: a.lua
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
api: { port: 8080 }
`, tc.field, tc.value)
			if _, err := Parse([]byte(yaml), "."); err == nil {
				t.Fatalf("expected validation error for fuse.%s=%s", tc.field, tc.value)
			}
		})
	}
}

func TestSmoothingAlphaValidation(t *testing.T) {
	// alpha=0 means "use default" via applyDefaults, so only test truly invalid values
	for _, bad := range []float64{-0.1, 1.1, 2.0} {
		yaml := `
site: { name: x, smoothing_alpha: ` + pretty(bad) + ` }
fuse: { max_amps: 16 }
drivers:
  - name: a
    lua: a.lua
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
api: { port: 8080 }
`
		if _, err := Parse([]byte(yaml), "."); err == nil {
			t.Errorf("alpha=%v should fail validation", bad)
		}
	}
}

func TestRejectsNegativeSiteControlValues(t *testing.T) {
	cases := []struct {
		field string
		value string
	}{
		{"control_interval_s", "-1"},
		{"grid_tolerance_w", "-1"},
		{"watchdog_timeout_s", "-1"},
		{"gain", "-0.1"},
		{"slew_rate_w", "-500"},
		{"min_dispatch_interval_s", "-1"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			yaml := fmt.Sprintf(`
site: { name: x, %s: %s }
fuse: { max_amps: 16 }
drivers:
  - name: a
    lua: a.lua
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
api: { port: 8080 }
`, tc.field, tc.value)
			if _, err := Parse([]byte(yaml), "."); err == nil {
				t.Fatalf("expected validation error for site.%s=%s", tc.field, tc.value)
			}
		})
	}
}

func TestAllOptionalSectionsParse(t *testing.T) {
	yaml := `
site: { name: Full }
fuse: { max_amps: 16 }
drivers:
  - name: f
    lua: f.lua
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
api: { port: 8080 }
homeassistant:
  enabled: true
  broker: 192.168.1.1
state:
  path: state.db
price:
  provider: sourceful
  zone: SE3
  vat_percent: 25
weather:
  provider: met_no
  latitude: 59.3293
  longitude: 18.0686
batteries:
  f:
    soc_min: 0.1
    weight: 2.0
`
	c, err := Parse([]byte(yaml), ".")
	if err != nil {
		t.Fatal(err)
	}
	if c.HomeAssistant == nil || !c.HomeAssistant.Enabled {
		t.Error("homeassistant section missing")
	}
	if c.Price == nil || c.Price.Zone != "SE3" {
		t.Error("price section missing")
	}
	if c.Weather == nil || c.Weather.Latitude != 59.3293 {
		t.Error("weather section missing")
	}
	if c.Batteries["f"].SoCMin == nil || *c.Batteries["f"].SoCMin != 0.1 {
		t.Error("battery override missing")
	}
}

func TestPVArrayGeometryDistinguishesMissingFromZero(t *testing.T) {
	yaml := minimalYAML + `
weather:
  provider: open_meteo
  latitude: 59.3293
  longitude: 18.0686
  pv_arrays:
    - name: partial
      kwp: 10
      tilt_deg: 35
    - name: north flat
      kwp: 5
      tilt_deg: 0
      azimuth_deg: 0
`
	c, err := Parse([]byte(yaml), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if c.Weather == nil || len(c.Weather.PVArrays) != 2 {
		t.Fatalf("weather arrays missing: %+v", c.Weather)
	}
	partial := c.Weather.PVArrays[0]
	if partial.AzimuthDeg != nil {
		t.Fatalf("omitted azimuth should remain nil, got %v", *partial.AzimuthDeg)
	}
	if _, _, _, ok := partial.CompleteGeometry(); ok {
		t.Fatal("partial geometry must not be treated as a north-facing array")
	}
	northFlat := c.Weather.PVArrays[1]
	tilt, azimuth, ratedW, ok := northFlat.CompleteGeometry()
	if !ok || tilt != 0 || azimuth != 0 || ratedW != 5000 {
		t.Fatalf("explicit zero geometry should remain valid: tilt=%v azimuth=%v ratedW=%v ok=%v", tilt, azimuth, ratedW, ok)
	}
}

func TestSiteMeterDriverReturnsName(t *testing.T) {
	c, err := Parse([]byte(minimalYAML), ".")
	if err != nil {
		t.Fatal(err)
	}
	if c.SiteMeterDriver() != "ferroamp" {
		t.Errorf("SiteMeterDriver: got %q", c.SiteMeterDriver())
	}
}

func TestSaveAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	c, _ := Parse([]byte(minimalYAML), dir)
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Site.Name != c.Site.Name {
		t.Errorf("roundtrip site.name: got %q", c2.Site.Name)
	}
}

func TestSaveAtomicDoesNotLeaveTmpFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	c, err := Parse([]byte(minimalYAML), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("SaveAtomic left tmp file behind: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("saved config missing: %v", err)
	}
}

func TestSaveAtomicKeepsOutOfTreeDriverPathAbsolute(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	c, err := Parse([]byte(minimalYAML), dir)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "external.lua")
	c.Drivers[0].Lua = outside
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Drivers[0].Lua != outside {
		t.Fatalf("driver path after save/load = %q, want original absolute %q", loaded.Drivers[0].Lua, outside)
	}
}

// config.yaml holds MQTT passwords, API keys and OAuth refresh tokens. Rename
// replaces the destination inode, so the temp file's mode is the mode the
// operator ends up with — including when the config on disk was already
// world-readable, or when an interrupted save left a world-readable temp
// behind for the next save to reuse.
func TestSaveAtomicWritesOwnerOnlyMode(t *testing.T) {
	tests := []struct {
		name string
		prep func(t *testing.T, path string)
	}{
		{
			name: "new config file",
			prep: func(*testing.T, string) {},
		},
		{
			name: "replacing a world-readable config",
			prep: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("site:\n  name: old\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "stale world-readable tmp from an interrupted save",
			prep: func(t *testing.T, path string) {
				if err := os.WriteFile(path+".tmp", []byte("half a config"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "c.yaml")
			c, err := Parse([]byte(minimalYAML), dir)
			if err != nil {
				t.Fatal(err)
			}
			tt.prep(t, path)
			if err := SaveAtomic(path, c); err != nil {
				t.Fatal(err)
			}
			if err := verifyConfigFileOwnerOnly(path); err != nil {
				t.Errorf("saved config is not owner-only — the file holds MQTT passwords and OAuth refresh tokens: %v", err)
			}
			if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
				t.Errorf("tmp file survived the save: %v", err)
			}
		})
	}
}

// A rename is only atomic for bytes that already reached the disk, and the
// rename itself only survives power loss once the directory entry is synced.
// Both syncs must happen, and they must straddle the rename in that order.
func TestSaveAtomicSyncsFileBeforeRenameAndDirAfter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	c, err := Parse([]byte(minimalYAML), dir)
	if err != nil {
		t.Fatal(err)
	}
	saved := func() bool {
		_, err := os.Stat(path)
		return err == nil
	}
	var order []string
	var savedAtFileSync, savedAtDirSync bool
	w := durableWriter{
		syncFile: func(f *os.File) error {
			order = append(order, "file")
			savedAtFileSync = saved()
			return f.Sync()
		},
		syncDir: func(d string) error {
			order = append(order, "dir")
			savedAtDirSync = saved()
			if d != dir {
				t.Errorf("syncDir got %q, want the config's directory %q", d, dir)
			}
			return syncDir(d)
		},
	}
	if err := saveAtomic(w, path, c); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "file,dir" {
		t.Fatalf("sync order = [%s], want [file,dir]", got)
	}
	if savedAtFileSync {
		t.Error("the temp file was fsynced after the rename; a power cut could publish a truncated config")
	}
	if !savedAtDirSync {
		t.Error("the directory was fsynced before the rename; the rename itself would not be durable")
	}
}

// The caller's contract is "the config is now saved". A sync that fails must
// not report success, or the settings UI tells the operator a change landed
// that the next power cut can still take away.
func TestSaveAtomicReportsSyncFailure(t *testing.T) {
	syncFailed := errors.New("no space left on device")
	const oldConfig = "site:\n  name: previous\n"
	tests := []struct {
		name        string
		writer      durableWriter
		keepsOldCfg bool
	}{
		{
			name: "temp file sync fails",
			writer: durableWriter{
				syncFile: func(*os.File) error { return syncFailed },
				syncDir:  syncDir,
			},
			// The rename never ran, so the config the gateway boots from is
			// still the one that was there before.
			keepsOldCfg: true,
		},
		{
			name: "directory sync fails",
			writer: durableWriter{
				syncFile: (*os.File).Sync,
				syncDir:  func(string) error { return syncFailed },
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "c.yaml")
			c, err := Parse([]byte(minimalYAML), dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(oldConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			err = saveAtomic(tt.writer, path, c)
			if !errors.Is(err, syncFailed) {
				t.Fatalf("saveAtomic error = %v, want it to report %v", err, syncFailed)
			}
			if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
				t.Errorf("tmp file survived a failed save: %v", err)
			}
			if tt.keepsOldCfg {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != oldConfig {
					t.Errorf("config on disk = %q, want the previous config left untouched", got)
				}
			}
		})
	}
}

func pretty(f float64) string {
	return fmt.Sprintf("%g", f)
}

// The path-normalization helpers pulled in with the EV cloud driver PR
// have three separate jobs that can silently conflict: stripLeadingDotDot
// removes "../" prefixes, ResolveDriverPaths joins relative paths against
// baseDir, and UnresolveDriverPaths goes back to config-relative form
// before the YAML hits disk. The interesting failure is the pair —
// Unresolve followed by Resolve must be the identity, including when the
// driver file lives OUTSIDE baseDir (Copilot #11). Without the
// out-of-tree guard, an absolute path like /opt/drivers/foo.lua round-
// trips to "../opt/drivers/foo.lua" → stripLeadingDotDot → "opt/drivers/
// foo.lua" → baseDir-joined to the wrong place.
func TestStripLeadingDotDot(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"drivers/x.lua":          "drivers/x.lua",
		"../drivers/x.lua":       "drivers/x.lua",
		"../../../drivers/x.lua": "drivers/x.lua",
		"/abs/drivers/x.lua":     "/abs/drivers/x.lua",
		"/etc/../driver/foo.lua": "/etc/../driver/foo.lua", // non-leading "../" preserved
	}
	for in, want := range cases {
		if got := stripLeadingDotDot(in); got != want {
			t.Errorf("stripLeadingDotDot(%q): got %q, want %q", in, got, want)
		}
	}
}

func testAbsolutePath(p string) string {
	got, err := filepath.Abs(filepath.FromSlash(p))
	if err != nil {
		panic(err)
	}
	return got
}

func TestResolveDriverPaths(t *testing.T) {
	baseDir := testAbsolutePath("/etc/ftw")
	absin := testAbsolutePath("/etc/ftw/drivers/b.lua")
	absout := testAbsolutePath("/opt/drivers/c.lua")
	c := &Config{Drivers: []Driver{
		{Name: "rel", Lua: "drivers/a.lua"},
		{Name: "absin", Lua: absin},
		{Name: "absout", Lua: absout},
		{Name: "escape", Lua: "../../secrets/d.lua"},
		{Name: "empty"},
	}}
	c.ResolveDriverPaths(baseDir)
	want := map[string]string{
		"rel":    filepath.Join(baseDir, "drivers", "a.lua"), // joined with baseDir
		"absin":  absin,                                      // already absolute, untouched
		"absout": absout,                                     // absolute outside baseDir, untouched
		"escape": filepath.Join(baseDir, "secrets", "d.lua"), // leading "../" stripped, then joined
		"empty":  "",
	}
	for _, d := range c.Drivers {
		if d.Lua != want[d.Name] {
			t.Errorf("resolve %s: got %q, want %q", d.Name, d.Lua, want[d.Name])
		}
	}
}

func TestUnresolveDriverPathsRoundtrip(t *testing.T) {
	baseDir := testAbsolutePath("/etc/ftw")
	absin := testAbsolutePath("/etc/ftw/drivers/b.lua")
	absout := testAbsolutePath("/opt/drivers/c.lua")
	original := []Driver{
		{Name: "rel", Lua: "drivers/a.lua"},
		{Name: "absin", Lua: absin},   // absolute but inside baseDir
		{Name: "absout", Lua: absout}, // absolute outside baseDir — must stay absolute
		{Name: "empty"},
	}
	c := &Config{Drivers: append([]Driver(nil), original...)}
	c.ResolveDriverPaths(baseDir)
	c.UnresolveDriverPaths(baseDir)

	// After Unresolve, relative / in-tree absolute paths collapse back
	// to baseDir-relative; out-of-tree absolutes must stay absolute so
	// the next Resolve doesn't strip a "../" from filepath.Rel and
	// silently re-anchor the driver under baseDir (Copilot #11).
	got := map[string]string{}
	for _, d := range c.Drivers {
		got[d.Name] = d.Lua
	}
	if got["rel"] != filepath.Join("drivers", "a.lua") {
		t.Errorf("rel: got %q, want %s", got["rel"], filepath.Join("drivers", "a.lua"))
	}
	if got["absin"] != filepath.Join("drivers", "b.lua") {
		t.Errorf("absin: got %q, want %s", got["absin"], filepath.Join("drivers", "b.lua"))
	}
	if got["absout"] != absout {
		t.Errorf("absout: got %q, want %s (must remain absolute)", got["absout"], absout)
	}
	if got["empty"] != "" {
		t.Errorf("empty: got %q, want empty string", got["empty"])
	}

	// Re-resolving must produce the same absolute paths as the first
	// Resolve — the UI save/load cycle relies on this being a fixed point.
	c.ResolveDriverPaths(baseDir)
	want := map[string]string{
		"rel":    filepath.Join(baseDir, "drivers", "a.lua"),
		"absin":  absin,
		"absout": absout,
		"empty":  "",
	}
	for _, d := range c.Drivers {
		if d.Lua != want[d.Name] {
			t.Errorf("re-resolve %s: got %q, want %q", d.Name, d.Lua, want[d.Name])
		}
	}
}

func TestSlewDefaults(t *testing.T) {
	c := &Config{}
	applyDefaults(c)
	if c.Site.SlewRateW != 3000 {
		t.Errorf("default slew_rate_w: got %f, want 3000", c.Site.SlewRateW)
	}
	if c.Site.SlewEnabled == nil || *c.Site.SlewEnabled != true {
		t.Errorf("default slew_enabled: got %v, want *true", c.Site.SlewEnabled)
	}
}

func TestSlewExplicitDisablePreserved(t *testing.T) {
	f := false
	c := &Config{Site: Site{SlewEnabled: &f}}
	applyDefaults(c)
	if c.Site.SlewEnabled == nil || *c.Site.SlewEnabled != false {
		t.Errorf("explicit slew_enabled=false must survive applyDefaults, got %v", c.Site.SlewEnabled)
	}
}

func TestAppLinkDefaultsOnAndPreservesOptOut(t *testing.T) {
	absent := &Config{}
	applyDefaults(absent)
	if absent.AppLink == nil || !absent.AppLink.Enabled {
		t.Fatalf("omitted app_link must default on, got %+v", absent.AppLink)
	}

	disabled := &Config{AppLink: &AppLink{Enabled: false}}
	applyDefaults(disabled)
	if disabled.AppLink.Enabled {
		t.Fatal("explicit app_link opt-out was overwritten")
	}
}

func TestParseAppLinkPreservesExplicitNullOptOut(t *testing.T) {
	tests := []struct {
		name    string
		suffix  string
		enabled bool
	}{
		{name: "omitted", enabled: true},
		{name: "bare null", suffix: "app_link:\n", enabled: false},
		{name: "named null", suffix: "app_link: null\n", enabled: false},
		{name: "null alias", suffix: "disabled: &disabled null\napp_link: *disabled\n", enabled: false},
		{name: "null with ignored complex key", suffix: "legacy:\n  ? [old, key]\n  : ignored\napp_link: null\n", enabled: false},
		{name: "merged null", suffix: "defaults: &defaults\n  app_link: null\n<<: *defaults\n", enabled: false},
		{name: "merged sequence first null", suffix: "off: &off {app_link: null}\non: &on {app_link: {enabled: true}}\n<<: [*off, *on]\n", enabled: false},
		{name: "merged sequence first true", suffix: "off: &off {app_link: null}\non: &on {app_link: {enabled: true}}\n<<: [*on, *off]\n", enabled: true},
		{name: "direct true overrides merged null", suffix: "off: &off {app_link: null}\n<<: *off\napp_link: {enabled: true}\n", enabled: true},
		{name: "empty mapping", suffix: "app_link: {}\n", enabled: false},
		{name: "explicit false", suffix: "app_link:\n  enabled: false\n", enabled: false},
		{name: "explicit true", suffix: "app_link:\n  enabled: true\n", enabled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(minimalYAML+tt.suffix), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.AppLink.On(); got != tt.enabled {
				t.Fatalf("app link enabled = %v, want %v", got, tt.enabled)
			}
		})
	}
}

func TestNotificationsDefaults(t *testing.T) {
	c := &Config{Notifications: &Notifications{Enabled: false}}
	applyDefaults(c)
	if c.Notifications.Provider != "ntfy" {
		t.Errorf("provider default: got %q", c.Notifications.Provider)
	}
	if c.Notifications.Ntfy == nil || c.Notifications.Ntfy.Server != "https://ntfy.sh" {
		t.Errorf("ntfy.server default: got %+v", c.Notifications.Ntfy)
	}
	if c.Notifications.DefaultPriority != 3 {
		t.Errorf("default_priority default: got %d", c.Notifications.DefaultPriority)
	}
}

// validFuse is a complete, safe fuse block so notification tests fail on the
// notification rule under test, not on an unrelated fuse check that runs first.
func validFuse() Fuse { return Fuse{MaxAmps: 16, Phases: 1, Voltage: 230} }

// A real box stores the legacy default: provider "ntfy", server set to the
// public host, and no topic (it was never entered). Web push is engine-owned,
// so enabling notifications must succeed on such a box — the topic-less ntfy
// is inactive, not a fatal config error. Holds whether the provider field is
// the legacy "ntfy" or the newer "".
func TestNotificationsEnableWithIncompleteNtfySucceeds(t *testing.T) {
	for _, provider := range []string{"ntfy", ""} {
		c := &Config{
			Site:          Site{SmoothingAlpha: 0.3},
			Fuse:          validFuse(),
			Notifications: &Notifications{Enabled: true, Provider: provider, Ntfy: &NtfyConfig{Server: "https://ntfy.sh", Topic: ""}},
		}
		if err := c.Validate(); err != nil {
			t.Errorf("provider %q: unexpected error enabling with topic-less ntfy: %v", provider, err)
		}
	}
}

// A box that genuinely configured ntfy — server and topic both set — stays
// valid, and the runtime still selects the ntfy transport.
func TestNotificationsEnableWithCompleteNtfyValid(t *testing.T) {
	c := &Config{
		Site:          Site{SmoothingAlpha: 0.3},
		Fuse:          validFuse(),
		Notifications: &Notifications{Enabled: true, Provider: "ntfy", Ntfy: &NtfyConfig{Server: "https://ntfy.sh", Topic: "my-topic"}},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected error for complete ntfy: %v", err)
	}
}

// A topic with no server to publish it to is a half-finished, hand-edited
// ntfy — a real mistake worth catching rather than silently dropping.
func TestNotificationsNtfyTopicWithoutServerErrors(t *testing.T) {
	for _, provider := range []string{"ntfy", ""} {
		c := &Config{
			Site:          Site{SmoothingAlpha: 0.3},
			Fuse:          validFuse(),
			Notifications: &Notifications{Enabled: true, Provider: provider, Ntfy: &NtfyConfig{Server: "", Topic: "my-topic"}},
		}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "notifications.ntfy.server required") {
			t.Errorf("provider %q: want ntfy.server-required error, got %v", provider, err)
		}
	}
}

func TestNotificationsValidateRejectsBadPriority(t *testing.T) {
	c := &Config{
		Site:          Site{SmoothingAlpha: 0.3},
		Fuse:          Fuse{MaxAmps: 16},
		Notifications: &Notifications{Enabled: false, DefaultPriority: 9},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error for default_priority=9")
	}
}

func TestNotificationsDisabledPartialPasses(t *testing.T) {
	c := &Config{
		Site:          Site{SmoothingAlpha: 0.3},
		Fuse:          Fuse{MaxAmps: 16},
		Notifications: &Notifications{Enabled: false, Ntfy: &NtfyConfig{Topic: ""}},
	}
	applyDefaults(c)
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNotificationsValidateRejectsUnknownProvider(t *testing.T) {
	c := &Config{
		Site:          Site{SmoothingAlpha: 0.3},
		Fuse:          Fuse{MaxAmps: 16},
		Notifications: &Notifications{Enabled: true, Provider: "slack"},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestDeviceRepositoryUnsignedManifestMustBeLocal(t *testing.T) {
	base := Config{Site: Site{SmoothingAlpha: 0.3}, Fuse: Fuse{MaxAmps: 16}}
	base.DeviceRepository = &DeviceRepository{Enabled: true, Repositories: []DriverRepositorySource{{
		ID: "dev", Enabled: true, ManifestURL: "https://example.test/manifest.json", AllowUnsigned: true,
	}}}
	applyDefaults(&base)
	if err := base.Validate(); err == nil {
		t.Fatal("unsigned remote manifest accepted")
	}
	base.DeviceRepository.Repositories[0].ManifestURL = "file:///tmp/ftw-driver-manifest.json"
	base.DeviceRepository.Repositories[0].AllowInsecure = true
	if err := base.Validate(); err != nil {
		t.Fatalf("local unsigned development manifest rejected: %v", err)
	}
}

func TestDeviceRepositoryOfficialTrustRootIsReadOnlyDiscoveryDefault(t *testing.T) {
	absent := Config{Site: Site{SmoothingAlpha: 0.3}, Fuse: Fuse{MaxAmps: 16}}
	applyDefaults(&absent)
	if absent.DeviceRepository == nil || !absent.DeviceRepository.Enabled {
		t.Fatal("omitted device_repository must enable signed read-only discovery")
	}
	if len(absent.DeviceRepository.Repositories) != 1 {
		t.Fatalf("default repositories = %+v", absent.DeviceRepository.Repositories)
	}
	repo := absent.DeviceRepository.Repositories[0]
	if repo.ID != DefaultDriverRepositoryID || repo.ManifestURL != DefaultDriverRepositoryManifestURL ||
		repo.TrustedKeys[DefaultDriverRepositorySigningKeyID] != DefaultDriverRepositoryPublicKey {
		t.Fatalf("official repository default = %+v", repo)
	}
	if err := absent.Validate(); err != nil {
		t.Fatalf("official repository default rejected: %v", err)
	}
	disabled := Config{DeviceRepository: &DeviceRepository{Enabled: false}}
	applyDefaults(&disabled)
	if disabled.DeviceRepository.Enabled {
		t.Fatal("explicit repository opt-out was overwritten")
	}
}

func TestDeviceRepositoryMigratesOnlyTheExactFormerFTWSource(t *testing.T) {
	legacy := DriverRepositorySource{
		ID: DefaultDriverRepositoryID, Name: legacyDriverRepositoryName,
		ManifestURL: legacyDriverRepositoryManifestURL, Enabled: false,
		TrustedKeys: map[string]string{
			DefaultDriverRepositorySigningKeyID: DefaultDriverRepositoryPublicKey,
		},
	}
	cfg := Config{DeviceRepository: &DeviceRepository{
		Enabled: false, RootDir: "kept", RefreshIntervalH: 12,
		Repositories: []DriverRepositorySource{legacy},
	}}
	applyDefaults(&cfg)
	got := cfg.DeviceRepository.Repositories[0]
	if got.ManifestURL != DefaultDriverRepositoryManifestURL || got.Name != DefaultDriverRepositoryName {
		t.Fatalf("former FTW source was not migrated: %+v", got)
	}
	if got.Enabled || cfg.DeviceRepository.Enabled || cfg.DeviceRepository.RootDir != "kept" || cfg.DeviceRepository.RefreshIntervalH != 12 {
		t.Fatalf("repository settings changed during migration: %+v", cfg.DeviceRepository)
	}

	custom := legacy
	custom.Name = "My pinned mirror"
	cfg.DeviceRepository.Repositories[0] = custom
	applyDefaults(&cfg)
	if cfg.DeviceRepository.Repositories[0].ManifestURL != legacyDriverRepositoryManifestURL {
		t.Fatalf("custom source was migrated: %+v", cfg.DeviceRepository.Repositories[0])
	}
}

func TestSerialAndStandaloneDriverCapabilities(t *testing.T) {
	serialCfg := Config{
		Site: Site{SmoothingAlpha: 0.3}, Fuse: Fuse{MaxAmps: 16},
		Drivers: []Driver{{
			Name: "p1", Lua: "p1.lua", IsSiteMeter: true,
			Capabilities: Capabilities{Serial: &SerialConfig{Address: "/dev/ttyUSB0"}},
		}},
	}
	applyDefaults(&serialCfg)
	serial := serialCfg.Drivers[0].Capabilities.Serial
	if serial.BaudRate != 115200 || serial.DataBits != 8 || serial.StopBits != 1 ||
		serial.Parity != "N" || serial.ReadTimeoutMS != 500 {
		t.Fatalf("serial defaults = %+v", serial)
	}
	if err := serialCfg.Validate(); err != nil {
		t.Fatalf("serial driver rejected: %v", err)
	}

	standalone := serialCfg
	standalone.Drivers = []Driver{{
		Name: "local", Lua: "local.lua", IsSiteMeter: true,
		Capabilities: Capabilities{Standalone: true},
	}}
	if err := standalone.Validate(); err != nil {
		t.Fatalf("standalone driver rejected: %v", err)
	}
}

func TestDeviceRepositorySourcefulFormatMustBeSignedAndKnown(t *testing.T) {
	base := Config{Site: Site{SmoothingAlpha: 0.3}, Fuse: Fuse{MaxAmps: 16}}
	base.DeviceRepository = &DeviceRepository{Enabled: true, Repositories: []DriverRepositorySource{{
		ID: "sourceful", Format: DriverRepositoryFormatSourcefulIndexV1,
		ManifestURL: "file:///tmp/sourceful-driver-index.json", Enabled: true,
		AllowInsecure: true, AllowUnsigned: true,
	}}}
	applyDefaults(&base)
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "must be signed") {
		t.Fatalf("unsigned Sourceful index error = %v", err)
	}
	base.DeviceRepository.Repositories[0].AllowUnsigned = false
	base.DeviceRepository.Repositories[0].TrustedKeys = map[string]string{"test": strings.Repeat("A", 44)}
	if err := base.Validate(); err != nil {
		t.Fatalf("signed Sourceful source rejected: %v", err)
	}
	base.DeviceRepository.Repositories[0].Format = "sourceful.driver-index/v9"
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported format") {
		t.Fatalf("unknown repository format error = %v", err)
	}
}

func TestNotificationsMaskSecrets(t *testing.T) {
	c := Config{Notifications: &Notifications{
		Provider: "ntfy",
		Ntfy:     &NtfyConfig{AccessToken: "tk_secret", Password: "pw_secret", Username: "u"},
	}}
	m := c.MaskSecrets()
	if m.Notifications.Ntfy.AccessToken != "" || m.Notifications.Ntfy.Password != "" {
		t.Errorf("secrets not blanked: %+v", m.Notifications.Ntfy)
	}
	if m.Notifications.Ntfy.Username != "u" {
		t.Errorf("username got blanked")
	}
	if c.Notifications.Ntfy.AccessToken != "tk_secret" {
		t.Errorf("original mutated")
	}
}

func TestFuseSafetyMarginNilUsesDefault(t *testing.T) {
	// Field omitted in YAML → nil pointer → default 0.5 A.
	f := Fuse{MaxAmps: 16, Voltage: 230, Phases: 3}
	if got := f.EffectiveSafetyMarginA(); got != DefaultFuseSafetyMarginA {
		t.Errorf("nil margin: got %v, want %v (DefaultFuseSafetyMarginA)",
			got, DefaultFuseSafetyMarginA)
	}
}

func TestFuseSafetyMarginExplicitZeroDisables(t *testing.T) {
	// The whole point of switching to *float64: explicit 0 is a real
	// operator choice ("no margin") and must NOT be silently upgraded
	// to the default. Regression for PR #219 review #1/#2/#3.
	zero := 0.0
	f := Fuse{MaxAmps: 16, Voltage: 230, Phases: 3, SafetyMarginA: &zero}
	if got := f.EffectiveSafetyMarginA(); got != 0 {
		t.Errorf("explicit 0 must disable margin, got %v", got)
	}
}

func TestFuseSafetyMarginExplicitValuePassesThrough(t *testing.T) {
	v := 1.5
	f := Fuse{MaxAmps: 16, Voltage: 230, Phases: 3, SafetyMarginA: &v}
	if got := f.EffectiveSafetyMarginA(); got != 1.5 {
		t.Errorf("got %v, want 1.5", got)
	}
}

func TestValidateRejectsNegativeSafetyMargin(t *testing.T) {
	yaml := `
site: { name: x, smoothing_alpha: 0.3 }
fuse: { max_amps: 16, phases: 3, voltage: 230, safety_margin_a: -0.1 }
drivers:
  - name: m
    lua: m.lua
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
api: { port: 8080 }
`
	_, err := Parse([]byte(yaml), ".")
	if err == nil || !strings.Contains(err.Error(), "safety_margin_a") {
		t.Errorf("expected safety_margin_a >= 0 rejection, got %v", err)
	}
}

func TestValidateRejectsSafetyMarginAtOrAboveMaxAmps(t *testing.T) {
	yaml := `
site: { name: x, smoothing_alpha: 0.3 }
fuse: { max_amps: 16, phases: 3, voltage: 230, safety_margin_a: 16.0 }
drivers:
  - name: m
    lua: m.lua
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
api: { port: 8080 }
`
	_, err := Parse([]byte(yaml), ".")
	if err == nil || !strings.Contains(err.Error(), "< fuse.max_amps") {
		t.Errorf("expected safety_margin_a < max_amps rejection, got %v", err)
	}
}

func TestValidateAcceptsExplicitZeroSafetyMargin(t *testing.T) {
	yaml := `
site: { name: x, smoothing_alpha: 0.3 }
fuse: { max_amps: 16, phases: 3, voltage: 230, safety_margin_a: 0.0 }
drivers:
  - name: m
    lua: m.lua
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
api: { port: 8080 }
`
	c, err := Parse([]byte(yaml), ".")
	if err != nil {
		t.Fatalf("explicit 0 must validate (operator-disabled margin), got %v", err)
	}
	// And the resolved value must be 0, not the default.
	if got := c.Fuse.EffectiveSafetyMarginA(); got != 0 {
		t.Errorf("EffectiveSafetyMarginA after explicit 0: got %v, want 0", got)
	}
}

func TestNotificationsPreserveMaskedSecrets(t *testing.T) {
	existing := &Config{Notifications: &Notifications{Provider: "ntfy", Ntfy: &NtfyConfig{AccessToken: "real_tok", Password: "real_pw"}}}
	incoming := &Config{Notifications: &Notifications{Provider: "ntfy", Ntfy: &NtfyConfig{}}}
	incoming.PreserveMaskedSecrets(existing)
	if incoming.Notifications.Ntfy.AccessToken != "real_tok" {
		t.Errorf("token not restored: %q", incoming.Notifications.Ntfy.AccessToken)
	}
	if incoming.Notifications.Ntfy.Password != "real_pw" {
		t.Errorf("password not restored")
	}
}

// --- UserDriversDirOverride tests ---

func TestResolveDriverPathsPrefersUserDir(t *testing.T) {
	bundledDir := t.TempDir()
	userDir := t.TempDir()

	// Write the driver only in userDir.
	if err := os.WriteFile(filepath.Join(userDir, "mydrv.lua"), []byte("--"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, origUser := DriversDirOverride, UserDriversDirOverride
	DriversDirOverride = bundledDir
	UserDriversDirOverride = userDir
	t.Cleanup(func() {
		DriversDirOverride = orig
		UserDriversDirOverride = origUser
	})

	c := &Config{Drivers: []Driver{{Lua: "drivers/mydrv.lua"}}}
	c.ResolveDriverPaths("/base")

	want := filepath.Join(userDir, "mydrv.lua")
	if c.Drivers[0].Lua != want {
		t.Errorf("got %q, want %q", c.Drivers[0].Lua, want)
	}
}

func TestResolveDriverPathsFallsBackToBundled(t *testing.T) {
	bundledDir := t.TempDir()
	userDir := t.TempDir()

	// Write the driver only in bundledDir — NOT in userDir.
	if err := os.WriteFile(filepath.Join(bundledDir, "mydrv.lua"), []byte("--"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, origUser := DriversDirOverride, UserDriversDirOverride
	DriversDirOverride = bundledDir
	UserDriversDirOverride = userDir
	t.Cleanup(func() {
		DriversDirOverride = orig
		UserDriversDirOverride = origUser
	})

	c := &Config{Drivers: []Driver{{Lua: "drivers/mydrv.lua"}}}
	c.ResolveDriverPaths("/base")

	want := filepath.Join(bundledDir, "mydrv.lua")
	if c.Drivers[0].Lua != want {
		t.Errorf("got %q, want %q", c.Drivers[0].Lua, want)
	}
}

func TestResolveDriverPathsPrefersManagedBeforeBundled(t *testing.T) {
	bundledDir := t.TempDir()
	managedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundledDir, "mydrv.lua"), []byte("bundled"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "mydrv.lua"), []byte("managed"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, origUser, origManaged := DriversDirOverride, UserDriversDirOverride, ManagedDriversDirOverride
	DriversDirOverride, UserDriversDirOverride, ManagedDriversDirOverride = bundledDir, "", managedDir
	t.Cleanup(func() {
		DriversDirOverride, UserDriversDirOverride, ManagedDriversDirOverride = orig, origUser, origManaged
	})
	c := &Config{Drivers: []Driver{{Lua: "drivers/mydrv.lua"}}}
	c.ResolveDriverPaths("/base")
	if want := filepath.Join(managedDir, "mydrv.lua"); c.Drivers[0].Lua != want {
		t.Fatalf("got %q, want %q", c.Drivers[0].Lua, want)
	}
}

func TestResolveDriverPathsLocalStillShadowsManaged(t *testing.T) {
	bundledDir, managedDir, userDir := t.TempDir(), t.TempDir(), t.TempDir()
	for _, dir := range []string{bundledDir, managedDir, userDir} {
		if err := os.WriteFile(filepath.Join(dir, "mydrv.lua"), []byte(dir), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig, origUser, origManaged := DriversDirOverride, UserDriversDirOverride, ManagedDriversDirOverride
	DriversDirOverride, UserDriversDirOverride, ManagedDriversDirOverride = bundledDir, userDir, managedDir
	t.Cleanup(func() {
		DriversDirOverride, UserDriversDirOverride, ManagedDriversDirOverride = orig, origUser, origManaged
	})
	c := &Config{Drivers: []Driver{{Lua: "drivers/mydrv.lua"}}}
	c.ResolveDriverPaths("/base")
	if want := filepath.Join(userDir, "mydrv.lua"); c.Drivers[0].Lua != want {
		t.Fatalf("got %q, want local %q", c.Drivers[0].Lua, want)
	}
}

func TestResolveDriverPathsUserEmptyBackCompat(t *testing.T) {
	bundledDir := t.TempDir()

	orig, origUser := DriversDirOverride, UserDriversDirOverride
	DriversDirOverride = bundledDir
	UserDriversDirOverride = ""
	t.Cleanup(func() {
		DriversDirOverride = orig
		UserDriversDirOverride = origUser
	})

	c := &Config{Drivers: []Driver{{Lua: "drivers/mydrv.lua"}}}
	c.ResolveDriverPaths("/base")

	want := filepath.Join(bundledDir, "mydrv.lua")
	if c.Drivers[0].Lua != want {
		t.Errorf("got %q, want %q", c.Drivers[0].Lua, want)
	}
}

func TestLocalSimulatorExampleParses(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..")
	data, err := os.ReadFile(filepath.Join(repoRoot, "config.local.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data, repoRoot); err != nil {
		t.Fatalf("config.local.example.yaml: %v", err)
	}
}
