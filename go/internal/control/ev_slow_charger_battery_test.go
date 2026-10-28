package control

import (
	"fmt"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/sitesim"
)

// A charger that declares a 180 s response time, as Easee Cloud does, and
// takes delay to start, beside a planned 9.4 kW battery charge on a 25 A
// site with 3 A of house load per phase. The car's 11 kW offer stays
// unshown until the charger applies it, and the battery must not take that
// room: when the car starts, no phase may pass the instant limit, and the
// car must not sit idle with room.
func TestSlowChargerUnshownOfferKeepsBatteryOut(t *testing.T) {
	for _, delay := range []time.Duration{5 * time.Second, 30 * time.Second, 40 * time.Second, 2 * time.Minute} {
		sc := simScenario{name: "slow charger", run: 15 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.HouseW, p.EVSE.Delay = houseA(3, 3, 3), delay
				p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 9400, MaxDischargeW: 9400, CapacityWh: 20000, SoC: .3, Latency: 3 * time.Second}
			},
			setup: func(s *simSite) {
				s.evMaxAge = evUnknownSettle
				planBatteryCharge(9400)(s)
			}}
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

// #1868's rig shape (s18): a 16 A site with 3 A of house load, a local
// charger that declares no response time and draws within 2 s, a car that
// takes only [0, 4140] W, and a 6000 W battery charge. Once the car draws,
// the reading shows it, and the battery shares the rest of the fuse well
// before evUnknownSettle; s18's judge needs it above 1000 W 100-120 s after
// Charge now.
func TestFastChargerYoungOfferLetsBatteryShare(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*simSite)
	}{
		{"manual hold", func(s *simSite) {
			s.state.SetBatteryManualHold(BatteryManualHold{PowerW: 6000, ExpiresAt: s.p.Now.Add(time.Hour)})
		}},
		{"plan", planBatteryCharge(6000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var expect []simExpect
			for _, at := range []time.Duration{60 * time.Second, 100 * time.Second, 120 * time.Second, 170 * time.Second} {
				expect = append(expect, simExpect{at, func(s *simSite) error {
					if s.p.EVSE.DrawW < 4100 || s.p.Battery.W < 3000 {
						return fmt.Errorf("car %.0f W and battery %.0f W at %v; both should charge", s.p.EVSE.DrawW, s.p.Battery.W, at)
					}
					return nil
				}})
			}
			sc := simScenario{name: "s18 " + tc.name, run: 4 * time.Minute,
				plant: func(p *sitesim.Plant) {
					p.Fuse.RatingA, p.HouseW, p.EVSE.Delay, p.EVSE.ReadbackLag = 16, houseA(3, 3, 3), 2*time.Second, 0
					p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 6000, MaxDischargeW: 6000, CapacityWh: 20000, SoC: .4, Latency: 2 * time.Second}
				},
				lp: func(c *loadpoint.Config) {
					c.MinChargeW, c.MaxChargeW, c.AllowedStepsW = 4140, 4140, []float64{0, 4140}
				},
				setup:  tc.setup,
				expect: expect}
			s := runSimScenario(sc, simStart)
			if s.failure != "" {
				t.Error(s.failure)
			}
			if limitA := fuseInstantFactor * s.p.Fuse.RatingA; s.peakA > limitA {
				t.Errorf("peak phase %.1f A above the instant limit %.1f A", s.peakA, limitA)
			}
		})
	}
}
