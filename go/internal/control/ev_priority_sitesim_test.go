package control

import (
	"fmt"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/sitesim"
)

// Exercise the whole loop, including delayed physical responses and fuse
// heating. A small house step must take battery charge first; a house
// that cannot fit the car must stop it, then let it restart without input.
func TestIssue1868ClosedLoopPriorityAndRecovery(t *testing.T) {
	for _, planned := range []bool{false, true} {
		for _, phases := range []int{1, 3} {
			for _, delay := range []time.Duration{5 * time.Second, 40 * time.Second, 120 * time.Second} {
				t.Run(fmt.Sprintf("plan=%v/phases=%d/charger=%s", planned, phases, delay), func(t *testing.T) {
					minimum := 6 * 230 * float64(phases)
					sc := simScenario{name: "issue1868", run: 20 * time.Minute,
						plant: func(p *sitesim.Plant) {
							p.Fuse.RatingA, p.HouseW = 16, houseA(3, 2, 2)
							p.EVSE.Phases, p.EVSE.Delay = phases, delay
							p.Car.Phases, p.Car.MaxW = phases, minimum
							p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 6000, MaxDischargeW: 6000, CapacityWh: 12200, SoC: .3, Latency: 2 * time.Second}
							p.Events = []sitesim.Event{
								{At: 5 * time.Minute, Kind: sitesim.Change, Do: func(p *sitesim.Plant) { p.HouseW = houseA(3.1, 2, 2) }},
								{At: 10 * time.Minute, Kind: sitesim.Change, Do: func(p *sitesim.Plant) { p.HouseW = houseA(16, 16, 16) }},
								{At: 14 * time.Minute, Kind: sitesim.Change, Do: func(p *sitesim.Plant) { p.HouseW = houseA(3, 2, 2) }},
							}
						},
						lp: func(c *loadpoint.Config) {
							c.MinChargeW, c.MaxChargeW = minimum, minimum
							c.PhaseMode = fmt.Sprintf("%dp", phases)
							c.AllowedStepsW = []float64{0, minimum}
						},
						setup: planBatteryCharge(6000),
					}
					if planned {
						sc.plan = planEV(minimum)
					}
					sc.expect = []simExpect{

						{9 * time.Minute, func(s *simSite) error {
							if s.p.EVSE.DrawW < minimum-.1 {
								return fmt.Errorf("car yielded to battery: car %.0f, battery %.0f W", s.p.EVSE.DrawW, s.batteryW())
							}
							for _, cmd := range s.commands {
								if cmd.device == "evse" && cmd.at.After(simStart.Add(5*time.Minute)) && cmd.w < minimum-.1 {
									return fmt.Errorf("small step cut the car: %v", cmd)
								}
							}
							return nil
						}},
						{14*time.Minute - time.Second, func(s *simSite) error {
							if s.p.EVSE.DrawW > 0 {
								return fmt.Errorf("car did not stop when house used the fuse: %.0f W", s.p.EVSE.DrawW)
							}
							return nil
						}},
						{20*time.Minute - time.Second, func(s *simSite) error {
							if s.p.EVSE.DrawW < minimum-.1 {
								return fmt.Errorf("car failed to restart: %.0f W; battery %.0f W", s.p.EVSE.DrawW, s.batteryW())
							}
							return nil
						}},
					}
					s := runSimScenario(sc, simStart)

					if s.p.Fuse.Blown != [3]bool{} {
						t.Fatalf("fuse opened: %+v", s.p.Fuse)
					}
					// Outside house steps may exceed the working margin during device
					// response. The fixture checks unsafe raises and sustained overages.
					if s.failure != "" {
						t.Fatal(s.failure)
					}
					t.Logf("car %.0f W, residual battery %.0f W, peak phase %.2f A", s.p.EVSE.DrawW, s.batteryW(), s.peakA)
				})
			}
		}
	}
}
