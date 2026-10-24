package control

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// A field case on v0.155.0-beta.1 (Easee Cloud, 25 A fuse): a charge at
// 10,350 W, a 25 s pause, then a 4,140 W request. Easee declares 180 s, so
// the rise sent before the pause stayed reserved, and the restart waited
// close to three minutes with fuse_limit while the phases read about 1.3 A.
// A cut ends the reservation of the higher orders before it: the charger
// moves to its latest order, and the meter shows its draw until the cut
// lands.
func TestEVHardLimitRestartAfterPauseIgnoresReplacedRise(t *testing.T) {
	s := newEVMeterSite(t, 0, 900)
	s.chargerDelay, s.telemetryDelay, s.chargerMaxAgeS = 5*time.Second, 10*time.Second, 180
	s.run(25 * time.Second)
	if s.offer() <= 0 {
		t.Fatalf("the car did not start before the pause: offers %v", offerWatts(s.offers))
	}
	s.ctrl.SetManualHold("garage", loadpoint.ManualHold{PowerW: 0, PhaseMode: "3p", Persistent: true})
	s.run(25 * time.Second)
	if s.offer() != 0 {
		t.Fatalf("the pause did not stop the offer: %v", offerWatts(s.offers))
	}
	askedAt := s.now
	s.ctrl.SetManualHold("garage", loadpoint.ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true})
	s.run(3 * time.Minute)
	var startedAt time.Time
	for _, o := range s.offersSince(askedAt) {
		if o.w > 0 {
			startedAt = o.at
			break
		}
	}
	if startedAt.IsZero() {
		t.Fatalf("no offer within 3 min after the restart: %v", offerWatts(s.offersSince(askedAt)))
	}
	// The idle-start dwell (35 s) may still apply; the replaced rise must not.
	if wait := startedAt.Sub(askedAt); wait > 60*time.Second {
		t.Fatalf("restart waited %v for a 4,140 W offer with the phases near 1.3 A", wait)
	} else {
		t.Logf("restart offered after %v", wait)
	}
}
