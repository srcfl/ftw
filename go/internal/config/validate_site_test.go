package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadSiteValidationConfig(t *testing.T, yaml string) error {
	t.Helper()
	_, err := loadSiteValidationConfigFull(t, yaml)
	return err
}

func loadSiteValidationConfigFull(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoadRejectsMoreThanThreeFusePhases(t *testing.T) {
	err := loadSiteValidationConfig(t, strings.Replace(minimalYAML, "max_amps: 16", "max_amps: 16\n  phases: 4", 1))
	if err == nil || err.Error() != "fuse.phases must be 1, 2 or 3" {
		t.Errorf("Load error = %v, want fuse phase validation error", err)
	}
}

func TestValidateAcceptsOneToThreePhases(t *testing.T) {
	for phases := 1; phases <= 3; phases++ {
		c := &Config{
			Site: Site{SmoothingAlpha: 0.3},
			Fuse: Fuse{MaxAmps: 16, Phases: phases, Voltage: 230},
		}
		if err := c.Validate(); err != nil {
			t.Errorf("phases=%d: unexpected error: %v", phases, err)
		}
	}
}

func meterDriver(name string, siteMeter bool) Driver {
	return Driver{
		Name:        name,
		Lua:         "drivers/test.lua",
		IsSiteMeter: siteMeter,
		Capabilities: Capabilities{
			Modbus: &ModbusConfig{Host: "192.168.1.10", Port: 502},
		},
	}
}

// Load repairs duplicate site meters instead of refusing the file: a
// config an older version booted with (first match won silently) must
// keep booting after an update, or the operator loses the UI they would
// fix it with. Field incident 2026-08-29: a driver install on v1.15.0
// left two is_site_meter drivers, and the v2.3.0 update crash-looped the
// box before the HTTP listener bound. The write path stays strict — see
// TestValidateRejectsDuplicateSiteMeters.
func TestLoadDemotesDuplicateSiteMetersAndWarns(t *testing.T) {
	yaml := strings.Replace(minimalYAML, "api:\n", `
  - name: second-meter
    lua: drivers/second-meter.lua
    is_site_meter: true
    capabilities:
      mqtt:
        host: 192.168.1.154
api:
`, 1)
	cfg, err := loadSiteValidationConfigFull(t, yaml)
	if err != nil {
		t.Fatalf("Load with duplicate site meters: %v", err)
	}
	if got := cfg.SiteMeterDriver(); got != "ferroamp" {
		t.Errorf("site meter = %q, want the first declared %q", got, "ferroamp")
	}
	for _, d := range cfg.Drivers {
		if d.Name == "second-meter" && d.IsSiteMeter {
			t.Error("second-meter kept is_site_meter after load")
		}
	}
	if len(cfg.LoadWarnings) != 1 ||
		!strings.Contains(cfg.LoadWarnings[0], `"ferroamp"`) ||
		!strings.Contains(cfg.LoadWarnings[0], `"second-meter"`) {
		t.Errorf("LoadWarnings = %q, want one warning naming both drivers", cfg.LoadWarnings)
	}
}

// The write path (Settings save, bootstrap POST /api/config) calls
// Validate directly and must keep rejecting the ambiguity — the operator
// is present to fix it before it persists.
func TestValidateRejectsDuplicateSiteMeters(t *testing.T) {
	c := &Config{
		Site:    Site{SmoothingAlpha: 0.3},
		Fuse:    Fuse{MaxAmps: 16, Phases: 3, Voltage: 230},
		Drivers: []Driver{meterDriver("a", true), meterDriver("b", true)},
	}
	if err := c.Validate(); err == nil ||
		err.Error() != "exactly one driver may set is_site_meter: true (found 2)" {
		t.Errorf("Validate error = %v, want duplicate site meter validation error", err)
	}
}

func TestValidateAcceptsSingleSiteMeter(t *testing.T) {
	c := &Config{
		Site:    Site{SmoothingAlpha: 0.3},
		Fuse:    Fuse{MaxAmps: 16, Phases: 3, Voltage: 230},
		Drivers: []Driver{meterDriver("a", true), meterDriver("b", false)},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
