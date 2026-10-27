package control

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/sitesim"
)

// A charger that declares no response time and takes delay to start, beside
// a planned 9.4 kW battery charge on a 25 A site with 3 A of house load per
// phase. The car's 11 kW offer stays unshown until the charger applies it,
// and the battery must not take that room: when the car starts, no phase
// may pass the instant limit, and the car must not sit idle with room.
func TestSlowChargerUnshownOfferKeepsBatteryOut(t *testing.T) {
	for _, delay := range []time.Duration{5 * time.Second, 30 * time.Second, 40 * time.Second, 2 * time.Minute} {
		sc := simScenario{name: "slow charger", run: 15 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.HouseW, p.EVSE.Delay = houseA(3, 3, 3), delay
				p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 9400, MaxDischargeW: 9400, CapacityWh: 20000, SoC: .3, Latency: 3 * time.Second}
			},
			setup: planBatteryCharge(9400)}
		s := runSimScenario(sc, simStart)
		t.Logf("delay %v: peak phase %.1f A, %v over %.0f A, idle with room %v, car %.0f W, battery %.0f W",
			delay, s.peakA, s.overLimit, s.p.Fuse.RatingA-evPhaseMarginA, s.idleWithRoom, s.p.EVSE.DrawW, s.p.Battery.W)
		if s.failure != "" {
			t.Errorf("delay %v: %s", delay, s.failure)
		}
		if limitA := fuseInstantFactor * s.p.Fuse.RatingA; s.peakA > limitA {
			t.Errorf("delay %v: peak phase %.1f A above the instant limit %.1f A", delay, s.peakA, limitA)
		}
	}
}
