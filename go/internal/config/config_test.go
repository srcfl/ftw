package config

import (
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
	if !c.V2X.Enabled || c.V2X.DriverName != "ferroamp" || c.V2X.MinReserveSoCPct != 35 {
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
  provider: elprisetjustnu
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

func TestResolveDriverPaths(t *testing.T) {
	baseDir := "/etc/ftw"
	c := &Config{Drivers: []Driver{
		{Name: "rel", Lua: "drivers/a.lua"},
		{Name: "absin", Lua: "/etc/ftw/drivers/b.lua"},
		{Name: "absout", Lua: "/opt/drivers/c.lua"},
		{Name: "escape", Lua: "../../secrets/d.lua"},
		{Name: "empty"},
	}}
	c.ResolveDriverPaths(baseDir)
	want := map[string]string{
		"rel":    "/etc/ftw/drivers/a.lua", // joined with baseDir
		"absin":  "/etc/ftw/drivers/b.lua", // already absolute, untouched
		"absout": "/opt/drivers/c.lua",     // absolute outside baseDir, untouched
		"escape": "/etc/ftw/secrets/d.lua", // leading "../" stripped, then joined
		"empty":  "",
	}
	for _, d := range c.Drivers {
		if d.Lua != want[d.Name] {
			t.Errorf("resolve %s: got %q, want %q", d.Name, d.Lua, want[d.Name])
		}
	}
}

func TestUnresolveDriverPathsRoundtrip(t *testing.T) {
	baseDir := "/etc/ftw"
	original := []Driver{
		{Name: "rel", Lua: "drivers/a.lua"},
		{Name: "absin", Lua: "/etc/ftw/drivers/b.lua"}, // absolute but inside baseDir
		{Name: "absout", Lua: "/opt/drivers/c.lua"},    // absolute outside baseDir — must stay absolute
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
	if got["rel"] != "drivers/a.lua" {
		t.Errorf("rel: got %q, want drivers/a.lua", got["rel"])
	}
	if got["absin"] != "drivers/b.lua" {
		t.Errorf("absin: got %q, want drivers/b.lua", got["absin"])
	}
	if got["absout"] != "/opt/drivers/c.lua" {
		t.Errorf("absout: got %q, want /opt/drivers/c.lua (must remain absolute)", got["absout"])
	}
	if got["empty"] != "" {
		t.Errorf("empty: got %q, want empty string", got["empty"])
	}

	// Re-resolving must produce the same absolute paths as the first
	// Resolve — the UI save/load cycle relies on this being a fixed point.
	c.ResolveDriverPaths(baseDir)
	want := map[string]string{
		"rel":    "/etc/ftw/drivers/a.lua",
		"absin":  "/etc/ftw/drivers/b.lua",
		"absout": "/opt/drivers/c.lua",
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

func TestNotificationsValidateRejectsEmptyTopic(t *testing.T) {
	c := &Config{
		Site:          Site{SmoothingAlpha: 0.3},
		Fuse:          Fuse{MaxAmps: 16},
		Notifications: &Notifications{Enabled: true, Provider: "ntfy", Ntfy: &NtfyConfig{Server: "https://ntfy.sh", Topic: ""}},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error for empty topic")
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
