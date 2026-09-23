package loadmodel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/modelstate"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestRestartLearningClearsEveryLoadProfileAndRestoresConfiguredPrior(t *testing.T) {
	s := NewService(nil, telemetry.NewStore(), "site", 4000, 0)
	if err := s.Reconfigure("site", telemetry.ForecastOptions{}, "Europe/Stockholm", "site-a"); err != nil {
		t.Fatal(err)
	}
	s.SeedHeatingCoef(275)
	for _, profile := range Profiles() {
		m := s.models[profile]
		m.Update(time.Now(), 2000, 5)
		m.HeatingW_per_degC = 900
	}
	if err := s.SetProfile(ProfileAway); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if err := s.RestartLearning(cutoff); err != nil {
		t.Fatal(err)
	}

	snap := s.Snapshot()
	if snap.ActiveProfile != ProfileAway {
		t.Fatalf("active profile changed to %q", snap.ActiveProfile)
	}
	for _, profile := range Profiles() {
		got := snap.Profiles[profile]
		if got.Samples != 0 || got.MAE != 0 || got.LastMs != 0 || got.HasTemperature {
			t.Fatalf("learned %s state survived: %+v", profile, got)
		}
		if got.HeatingW_per_degC != 275 {
			t.Fatalf("%s heating prior = %.0f, want configured 275", profile, got.HeatingW_per_degC)
		}
		if got.PeakW != 4000 || got.Timezone != "Europe/Stockholm" || got.ConfigRevision != "site-a" {
			t.Fatalf("configured %s state changed: %+v", profile, got)
		}
		if got.LearningStartedMS != cutoff.UnixMilli() {
			t.Fatalf("%s cutoff = %d", profile, got.LearningStartedMS)
		}
	}
}

func TestRestartLearningRejectsLoadSampleCapturedBeforeCutoff(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("site")
	s := NewService(nil, tel, "site", 4000, 0)
	cutoff := time.Now().Add(time.Millisecond)
	if err := s.RestartLearning(cutoff); err != nil {
		t.Fatal(err)
	}

	s.sampleAt(cutoff.Add(10 * time.Millisecond))

	if got := s.Model().Samples; got != 0 {
		t.Fatalf("pre-cutoff telemetry trained load model: %d samples", got)
	}
}

func TestRestartLearningRejectsMixedLoadBalanceWithOldInput(t *testing.T) {
	tel := telemetry.NewStore()
	now := time.Now()
	cutoff := now.Add(-time.Second)
	powerData := func(watts float64, measured time.Time) []byte {
		data, err := json.Marshal(map[string]telemetry.ForecastPowerSample{
			"forecast_power": {
				Version:      1,
				Known:        true,
				Watts:        watts,
				MeasuredAtMS: measured.UnixMilli(),
				ReceivedAtMS: now.UnixMilli(),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	tel.Update("site", telemetry.DerMeter, 1000, nil, powerData(1000, cutoff.Add(-time.Second)))
	tel.RecordDriverSuccess("site")
	tel.Update("battery", telemetry.DerBattery, 500, nil, powerData(500, cutoff.Add(time.Second)))
	tel.RecordDriverSuccess("battery")
	s := NewService(nil, tel, "site", 4000, 0)
	if err := s.RestartLearning(cutoff); err != nil {
		t.Fatal(err)
	}

	s.sampleAt(now.Add(time.Second))

	if got := s.Model().Samples; got != 0 {
		t.Fatalf("balance with pre-cutoff meter trained load model: %d samples", got)
	}
}

func TestRestartLearningInvalidatesLoadSampleInFlight(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("site")
	s := NewService(nil, tel, "site", 4000, 0)
	entered := make(chan struct{})
	release := make(chan struct{})
	s.Temp = func(time.Time) (float64, bool) {
		close(entered)
		<-release
		return 5, true
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.sampleAt(time.Now())
	}()
	<-entered
	if err := s.RestartLearning(time.Now()); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done

	if got := s.Model().Samples; got != 0 {
		t.Fatalf("in-flight sample survived restart: %d", got)
	}
}

func TestRestartLearningLoadRetryPersistsAllProfilesWithoutErasingNewSamples(t *testing.T) {
	bad := openTestDB(t)
	s := NewService(bad, telemetry.NewStore(), "site", 4000, 0)
	s.SeedHeatingCoef(275)
	if err := bad.Close(); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	if err := s.RestartLearning(cutoff); err == nil {
		t.Fatal("closed store did not surface persistence failure")
	}
	for _, profile := range Profiles() {
		if !s.models[profile].Update(cutoff.Add(time.Minute), 1000, 10) {
			t.Fatalf("post-reset %s sample rejected", profile)
		}
	}

	good, err := state.Open(t.TempDir() + "/retry.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { good.Close() })
	s.Store = good
	if err := s.RestartLearning(cutoff); err != nil {
		t.Fatal(err)
	}
	restored := NewService(good, telemetry.NewStore(), "site", 4000, 0)
	if restored.LearningStartedMS() != cutoff.UnixMilli() {
		t.Fatalf("restored cutoff = %d", restored.LearningStartedMS())
	}
	for _, profile := range Profiles() {
		if got := restored.models[profile]; got.Samples != 1 || got.LearningStartedMS != cutoff.UnixMilli() {
			t.Fatalf("same-cutoff retry lost %s learning: %+v", profile, got)
		}
	}
}

func TestLoadRestoreReconcilesPartiallyPersistedEpoch(t *testing.T) {
	st := openTestDB(t)
	s := NewService(st, telemetry.NewStore(), "site", 4000, 0)
	at := time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	for _, profile := range Profiles() {
		s.models[profile].Update(at, 1000, 10)
	}
	if err := s.persist(); err != nil {
		t.Fatal(err)
	}

	cutoff := at.Add(time.Hour).UnixMilli()
	home := freshProfile(s.models[ProfileHome], ProfileHome, "UTC", cutoff, nil)
	js, err := modelstate.Wrap(FeatureHash(), home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig(stateKey(ProfileHome), js); err != nil {
		t.Fatal(err)
	}

	restored := NewService(st, telemetry.NewStore(), "site", 4000, 0)
	for _, profile := range Profiles() {
		got := restored.models[profile]
		if got.Samples != 0 || got.LearningStartedMS != cutoff {
			t.Fatalf("partial epoch left stale %s state: %+v", profile, got)
		}
	}
}
