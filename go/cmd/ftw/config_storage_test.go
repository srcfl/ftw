package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestConfigStorageKeepsForecastLearningIdentity(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "state.db")
	path := filepath.Join(dir, "config.yaml")
	script := filepath.Join(dir, "meter.lua")
	if err := os.WriteFile(script, []byte("measurement code"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(`
site:
  name: Stored site
fuse:
  max_amps: 16
api:
  port: 8080
app_link:
  enabled: false
weather:
  provider: open_meteo
  latitude: 59
  longitude: 18
  timezone: Europe/Stockholm
  heating_coefficient_w_per_c: 0
planner:
  pv_forecast_safety_k: 0
drivers:
  - name: meter
    lua: meter.lua
    is_site_meter: true
    capabilities:
      standalone: true
    config:
      scale: 1
      enabled: false
`), dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	beforeSite := newForecastSiteConfig(st)
	beforeSite.Configure(cfg, nil)
	before := beforeSite.Snapshot()
	const opaque = "{ \"state\": [1, 2, 3] }"
	if err := st.SaveConfig("forecast/energyplan_state_v1", opaque); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.InitializeStorage(path, database, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	afterSite := newForecastSiteConfig(st)
	afterSite.Configure(loaded, nil)
	after := afterSite.Snapshot()
	if before.SiteID != after.SiteID || before.LearningRevision != after.LearningRevision || before.Revision != after.Revision || before.WeatherSinceMS != after.WeatherSinceMS {
		t.Fatalf("storage changed forecast identity:\nbefore=%+v\nafter=%+v", before, after)
	}
	if raw, _ := st.LoadConfig("forecast/energyplan_state_v1"); raw != opaque {
		t.Fatal("storage reserialized worker state")
	}
	if loaded.Planner.PVForecastSafetyK == nil || *loaded.Planner.PVForecastSafetyK != 0 {
		t.Fatal("explicit zero became the default")
	}
}
