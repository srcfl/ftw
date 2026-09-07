package loadmodel

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestReconfigureBindsEveryProfileAndSurvivesRestart(t *testing.T) {
	st := openTestDB(t)
	s := NewService(st, telemetry.NewStore(), "site", 4000, 0)
	opts := telemetry.ForecastOptions{}
	if err := s.Reconfigure("site", opts, "Europe/Stockholm", "a"); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	for _, p := range Profiles() {
		s.models[p].Update(at, 300, 5)
	}
	if err := s.persist(); err != nil {
		t.Fatal(err)
	}
	restarted := NewService(st, telemetry.NewStore(), "site", 4000, 0)
	if err := restarted.Reconfigure("site", opts, "Europe/Stockholm", "a"); err != nil {
		t.Fatal(err)
	}
	for _, p := range Profiles() {
		if restarted.models[p].Samples != 1 {
			t.Fatalf("same binding lost %s", p)
		}
	}
	if err := restarted.Reconfigure("new-meter", opts, "Europe/Stockholm", "b"); err != nil {
		t.Fatal(err)
	}
	for _, p := range Profiles() {
		if m := restarted.models[p]; m.Samples != 0 || m.ConfigRevision != "b" {
			t.Fatalf("old profile survived: %s", p)
		}
	}
	// A partial save cannot bless a profile from the prior electrical boundary.
	restarted.models[ProfileAway].ConfigRevision = "a"
	restarted.models[ProfileAway].Update(at, 9999, 0)
	if err := restarted.persist(); err != nil {
		t.Fatal(err)
	}
	partial := NewService(st, telemetry.NewStore(), "new-meter", 4000, 0)
	partial.models[ProfileHome].Update(at, 200, 5)
	if err := partial.Reconfigure("new-meter", opts, "Europe/Stockholm", "b"); err != nil {
		t.Fatal(err)
	}
	for _, p := range Profiles() {
		if partial.models[p].Samples != 0 {
			t.Fatalf("mixed saved revisions retained %s", p)
		}
	}
}

func TestReconfigureRejectsTrainingCapturedAtPriorBoundary(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("site")
	s := NewService(nil, tel, "site", 4000, 0)
	if err := s.Reconfigure("site", telemetry.ForecastOptions{}, "UTC", "a"); err != nil {
		t.Fatal(err)
	}
	captured, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.Temp = func(time.Time) (float64, bool) { close(captured); <-release; return 5, true }
	go func() { defer close(done); s.sampleAt(time.Now()) }()
	<-captured
	if err := s.Reconfigure("site", telemetry.ForecastOptions{HouseholdInvalidReason: "changed topology"}, "UTC", "b"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if s.Model().Samples != 0 {
		t.Fatal("old balance trained new topology")
	}
}

func TestResetKeepsConfigBinding(t *testing.T) {
	s := NewService(nil, telemetry.NewStore(), "site", 4000, 0)
	if err := s.Reconfigure("site", telemetry.ForecastOptions{}, "UTC", "a"); err != nil {
		t.Fatal(err)
	}
	s.models[ProfileAway].Update(time.Now(), 300, 5)
	s.Reset()
	if s.Model().ConfigRevision != "a" || s.models[ProfileAway].Samples != 1 {
		t.Fatal("user reset changed binding or inactive profile")
	}
}
