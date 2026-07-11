package calendar

import (
	"testing"
	"time"
)

func TestEVSessionRecordedFromSessionWh(t *testing.T) {
	s := newTestService(&fakeLP{}, &fakeLM{})
	t0 := time.Date(2026, 7, 1, 18, 0, 0, 0, time.UTC)

	if d := s.observeEV([]EVSample{{ID: "easee", Connected: true, Charging: true, SessionWh: 0, PowerW: 11000}}, t0); len(d) != 0 {
		t.Fatalf("no completion expected at session start, got %d", len(d))
	}
	s.observeEV([]EVSample{{ID: "easee", Connected: true, Charging: true, SessionWh: 5500, PowerW: 11000}}, t0.Add(30*time.Minute))
	done := s.observeEV([]EVSample{{ID: "easee", Connected: true, Charging: false, SessionWh: 5500, PowerW: 0}}, t0.Add(31*time.Minute))

	if len(done) != 1 {
		t.Fatalf("expected 1 completed session, got %d", len(done))
	}
	if done[0].EnergyWh != 5500 {
		t.Fatalf("energy: want 5500, got %v", done[0].EnergyWh)
	}
	if !done[0].Start.Equal(t0) || !done[0].End.Equal(t0.Add(31*time.Minute)) {
		t.Fatalf("session bounds wrong: %+v", done[0])
	}
	if done[0].ID != "easee" {
		t.Fatalf("id not carried: %q", done[0].ID)
	}
}

func TestEVShortSessionFiltered(t *testing.T) {
	s := newTestService(&fakeLP{}, &fakeLM{})
	t0 := time.Now()
	s.observeEV([]EVSample{{ID: "x", Connected: true, Charging: true, PowerW: 11000}}, t0)
	done := s.observeEV([]EVSample{{ID: "x", Connected: true, Charging: false, SessionWh: 5000}}, t0.Add(10*time.Second))
	if len(done) != 0 {
		t.Fatalf("sub-minute session should be filtered, got %d", len(done))
	}
}

func TestEVLowEnergyFiltered(t *testing.T) {
	s := newTestService(&fakeLP{}, &fakeLM{})
	t0 := time.Now()
	s.observeEV([]EVSample{{ID: "x", Connected: true, Charging: true, PowerW: 10}}, t0)
	done := s.observeEV([]EVSample{{ID: "x", Connected: true, Charging: false, SessionWh: 50}}, t0.Add(5*time.Minute))
	if len(done) != 0 {
		t.Fatalf("sub-100Wh session should be filtered, got %d", len(done))
	}
}

func TestEVUnplugEndsSession(t *testing.T) {
	s := newTestService(&fakeLP{}, &fakeLM{})
	t0 := time.Now()
	s.observeEV([]EVSample{{ID: "x", Connected: true, Charging: true, PowerW: 11000}}, t0)
	done := s.observeEV([]EVSample{{ID: "x", Connected: false, Charging: false, SessionWh: 7000}}, t0.Add(20*time.Minute))
	if len(done) != 1 || done[0].EnergyWh != 7000 {
		t.Fatalf("unplug should finish the session: %+v", done)
	}
}

func TestEVDriverDisappearEndsSession(t *testing.T) {
	s := newTestService(&fakeLP{}, &fakeLM{})
	t0 := time.Now()
	s.observeEV([]EVSample{{ID: "x", Connected: true, Charging: true, PowerW: 11000}}, t0)
	s.observeEV([]EVSample{{ID: "x", Connected: true, Charging: true, SessionWh: 6000, PowerW: 11000}}, t0.Add(30*time.Minute))
	done := s.observeEV(nil, t0.Add(31*time.Minute)) // driver gone entirely
	if len(done) != 1 || done[0].EnergyWh != 6000 {
		t.Fatalf("driver disappearance should finish the session: %+v", done)
	}
}

func TestEVIntegrationFallback(t *testing.T) {
	// Driver reports no SessionWh → energy comes from ∫ power·dt. Steps stay
	// under the 1h gap guard so the integration accumulates.
	s := newTestService(&fakeLP{}, &fakeLM{})
	t0 := time.Now()
	s.observeEV([]EVSample{{ID: "x", Connected: true, Charging: true, PowerW: 6000}}, t0)
	s.observeEV([]EVSample{{ID: "x", Connected: true, Charging: true, PowerW: 6000}}, t0.Add(30*time.Minute)) // +3000 Wh
	done := s.observeEV([]EVSample{{ID: "x", Connected: true, Charging: false, PowerW: 0}}, t0.Add(45*time.Minute))
	if len(done) != 1 {
		t.Fatalf("expected 1 session, got %d", len(done))
	}
	if done[0].EnergyWh < 2500 || done[0].EnergyWh > 3500 {
		t.Fatalf("integration fallback energy out of expected range: %v", done[0].EnergyWh)
	}
}

func TestWriteSessionUIDStable(t *testing.T) {
	// sanitizeUID keeps the path safe and the UID deterministic per session.
	if got := sanitizeUID("garage/1 left"); got != "garage-1-left" {
		t.Fatalf("sanitizeUID: got %q", got)
	}
}
