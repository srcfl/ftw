package telemetry

import (
	"testing"
	"time"
)

func TestControlWindowsAreBoundedAndMissingDataBreaksProof(t *testing.T) {
	s := NewStore()
	now := time.Now()
	for i := 0; i < 1000; i++ {
		s.recordControlObservation("battery", DerBattery, 1000, []byte(`{}`), now.Add(time.Duration(i)*time.Second))
	}
	points := s.controlObservations["battery:battery"]
	if len(points) != 32 {
		t.Fatalf("unbounded series: %d", len(points))
	}
	at := now.Add(999 * time.Second)
	w := observationWindow(points, at.Add(-12*time.Second), at)
	if !w.Usable(at) || w.MeanW != 1000 {
		t.Fatalf("stable window: %+v", w)
	}
	s.recordControlObservation("battery", DerBattery, 1000, []byte(`{"control_power_available":false}`), at.Add(time.Second))
	if len(s.controlObservations["battery:battery"]) != 0 {
		t.Fatal("missing power retained an apparently complete window")
	}
	s.recordControlObservation("battery", DerBattery, 1000, []byte(`{"power_observed_at":"2020-01-01T00:00:00Z"}`), at.Add(2*time.Second))
	if len(s.controlObservations["battery:battery"]) != 0 {
		t.Fatal("stale source became a new measurement")
	}
}

func TestProofDoesNotDependOnStatusPolling(t *testing.T) {
	s := NewStore()
	start := time.Now()
	// Let the site settle before requesting a change.
	for i := 0; i <= 6; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		s.mu.Lock()
		s.readings["battery:battery"] = &DerReading{Driver: "battery", DerType: DerBattery}
		s.recordControlObservation("battery", DerBattery, 0, []byte(`{}`), at)
		s.mu.Unlock()
	}
	c := s.BeginCommand("battery", []byte(`{"action":"battery","power_w":1000}`), start.Add(7*time.Second))
	s.CompleteCommand(c, "accepted")
	got, _ := s.CommandEvidence("battery", "battery")
	if got.Baseline["battery:battery"].Window.Count != 7 {
		t.Fatalf("no before-command window: %+v", got)
	}
	for i := 8; i <= 20; i++ {
		s.mu.Lock()
		s.observeCommand("battery", DerBattery, 1000, []byte(`{}`), start.Add(time.Duration(i)*time.Second))
		s.mu.Unlock()
	}
	got, _ = s.CommandEvidence("battery", "battery")
	if got.PowerMatchSince.IsZero() || got.LastObservation.Sub(got.PowerMatchSince) < 10*time.Second {
		t.Fatal("no proof without an API poll")
	}
}
