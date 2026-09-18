package loadmodel

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestResetPreservesHeatingCoefficient(t *testing.T) {
	s := NewService(nil, telemetry.NewStore(), "site", 4000, 17250)
	s.SetHeatingCoef(275)

	s.Reset()

	if got := s.Model().HeatingW_per_degC; got != 275 {
		t.Fatalf("heating coefficient after reset = %v, want 275", got)
	}
}

// TestSeedHeatingCoefDoesNotOverwriteLearnedValue — restart on a deployment
// where the load model has been adapting must not clobber the learned
// HeatingW_per_degC with the operator-config seed. Without this guarantee
// every binary update silently throws away weeks of online fit. The
// startup wiring in cmd/ftw/main.go uses SeedHeatingCoef
// precisely because operator config is a *prior*, not an override.
func TestSeedHeatingCoefDoesNotOverwriteLearnedValue(t *testing.T) {
	s := NewService(nil, telemetry.NewStore(), "site", 4000, 17250)
	// Simulate a model that has been adapting in production.
	s.mu.Lock()
	m := s.activeModelLocked()
	m.HeatingW_per_degC = 80.0
	m.Samples = 500
	s.mu.Unlock()

	s.SeedHeatingCoef(300) // operator config value carried into a restart

	if got := s.Model().HeatingW_per_degC; got != 80.0 {
		t.Errorf("learned heating coef must survive seed-only call: got %.0f, want 80", got)
	}
}

func TestSeedHeatingCoefAppliesOnColdStart(t *testing.T) {
	s := NewService(nil, telemetry.NewStore(), "site", 4000, 17250)
	// Fresh model — no samples observed yet.

	s.SeedHeatingCoef(300)

	if got := s.Model().HeatingW_per_degC; got != 300 {
		t.Errorf("cold-start seed should apply: got %.0f, want 300", got)
	}
}

func TestProfileSwitchTrainsOnlyActiveProfile(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("site")

	s := NewService(nil, tel, "site", 4000, 17250)
	now := time.Now()
	s.sampleAt(now)

	if err := s.SetProfile(ProfileAway); err != nil {
		t.Fatalf("set profile: %v", err)
	}
	tel.Update("site", telemetry.DerMeter, 200, nil, nil)
	tel.RecordDriverSuccess("site")
	s.sampleAt(time.Now())

	snap := s.Snapshot()
	if snap.ActiveProfile != ProfileAway {
		t.Fatalf("active profile = %q, want %q", snap.ActiveProfile, ProfileAway)
	}
	if got := snap.Profiles[ProfileHome].Samples; got != 1 {
		t.Fatalf("home samples = %d, want 1", got)
	}
	if got := snap.Profiles[ProfileAway].Samples; got != 1 {
		t.Fatalf("away samples = %d, want 1", got)
	}
}

func TestProfileAndModelsPersist(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()

	s := NewService(st, telemetry.NewStore(), "site", 4000, 17250)
	if err := s.SetProfile(ProfileAway); err != nil {
		t.Fatalf("set profile: %v", err)
	}
	t0 := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	s.mu.Lock()
	for i := 0; i < 8; i++ {
		s.activeModelLocked().Update(t0.AddDate(0, 0, 7*i), 250, HeatingReferenceC)
	}
	s.mu.Unlock()
	if err := s.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	restored := NewService(st, telemetry.NewStore(), "site", 4000, 17250)
	if got := restored.Profile(); got != ProfileAway {
		t.Fatalf("restored profile = %q, want %q", got, ProfileAway)
	}
	if got := restored.Model().Samples; got != 8 {
		t.Fatalf("restored away samples = %d, want 8", got)
	}
}

func TestSampleRequiresOnlineSiteMeter(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)

	s := NewService(nil, tel, "site", 4000, 17250)
	s.sampleAt(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC))

	if got := s.Model().Samples; got != 0 {
		t.Fatalf("samples = %d, want 0 when site meter has no online health", got)
	}
}

func TestSampleRejectsMissingDERs(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 4000, nil, nil)
	tel.RecordDriverSuccess("site")
	tel.Update("battery", telemetry.DerBattery, 3000, nil, nil)
	tel.DriverHealthMut("battery").SetOffline()
	s := NewService(nil, tel, "site", 4000, 11000)
	s.sampleAt(time.Now())
	if s.Model().Samples != 0 {
		t.Fatal("missing battery trained 4kW as house load")
	}
	tel.RecordDriverSuccess("battery")
	tel.SetDriverCommandFault("battery", true, "refused")
	s.sampleAt(time.Now())
	m := s.Model()
	if m.Samples != 1 || m.Bucket[m.hourOfWeek(time.Now())].Mean != 1000 {
		t.Fatalf("fresh command fault dropped real battery flow: %+v", m)
	}
}
