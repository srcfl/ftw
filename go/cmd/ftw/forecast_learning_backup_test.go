package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/backup"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadmodel"
	"github.com/srcfl/ftw/go/internal/modelstate"
	"github.com/srcfl/ftw/go/internal/pvmodel"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestForecastLearningNativeMigrationBackupRestore(t *testing.T) {
	for _, signal := range []string{"pv", "load"} {
		t.Run(signal, func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			if err := os.Mkdir(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(dataDir, "config.yaml")
			databasePath := filepath.Join(dataDir, "state.db")
			cfg, err := config.Parse([]byte(`
site:
  name: Forecast backup test
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
  pv_rated_w: 8000
  heating_w_per_degc: 275
planner:
  pv_forecast_safety_k: 0
`), dataDir)
			if err != nil {
				t.Fatal(err)
			}
			st, err := state.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })

			siteConfig := newForecastSiteConfig(st)
			siteConfig.Configure(cfg, nil)
			site := siteConfig.Snapshot()
			if site.IdentityPending || !site.HasLocation || site.LearningRevision == "" {
				t.Fatalf("invalid forecast fixture site: %+v", site)
			}
			// PV training requires sunlight; keep both fixture observations in
			// Stockholm daytime so this native regression does not depend on the clock.
			start := time.Date(2026, time.June, 1, 10, 0, 0, 0, time.UTC)
			seedForecastGoModels(t, st, site, start)

			native := learningNative(t, st)
			if err := native.Update(context.Background(), site, learningObservation(site, start), nil, false); err != nil {
				t.Fatal(err)
			}
			tracker := migrationLearningTracker(t, st, &site, native, cfg)
			beforePV := tracker.pv.Model()
			beforeLoad := tracker.load.Snapshot()
			other := "load"
			if signal == "load" {
				other = "pv"
			}
			beforeNativeOther := append([]byte(nil), learningModel(t, native, other)...)

			cutoff := start.Add(22 * time.Minute)
			tracker.clock = func() time.Time { return cutoff }
			if err := tracker.RestartLearning(context.Background(), signal); err != nil {
				t.Fatal(err)
			}
			assertMigrationLearningState(t, tracker, signal, cutoff.UnixMilli(), beforePV, beforeLoad, beforeNativeOther)

			cfg, err = config.InitializeStorage(configPath, databasePath, cfg, st)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := config.Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			migratedSiteConfig := newForecastSiteConfig(st)
			migratedSiteConfig.Configure(loaded, nil)
			migratedSite := migratedSiteConfig.Snapshot()
			assertForecastSiteIdentity(t, site, migratedSite)

			postResetPV := tracker.pv.Model()
			postResetLoad := tracker.load.Snapshot()
			postResetNative := append([]byte(nil), native.Snapshot()...)
			periods := tracker.learningPeriods

			info, err := backup.Create(context.Background(), backup.CreateOptions{
				ConfigPath: configPath,
				State:      st,
				StatePath:  databasePath,
				DataDir:    dataDir,
				OutputDir:  filepath.Join(root, "backups"),
				Now:        cutoff.Add(time.Hour),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backup.Verify(info.Path); err != nil {
				t.Fatal(err)
			}
			restoredDir := filepath.Join(root, "restored")
			if _, err := backup.Restore(info.Path, restoredDir, cutoff.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}

			restoredConfig, err := config.Load(filepath.Join(restoredDir, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			restoredStore, err := state.Open(restoredConfig.ConfigDatabase)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restoredStore.Close() })
			restoredSiteConfig := newForecastSiteConfig(restoredStore)
			restoredSiteConfig.Configure(restoredConfig, nil)
			restoredSite := restoredSiteConfig.Snapshot()
			assertForecastSiteIdentity(t, site, restoredSite)

			restoredNative := learningNative(t, restoredStore)
			restored := migrationLearningTracker(t, restoredStore, &restoredSite, restoredNative, restoredConfig)
			if err := restored.restoreLearning(context.Background()); err != nil {
				t.Fatal(err)
			}
			if restored.learningPeriods != periods {
				t.Fatalf("learning periods changed across backup restore: got %+v want %+v", restored.learningPeriods, periods)
			}
			if got := restored.pv.Model(); !reflect.DeepEqual(got, postResetPV) {
				t.Fatalf("PV model changed across backup restore:\ngot  %+v\nwant %+v", got, postResetPV)
			}
			if got := restored.load.Snapshot(); !reflect.DeepEqual(got, postResetLoad) {
				t.Fatalf("load models changed across backup restore:\ngot  %+v\nwant %+v", got, postResetLoad)
			}
			if got := restoredNative.Snapshot(); !bytes.Equal(got, postResetNative) {
				t.Fatalf("native model changed across backup restore:\ngot  %s\nwant %s", got, postResetNative)
			}
			assertMigrationLearningState(t, restored, signal, cutoff.UnixMilli(), beforePV, beforeLoad, beforeNativeOther)

			effectiveSite := restored.learningSiteLocked(restoredSite)
			eligible := learningObservation(effectiveSite, start.Add(30*time.Minute))
			if err := restoredNative.Update(context.Background(), effectiveSite, eligible, nil, false); err != nil {
				t.Fatal(err)
			}
			if status := restoredNative.LearningStatus(effectiveSite, signal); status.Status != "learning" || status.LatestTrainingMS != eligible.EndMS {
				t.Fatalf("new post-reset observation did not train %s: %+v", signal, status)
			}
			beforeStale := append([]byte(nil), restoredNative.Snapshot()...)
			if err := restoredNative.Update(context.Background(), effectiveSite, learningObservation(effectiveSite, start), nil, false); err == nil {
				t.Fatal("pre-reset observation was accepted after restore")
			}
			if got := restoredNative.Snapshot(); !bytes.Equal(got, beforeStale) {
				t.Fatal("rejected pre-reset observation changed native state")
			}
		})
	}
}

func seedForecastGoModels(t *testing.T, st *state.Store, site forecastSite, at time.Time) {
	t.Helper()
	pv := pvmodel.NewModel(8000)
	pv.ConfigRevision = site.LearningRevision
	if !pv.Update(800, 20, at, 3000) {
		t.Fatal("PV fixture did not learn")
	}
	encoded, err := modelstate.Wrap(pvmodel.FeatureHash(), pv)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig("pvmodel/state_utc", encoded); err != nil {
		t.Fatal(err)
	}

	values := map[string]string{
		"loadmodel/profile":  string(loadmodel.ProfileAway),
		"loadmodel/timezone": site.Timezone,
	}
	for _, profile := range loadmodel.Profiles() {
		model := loadmodel.NewModel(4000)
		if profile == loadmodel.ProfileAway {
			model.PriorScale = 0.25
		}
		model.ConfigRevision = site.LearningRevision
		model.Timezone = site.Timezone
		model.HeatingW_per_degC = 900
		if !model.Update(at, 10_000, 10) {
			t.Fatalf("%s load fixture did not learn", profile)
		}
		encoded, err := modelstate.Wrap(loadmodel.FeatureHash(), model)
		if err != nil {
			t.Fatal(err)
		}
		values["loadmodel/state_utc:"+string(profile)] = encoded
	}
	if err := st.SaveConfigValues(values); err != nil {
		t.Fatal(err)
	}
}

func migrationLearningTracker(t *testing.T, st *state.Store, site *forecastSite, native *rustForecast, cfg *config.Config) *forecastTracker {
	t.Helper()
	tel := telemetry.NewStore()
	pv := pvmodel.NewService(st, tel, func(time.Time) float64 { return 800 }, nil, 8000)
	pv.Reconfigure(func(time.Time) float64 { return 800 }, site.LearningRevision)
	load := loadmodel.NewService(st, tel, site.Meter, 4000, 0)
	if err := load.Reconfigure(site.Meter, site.Options, site.Timezone, site.LearningRevision); err != nil {
		t.Fatal(err)
	}
	load.SeedHeatingCoef(cfg.Weather.HeatingWPerDegC)
	return &forecastTracker{
		store:     st,
		tele:      tel,
		pv:        pv,
		load:      load,
		site:      func() forecastSite { return *site },
		candidate: native,
	}
}

func assertForecastSiteIdentity(t *testing.T, want, got forecastSite) {
	t.Helper()
	if got.IdentityPending || got.SiteID != want.SiteID || got.LearningRevision != want.LearningRevision || got.Revision != want.Revision || got.WeatherSinceMS != want.WeatherSinceMS {
		t.Fatalf("forecast identity changed across config migration or restore:\ngot  %+v\nwant %+v", got, want)
	}
}

func assertMigrationLearningState(t *testing.T, tracker *forecastTracker, signal string, cutoff int64, beforePV pvmodel.Model, beforeLoad loadmodel.Snapshot, beforeNativeOther []byte) {
	t.Helper()
	wantPeriods := forecastLearningPeriods{ConfigRevision: rustConfigRevision(tracker.site())}
	if signal == "pv" {
		wantPeriods.PVMS = cutoff
	} else {
		wantPeriods.LoadMS = cutoff
	}
	if tracker.learningPeriods != wantPeriods {
		t.Fatalf("%s reset saved wrong learning periods: got %+v want %+v", signal, tracker.learningPeriods, wantPeriods)
	}
	other := "load"
	if signal == "load" {
		other = "pv"
	}
	if got := learningModel(t, tracker.candidate.(*rustForecast), other); !bytes.Equal(got, beforeNativeOther) {
		t.Fatalf("%s reset changed native %s state", signal, other)
	}
	if status := tracker.LearningStatus(signal); status.Status != "cold_start" || status.StartedMS != cutoff || status.LatestTrainingMS != 0 {
		t.Fatalf("%s reset status=%+v", signal, status)
	}
	if signal == "pv" {
		got := tracker.pv.Model()
		if got.Samples != 0 || got.LastMs != 0 || got.LearningStartedMS != cutoff {
			t.Fatalf("PV model was not cleared at cutoff: %+v", got)
		}
		if load := tracker.load.Snapshot(); !reflect.DeepEqual(load, beforeLoad) {
			t.Fatalf("PV reset changed load models:\ngot  %+v\nwant %+v", load, beforeLoad)
		}
		return
	}
	if got := tracker.pv.Model(); !reflect.DeepEqual(got, beforePV) {
		t.Fatalf("load reset changed PV model:\ngot  %+v\nwant %+v", got, beforePV)
	}
	load := tracker.load.Snapshot()
	if load.ActiveProfile != beforeLoad.ActiveProfile {
		t.Fatalf("load reset changed active profile: got %s want %s", load.ActiveProfile, beforeLoad.ActiveProfile)
	}
	for _, profile := range loadmodel.Profiles() {
		got := load.Profiles[profile]
		if got.Samples != 0 || got.LastMs != 0 || got.MAE != 0 || got.HasTemperature || got.LearningStartedMS != cutoff {
			t.Fatalf("load profile %s was not cleared at cutoff: %+v", profile, got)
		}
		if got.HeatingW_per_degC != 275 || got.ConfigRevision != beforeLoad.Profiles[profile].ConfigRevision || got.Timezone != beforeLoad.Profiles[profile].Timezone || got.PeakW != beforeLoad.Profiles[profile].PeakW {
			t.Fatalf("load profile %s lost configured state: %+v", profile, got)
		}
	}
}
