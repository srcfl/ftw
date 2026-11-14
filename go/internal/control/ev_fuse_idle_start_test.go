package control

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// firstOfferSince is the first order above 0 W from at on, and when it was
// sent; zero time when none was.
func (s *evMeterSite) firstOfferSince(at time.Time) evTimed {
	for _, o := range s.offersSince(at) {
		if o.w > 0 {
			return o
		}
	}
	return evTimed{}
}

// The home box on 25 Oct 2026: the car ran for minutes at the busy phase's
// working limit (17.9 of 19 A there; 22.9 of 24 A on this 25 A site), then
// stopped. Its next start read the heat its own current had left on top of
// the start it asked for: the cap stayed at 0 for 84 s, then gave 9 A. A
// start reads the phases now plus the car's new current; the heat, under the
// working limit, does not hold it back. Here the car runs at 14 A beside
// 10.5 A on L1, exactly the 24.5 A working limit.
func TestEVIdleStartReadsThePhasesNotTheCarsOwnHeat(t *testing.T) {
	s := newEVMeterSite(t, 0, 0)
	s.housePhaseA, s.houseW = [3]float64{10.5, 2, 2}, (10.5+2+2)*230
	s.run(10 * time.Minute)
	if s.offer() != 9660 || s.evW != 9660 {
		t.Fatalf("setup: offer %.0f, draw %.0f W, want 14 A on L1's working limit", s.offer(), s.evW)
	}
	s.ctrl.SetManualHold("garage", loadpoint.ManualHold{PowerW: 0, PhaseMode: "3p", Persistent: true})
	s.run(20 * time.Second)
	if s.evW != 0 {
		t.Fatalf("setup: the car still draws %.0f W after the stop", s.evW)
	}
	askedAt := s.now
	s.ctrl.SetManualHold("garage", loadpoint.ManualHold{PowerW: 10350, PhaseMode: "3p", Persistent: true})
	s.run(2 * time.Minute)
	start := s.firstOfferSince(askedAt)
	if start.at.IsZero() {
		t.Fatalf("no start within 2 min, L1 at %.1f A: offers %v", s.phaseA()[0], offerWatts(s.offersSince(askedAt)))
	}
	// The 35 s restart dwell runs from the car's stop, 15 s before the ask.
	if wait := start.at.Sub(askedAt); wait > 25*time.Second || start.w < 8970 {
		t.Fatalf("start %.0f W after %v with L1 at 10.5 A and 24.5 A to the working limit", start.w, wait)
	} else {
		t.Logf("start %.0f W after %v", start.w, wait)
	}
}

// A real overage on the busy phase heats the fuse past the working limit.
// When the house load drops, the start still waits for the heat filter to
// cool under the limit, and only then for the restart dwell.
func TestEVIdleStartWaitsForTheHeatOfARealOverage(t *testing.T) {
	s := newEVMeterSite(t, 0, 0)
	s.housePhaseA, s.houseW = [3]float64{26, 2, 2}, (26+2+2)*230
	s.run(5 * time.Minute)
	if s.offer() != 0 {
		t.Fatalf("setup: the car started at %.0f W with L1 at 26 A", s.offer())
	}
	droppedAt := s.now
	s.housePhaseA, s.houseW = [3]float64{8, 2, 2}, (8+2+2)*230
	s.run(3 * time.Minute)
	start := s.firstOfferSince(droppedAt)
	if start.at.IsZero() {
		t.Fatalf("no start within 3 min of the drop: offers %v", offerWatts(s.offersSince(droppedAt)))
	}
	// L1's filtered current stays over the 24.5 A line for about 8 s after
	// the drop: 60 s × ln((26² - 8²)/(24.5² - 8²)). The dwell starts only then.
	if wait := start.at.Sub(droppedAt); wait < evRestartDwell+7*time.Second {
		t.Fatalf("started %.0f W %v after the drop, before the heat of a 26 A overage had cooled", start.w, wait)
	} else {
		t.Logf("start %.0f W after %v", start.w, wait)
	}
}
