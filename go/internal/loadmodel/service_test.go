package loadmodel

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

func TestResetPreservesHeatingCoefficient(t *testing.T) {
	s := NewService(nil, telemetry.NewStore(), "site", 4000)
	s.SetHeatingCoef(275)

	s.Reset()

	if got := s.Model().HeatingW_per_degC; got != 275 {
		t.Fatalf("heating coefficient after reset = %v, want 275", got)
	}
}

func TestProfileSwitchTrainsOnlyActiveProfile(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("site")

	s := NewService(nil, tel, "site", 4000)
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	s.sampleAt(now)

	if err := s.SetProfile(ProfileAway); err != nil {
		t.Fatalf("set profile: %v", err)
	}
	tel.Update("site", telemetry.DerMeter, 200, nil, nil)
	tel.RecordDriverSuccess("site")
	s.sampleAt(now.Add(time.Hour))

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

	s := NewService(st, telemetry.NewStore(), "site", 4000)
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

	restored := NewService(st, telemetry.NewStore(), "site", 4000)
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

	s := NewService(nil, tel, "site", 4000)
	s.sampleAt(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC))

	if got := s.Model().Samples; got != 0 {
		t.Fatalf("samples = %d, want 0 when site meter has no online health", got)
	}
}

func TestSampleUsesOnlyOnlineDERsAndSubtractsEV(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("site")

	tel.Update("pv-offline", telemetry.DerPV, -700, nil, nil)
	tel.DriverHealthMut("pv-offline").SetOffline()
	tel.Update("bat-offline", telemetry.DerBattery, -200, nil, nil)
	tel.DriverHealthMut("bat-offline").SetOffline()

	tel.Update("charger", telemetry.DerEV, 300, nil, nil)
	tel.RecordDriverSuccess("charger")

	s := NewService(nil, tel, "site", 4000)
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	s.sampleAt(now)

	m := s.Model()
	if m.Samples != 1 {
		t.Fatalf("samples = %d, want 1", m.Samples)
	}
	got := m.Bucket[HourOfWeek(now)].Mean
	if math.Abs(got-700) > 1 {
		t.Fatalf("bucket mean = %.1f, want house load 700 W", got)
	}
}
