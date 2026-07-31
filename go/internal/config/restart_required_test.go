package config

import (
	"strings"
	"testing"
)

func TestRestartRequiredFor_NoChange(t *testing.T) {
	cfg := baseCfg()
	if r := RestartRequiredFor(cfg, cfg); len(r) != 0 {
		t.Fatalf("expected no reasons, got %v", r)
	}
}

func TestRestartRequiredFor_HotReloadable(t *testing.T) {
	old := baseCfg()
	newC := baseCfg()
	// All of these are hot-reloaded by the configreload applier today.
	newC.Site.GridTargetW = 500
	newC.Site.GridToleranceW = 200
	newC.Site.SlewRateW = 1500
	newC.Site.MinDispatchIntervalS = 30
	newC.Fuse.MaxAmps = 25
	newC.Drivers = append(newC.Drivers, Driver{Name: "extra"})
	if newC.Weather == nil {
		newC.Weather = &Weather{}
	}
	*newC.Weather = *old.Weather
	newC.Weather.PVRatedW = 12000
	newC.Weather.Latitude = 59.0
	newC.Weather.Longitude = 18.0
	// HA changes hot-reload via (*ha.Bridge).Reload.
	newC.HomeAssistant = &HomeAssistant{
		Enabled: true, Broker: "10.0.0.5", Port: 1883, PublishIntervalS: 10,
	}
	old.HomeAssistant = &HomeAssistant{
		Enabled: true, Broker: "192.168.1.1", Port: 1883, PublishIntervalS: 5,
	}

	if r := RestartRequiredFor(old, newC); len(r) != 0 {
		t.Fatalf("expected no reasons (all hot-reloadable), got %v", r)
	}
}

func TestRestartRequiredFor_SiteBootScalars(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(*Config)
		wantIn string
	}{
		{"control_interval_s", func(c *Config) { c.Site.ControlIntervalS = 10 }, "control_interval_s"},
		{"watchdog_timeout_s", func(c *Config) { c.Site.WatchdogTimeoutS = 30 }, "watchdog_timeout_s"},
		{"smoothing_alpha", func(c *Config) { c.Site.SmoothingAlpha = 0.9 }, "smoothing_alpha"},
		{"gain", func(c *Config) { c.Site.Gain = 0.7 }, "site.gain"},
		{"name", func(c *Config) { c.Site.Name = "renamed" }, "site.name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := baseCfg()
			n := baseCfg()
			tc.mut(n)
			r := RestartRequiredFor(old, n)
			if !containsSubstring(r, tc.wantIn) {
				t.Fatalf("expected reason mentioning %q, got %v", tc.wantIn, r)
			}
		})
	}
}

func TestRestartRequiredFor_BootSections(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(*Config)
		wantIn string
	}{
		{"api.port", func(c *Config) { c.API.Port = 9090 }, "api.port"},
		{"state path", func(c *Config) { c.State = &StateConf{Path: "/var/lib/ftw/state.db"} }, "state"},
		{"price provider", func(c *Config) { c.Price = &Price{Provider: "entsoe"} }, "price"},
		{"planner toggled", func(c *Config) { c.Planner = &Planner{Enabled: true} }, "planner"},
		{"nova toggled", func(c *Config) { c.Nova = &Nova{Enabled: true, URL: "https://x"} }, "nova"},
		{"home link toggled", func(c *Config) { c.HomeLink = &HomeLink{Enabled: true} }, "home_link"},
		{"ev_charger added", func(c *Config) {
			c.EVCharger = &EVCharger{Provider: "easee", Username: "a@b.c"}
		}, "ev_charger"},
		{"caldav credentials changed", func(c *Config) {
			c.CalDAV = &CalDAV{Enabled: true, Username: "calendar-user", Password: "rotated"}
		}, "caldav"},
		{"weather provider change", func(c *Config) {
			c.Weather = &Weather{Provider: "open_meteo", Latitude: 59, Longitude: 18}
		}, "weather"},
		{"weather pv_arrays added", func(c *Config) {
			c.Weather = &Weather{Provider: "met_no", Latitude: 59, Longitude: 18,
				PVArrays: []PVArray{{KWp: 5, TiltDeg: 30, AzimuthDeg: 180}}}
		}, "weather"},
		{"weather heating coefficient", func(c *Config) {
			c.Weather.HeatingWPerDegC = 250
		}, "weather"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := baseCfg()
			n := baseCfg()
			tc.mut(n)
			r := RestartRequiredFor(old, n)
			if !containsSubstring(r, tc.wantIn) {
				t.Fatalf("expected reason mentioning %q, got %v", tc.wantIn, r)
			}
		})
	}
}

func TestRestartRequiredFor_NilInputs(t *testing.T) {
	if r := RestartRequiredFor(nil, baseCfg()); r != nil {
		t.Fatalf("expected nil reasons for nil old, got %v", r)
	}
	if r := RestartRequiredFor(baseCfg(), nil); r != nil {
		t.Fatalf("expected nil reasons for nil new, got %v", r)
	}
}

func baseCfg() *Config {
	return &Config{
		Site: Site{
			Name: "home", ControlIntervalS: 5, WatchdogTimeoutS: 60,
			SmoothingAlpha: 0.5, Gain: 0.3, GridTargetW: 0,
			GridToleranceW: 100, SlewRateW: 1000, MinDispatchIntervalS: 5,
		},
		Fuse:    Fuse{MaxAmps: 20, Phases: 3, Voltage: 230},
		API:     API{Port: 8080},
		Drivers: []Driver{{Name: "ferro"}},
		Weather: &Weather{Provider: "met_no", Latitude: 59, Longitude: 18, PVRatedW: 10000},
	}
}

func containsSubstring(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
