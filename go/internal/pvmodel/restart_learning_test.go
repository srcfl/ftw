package pvmodel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestRestartLearningClearsLearnedPVState(t *testing.T) {
	s := NewService(nil, nil, func(time.Time) float64 { return 800 }, nil, 5000)
	s.SetACLimit(4200)
	s.model.ConfigRevision = "site-a"
	at := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		s.model.Update(800, 20, at.Add(time.Duration(i)*time.Minute), 3000)
		s.Residuals.Add(at.Add(time.Duration(i)*time.Minute), 2500, 3000)
	}

	if err := s.RestartLearning(at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got := s.Model()
	if got.Samples != 0 || got.MAE != 0 || got.RelMAE != 0 || got.InferredScaleKnown || got.ScaleSamples != 0 {
		t.Fatalf("learned state survived: %+v", got)
	}
	if s.Residuals.Len() != 0 {
		t.Fatal("PV residuals survived")
	}
	if got.RatedW != 5000 || got.ACLimitW != 4200 || got.ConfigRevision != "site-a" {
		t.Fatalf("configured PV state changed: %+v", got)
	}
	if got.LearningStartedMS != at.Add(time.Hour).UnixMilli() || s.LearningStartedMS() != got.LearningStartedMS {
		t.Fatalf("learning cutoff = %d", got.LearningStartedMS)
	}
}

func TestRestartLearningRejectsSampleCapturedBeforeCutoff(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("pv", telemetry.DerPV, -3000, nil, nil)
	tel.RecordDriverSuccess("pv")
	s := NewService(nil, tel, func(time.Time) float64 { return 800 }, func(time.Time) (float64, bool) { return 20, true }, 5000)
	cutoff := time.Now().Add(time.Millisecond)
	if err := s.RestartLearning(cutoff); err != nil {
		t.Fatal(err)
	}

	s.sampleAt(cutoff.Add(10 * time.Millisecond))

	if got := s.Model().Samples; got != 0 {
		t.Fatalf("pre-cutoff telemetry trained PV model: %d samples", got)
	}
}

func TestRestartLearningRejectsMixedPVWithOldInput(t *testing.T) {
	tel := telemetry.NewStore()
	now := time.Now()
	cutoff := now.Add(-time.Second)
	tel.Update("old-pv", telemetry.DerPV, -1000, nil, forecastPowerData(t, -1000, cutoff.Add(-time.Second), now))
	tel.RecordDriverSuccess("old-pv")
	tel.Update("new-pv", telemetry.DerPV, -2000, nil, forecastPowerData(t, -2000, cutoff.Add(time.Second), now))
	tel.RecordDriverSuccess("new-pv")
	s := NewService(nil, tel, func(time.Time) float64 { return 800 }, func(time.Time) (float64, bool) { return 20, true }, 5000)
	if err := s.RestartLearning(cutoff); err != nil {
		t.Fatal(err)
	}

	s.sampleAt(now.Add(time.Second))

	if got := s.Model().Samples; got != 0 {
		t.Fatalf("PV sum with pre-cutoff source trained model: %d samples", got)
	}
}

func TestRestartLearningIgnoresOldNonPVInputForPVTraining(t *testing.T) {
	tel := telemetry.NewStore()
	now := time.Now()
	cutoff := now.Add(-time.Second)
	tel.Update("pv", telemetry.DerPV, -3000, nil, forecastPowerData(t, -3000, cutoff.Add(time.Second), now))
	tel.RecordDriverSuccess("pv")
	tel.Update("battery", telemetry.DerBattery, 500, nil, forecastPowerData(t, 500, cutoff.Add(-time.Second), now))
	tel.RecordDriverSuccess("battery")
	s := NewService(nil, tel, func(time.Time) float64 { return 800 }, func(time.Time) (float64, bool) { return 20, true }, 5000)
	if err := s.RestartLearning(cutoff); err != nil {
		t.Fatal(err)
	}

	s.sampleAt(now.Add(time.Second))

	if got := s.Model().Samples; got != 1 {
		t.Fatalf("old non-PV input blocked fresh PV training: %d samples", got)
	}
}

func forecastPowerData(t *testing.T, watts float64, measured, received time.Time) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]telemetry.ForecastPowerSample{
		"forecast_power": {
			Version:      1,
			Known:        true,
			Watts:        watts,
			MeasuredAtMS: measured.UnixMilli(),
			ReceivedAtMS: received.UnixMilli(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRestartLearningInvalidatesPVSampleInFlight(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("pv", telemetry.DerPV, -3000, nil, nil)
	tel.RecordDriverSuccess("pv")
	entered := make(chan struct{})
	release := make(chan struct{})
	s := NewService(nil, tel, func(time.Time) float64 {
		close(entered)
		<-release
		return 800
	}, func(time.Time) (float64, bool) { return 20, true }, 5000)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.sampleAt(time.Now())
	}()
	<-entered
	cutoff := time.Now()
	if err := s.RestartLearning(cutoff); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done

	if got := s.Model().Samples; got != 0 || s.Residuals.Len() != 0 {
		t.Fatalf("in-flight sample survived restart: samples=%d residuals=%d", got, s.Residuals.Len())
	}
}

func TestRestartLearningPVRetryPersistsWithoutErasingNewSamples(t *testing.T) {
	bad := openTestDB(t)
	s := NewService(bad, nil, func(time.Time) float64 { return 800 }, nil, 5000)
	if err := bad.Close(); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC)
	if err := s.RestartLearning(cutoff); err == nil {
		t.Fatal("closed store did not surface persistence failure")
	}
	if !s.model.Update(800, 20, cutoff.Add(time.Minute), 3000) {
		t.Fatal("post-reset fixture sample rejected")
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
	restored := NewService(good, nil, func(time.Time) float64 { return 800 }, nil, 5000)
	if got := restored.Model(); got.Samples != 1 || got.LearningStartedMS != cutoff.UnixMilli() {
		t.Fatalf("same-cutoff retry erased or failed to persist new learning: %+v", got)
	}
}
