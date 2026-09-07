package main

import (
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/loadmodel"
	"github.com/srcfl/ftw/go/internal/modelstate"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestForecastSiteSeparatesLearningAndEvaluationRevisions(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	script := filepath.Join(t.TempDir(), "meter.lua")
	if err := os.WriteFile(script, []byte("first measurement code"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Drivers: []config.Driver{{Name: "meter", Lua: script, IsSiteMeter: true, Config: map[string]any{"scale": 1.0}}}, Weather: &config.Weather{Provider: "open_meteo", Latitude: 59, Longitude: 18}}
	s := newForecastSiteConfig(st)
	s.Configure(cfg, nil)
	first := s.Snapshot()
	if first.HasPVScale || forecastRatedPVW(cfg.Weather) != 0 {
		t.Fatal("unknown PV capacity became a guessed rating")
	}
	s.engineVersion = "another-worker-build"
	s.Configure(cfg, nil)
	build := s.Snapshot()
	if build.LearningRevision != first.LearningRevision || build.Revision == first.Revision {
		t.Fatal("worker build must change evaluation cohort, not learned site binding")
	}
	cfg.Drivers[0].Config["scale"] = 2.0
	s.Configure(cfg, nil)
	scaled := s.Snapshot()
	if scaled.LearningRevision == build.LearningRevision {
		t.Fatal("meter scaling retained old learned state")
	}
	if err := os.WriteFile(script, []byte("changed measurement code"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Configure(cfg, nil)
	if s.Snapshot().LearningRevision == scaled.LearningRevision {
		t.Fatal("changed measurement code retained old learned state")
	}
	copy := s.Snapshot()
	copy.Options.ExpectedFlows[0].Driver = "mutated"
	if s.Snapshot().Options.ExpectedFlows[0].Driver == "mutated" {
		t.Fatal("site snapshot aliases expected flows")
	}
	cfg.Weather.Latitude = math.NaN()
	s.Configure(cfg, nil)
	if s.Snapshot().HasLocation || s.Snapshot().Options.HouseholdInvalidReason == "" {
		t.Fatal("unserializable configuration remained qualified")
	}
}

func TestForecastReleaseEvidenceDoesNotCrossDriverRestart(t *testing.T) {
	current := uint64(4)
	live := true
	get := func(string) (uint64, bool) { return current, live }
	evidence := bindForecastReleaseGeneration(func(string) bool { return true }, []string{"solar"}, get)
	if !evidence("solar") || evidence("other") {
		t.Fatal("incorrect initial binding")
	}
	current++
	if evidence("solar") {
		t.Fatal("new driver instance inherited old release proof")
	}
	evidence = bindForecastReleaseGeneration(func(string) bool { return true }, []string{"solar"}, get)
	if !evidence("solar") {
		t.Fatal("fresh proof did not bind new driver")
	}
	live = false
	if evidence("solar") {
		t.Fatal("missing driver retained release proof")
	}
}

func TestForecastLiveIdentityDelayedSerialRestartAndReplacement(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{Drivers: []config.Driver{{Name: "meter", IsSiteMeter: true}}}
	// The historical row is deliberately stale; it cannot bind this process.
	_, _ = st.RegisterDevice(state.Device{DriverName: "meter", Make: "vendor", Serial: "wrong-device"})
	live := ""
	s := newForecastSiteConfig(st)
	s.identity = func(string) (string, bool) { return live, live != "" }
	s.Configure(cfg, nil)
	if !s.Snapshot().IdentityPending {
		t.Fatal("historical devices row vouched for unknown running device")
	}
	live = "vendor:serial-a"
	if !s.RefreshIdentity(time.Now()) {
		t.Fatal("delayed serial did not trigger binding")
	}
	first := s.Snapshot()
	if first.IdentityPending {
		t.Fatal("reported serial remained pending")
	}
	// Save actual legacy models with their physical binding.
	for _, p := range loadmodel.Profiles() {
		m := loadmodel.NewModel(4000)
		m.ConfigRevision = first.LearningRevision
		m.Timezone = first.Timezone
		m.Samples = 7
		encoded, err := modelstate.Wrap(loadmodel.FeatureHash(), m)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SaveConfig("loadmodel/state_utc:"+string(p), string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.SaveConfig("loadmodel/timezone", first.Timezone)
	restarted := newForecastSiteConfig(st)
	live = "ep:tcp://meter:502"
	restarted.identity = func(string) (string, bool) { return live, live != "" }
	restarted.Configure(cfg, nil)
	restarted.RefreshIdentity(time.Now().Add(time.Minute))
	if !restarted.Snapshot().IdentityPending {
		t.Fatal("weak startup endpoint replaced confirmed serial")
	}
	model := loadmodel.NewService(st, telemetry.NewStore(), "meter", 4000, 0)
	if model.Model().Samples != 7 {
		t.Fatal("pending startup overwrote learned model")
	}
	live = "vendor:serial-a"
	restarted.RefreshIdentity(time.Now())
	same := restarted.Snapshot()
	if same.IdentityPending || same.LearningRevision != first.LearningRevision {
		t.Fatal("same final live identity changed persisted binding")
	}
	if err := model.Reconfigure(same.Meter, same.Options, same.Timezone, same.LearningRevision); err != nil {
		t.Fatal(err)
	}
	if model.Model().Samples != 7 {
		t.Fatal("same serial discarded restored learning")
	}
	live = "vendor:serial-b"
	if !restarted.RefreshIdentity(time.Now()) {
		t.Fatal("same-alias physical replacement was not detected")
	}
	next := restarted.Snapshot()
	if next.IdentityPending || next.LearningRevision == same.LearningRevision {
		t.Fatal("new serial reused old site model binding")
	}
	if err := model.Reconfigure(next.Meter, next.Options, next.Timezone, next.LearningRevision); err != nil {
		t.Fatal(err)
	}
	if model.Model().Samples != 0 {
		t.Fatal("new device inherited old household model")
	}
}

func TestForecastEndpointFallbackAndIndependentPVIdentity(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{Drivers: []config.Driver{{Name: "meter", Lua: "meter.lua", IsSiteMeter: true}, {Name: "pv", Lua: "pv.lua"}, {Name: "ev", Lua: "ev.lua"}}}
	catalog := []drivers.CatalogEntry{{Filename: "meter.lua", Capabilities: []string{"meter"}}, {Filename: "pv.lua", Capabilities: []string{"pv"}}, {Filename: "ev.lua", Capabilities: []string{"ev"}}}
	ids := map[string]string{"meter": "meter:sn", "pv": "pv:sn"}
	s := newForecastSiteConfig(st)
	s.identity = func(name string) (string, bool) { id := ids[name]; return id, id != "" }
	s.Configure(cfg, catalog)
	first := s.Snapshot()
	if !first.IdentityPending || first.Options.HouseholdInvalidReason == "" || first.Options.PVInvalidReason != "" {
		t.Fatal("unknown EV failed to preserve independent PV qualification")
	}
	ids["ev"] = "ep:charger-endpoint"
	s.RefreshIdentity(s.configuredAt.Add(time.Second))
	if !s.Snapshot().IdentityPending {
		t.Fatal("endpoint bypassed init grace")
	}
	s.RefreshIdentity(s.configuredAt.Add(4 * time.Second))
	ready := s.Snapshot()
	if ready.IdentityPending {
		t.Fatal("endpoint-only device can never learn")
	}
	restarted := newForecastSiteConfig(st)
	restarted.identity = s.identity
	restarted.Configure(cfg, catalog)
	restarted.RefreshIdentity(restarted.configuredAt.Add(4 * time.Second))
	if restarted.Snapshot().LearningRevision != ready.LearningRevision {
		t.Fatal("same endpoint fallback changed across restart")
	}
	ids["ev"] = "charger:new-serial"
	restarted.RefreshIdentity(time.Now())
	if restarted.Snapshot().LearningRevision == ready.LearningRevision {
		t.Fatal("stronger live identity was ignored")
	}
	ids["pv"] = ""
	restarted.RefreshIdentity(time.Now())
	if restarted.Snapshot().Options.PVInvalidReason == "" {
		t.Fatal("unknown PV identity remained eligible")
	}
}

func TestForecastWeatherGenerationReceiptSurvivesRestart(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{Weather: &config.Weather{Provider: "open_meteo", Latitude: 59, Longitude: 18}}
	first := newForecastSiteConfig(st)
	first.Configure(cfg, nil)
	cutoff := first.Snapshot().WeatherSinceMS
	second := newForecastSiteConfig(st)
	second.Configure(cfg, nil)
	if second.Snapshot().WeatherSinceMS != cutoff {
		t.Fatal("restart changed unchanged provider cutoff")
	}
	// A prior receipt cannot qualify forecasts for a new weather location.
	second.weatherSinceMS = cutoff - 1000
	cfg.Weather.Latitude = 60
	second.Configure(cfg, nil)
	if second.Snapshot().WeatherSinceMS <= cutoff-1000 {
		t.Fatal("new location kept old weather generation")
	}
	third := newForecastSiteConfig(st)
	third.Configure(cfg, nil)
	if third.Snapshot().WeatherSinceMS != second.Snapshot().WeatherSinceMS {
		t.Fatal("new generation did not persist")
	}
}
