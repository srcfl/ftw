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
	newC.API.LANAuth = true
	newC.Site.ControlIntervalS = 10
	newC.Site.WatchdogTimeoutS = 30
	newC.Site.SmoothingAlpha = 0.9
	newC.Site.Gain = 0.7
	newC.Site.Name = "renamed"
	newC.State = &StateConf{ColdRetentionDays: 400, BackupDir: "backups"}
	newC.Price = &Price{Provider: "sourceful", Zone: "SE4", GridTariffOreKwh: 30, VATPercent: 25}
	old.Price = &Price{Provider: "sourceful", Zone: "SE4", GridTariffOreKwh: 20, VATPercent: 25}
	newC.Planner = &Planner{Enabled: true, SoCMin: 0.15, SoCMax: 0.9, MinArbitrageSpreadOreKwh: 5}
	old.Planner = &Planner{Enabled: true, SoCMin: 0.1, SoCMax: 0.95}
	newC.Weather.HeatingWPerDegC = 250
	tilt, azimuth := 30.0, 180.0
	newC.Weather.PVArrays = []PVArray{{KWp: 5, TiltDeg: &tilt, AzimuthDeg: &azimuth}}

	if r := RestartRequiredFor(old, newC); len(r) != 0 {
		t.Fatalf("expected no reasons (all hot-reloadable), got %v", r)
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
		{"app link toggled off", func(c *Config) { c.AppLink = &AppLink{Enabled: false} }, "app_link"},
		{"ev_charger added", func(c *Config) {
			c.EVCharger = &EVCharger{Provider: "easee", Username: "a@b.c"}
		}, "ev_charger"},
		{"ocpp enabled", func(c *Config) {
			c.OCPP = &OCPP{Enabled: true, Port: 8887, Username: "ftw", Password: "long-random-string"}
		}, "ocpp"},
		{"state cold_dir", func(c *Config) { c.State = &StateConf{ColdDir: "/var/lib/ftw/cold"} }, "state"},
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

func TestRestartRequiredFor_WeatherOffToOnNeedsRestart(t *testing.T) {
	old := baseCfg()
	old.Weather = &Weather{Provider: "none"}
	n := baseCfg()
	n.Weather = &Weather{Provider: "open_meteo", Latitude: 59, Longitude: 18}
	if !containsSubstring(RestartRequiredFor(old, n), "weather") {
		t.Fatal("enabling weather from none should require restart")
	}
}

func TestRestartRequiredFor_WeatherGeometryIsHot(t *testing.T) {
	old := baseCfg()
	n := baseCfg()
	n.Weather.HeatingWPerDegC = 250
	tilt, azimuth := 30.0, 180.0
	n.Weather.PVArrays = []PVArray{{KWp: 5, TiltDeg: &tilt, AzimuthDeg: &azimuth}}
	n.Weather.Provider = "open_meteo"
	if r := RestartRequiredFor(old, n); len(r) != 0 {
		t.Fatalf("weather geometry/provider swap should be live, got %v", r)
	}
}

func TestRestartRequiredFor_StateRetentionIsHot(t *testing.T) {
	old := baseCfg()
	n := baseCfg()
	n.State = &StateConf{ColdRetentionDays: 400}
	if r := RestartRequiredFor(old, n); len(r) != 0 {
		t.Fatalf("cold_retention_days should be live, got %v", r)
	}
}

func TestRestartRequiredFor_WeatherOnToOffIsHot(t *testing.T) {
	old := baseCfg()
	n := baseCfg()
	n.Weather = &Weather{Provider: "none"}
	if r := RestartRequiredFor(old, n); len(r) != 0 {
		t.Fatalf("disabling weather should be live via Reconfigure, got %v", r)
	}
}

func TestRestartRequiredFor_PriceTariffIsHotWhenProviderStays(t *testing.T) {
	old := baseCfg()
	old.Price = &Price{Provider: "sourceful", Zone: "SE4", GridTariffOreKwh: 20, VATPercent: 25}
	n := baseCfg()
	n.Price = &Price{Provider: "sourceful", Zone: "SE4", GridTariffOreKwh: 45, VATPercent: 25, ExportBonusOreKwh: 5}
	if r := RestartRequiredFor(old, n); len(r) != 0 {
		t.Fatalf("price tariff should be live, got %v", r)
	}
}

func TestRestartRequiredFor_PlannerSoCIsHotWhenEnabledStays(t *testing.T) {
	old := baseCfg()
	old.Planner = &Planner{Enabled: true, Engine: "core", SoCMin: 0.10, SoCMax: 0.95, IntervalMin: 15}
	n := baseCfg()
	n.Planner = &Planner{Enabled: true, Engine: "core", SoCMin: 0.15, SoCMax: 0.88, IntervalMin: 10, MinArbitrageSpreadOreKwh: 8}
	if r := RestartRequiredFor(old, n); len(r) != 0 {
		t.Fatalf("planner SoC/interval/spread should be live, got %v", r)
	}
}

func TestRestartRequiredFor_PlannerEngineSwapNeedsRestart(t *testing.T) {
	old := baseCfg()
	old.Planner = &Planner{Enabled: true, Engine: "core"}
	n := baseCfg()
	n.Planner = &Planner{Enabled: true, Engine: "energyplan"}
	if !containsSubstring(RestartRequiredFor(old, n), "planner") {
		t.Fatal("swapping planner engine should require restart")
	}
}

func TestRestartRequiredFor_PriceZoneSwapNeedsRestart(t *testing.T) {
	old := baseCfg()
	old.Price = &Price{Provider: "sourceful", Zone: "SE4"}
	n := baseCfg()
	n.Price = &Price{Provider: "sourceful", Zone: "SE3"}
	if !containsSubstring(RestartRequiredFor(old, n), "price") {
		t.Fatal("swapping price zone should require restart")
	}
}

func TestRestartRequiredFor_AssistantIsHot(t *testing.T) {
	old := baseCfg()
	n := baseCfg()
	n.Assistant = &Assistant{Enabled: true, APIKey: "sk-or-v1-test", Model: "openrouter/free"}
	if r := RestartRequiredFor(old, n); len(r) != 0 {
		t.Fatalf("assistant is read per request, got restart reasons %v", r)
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
