package control

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/sitesim"
)

// The site rig's s18 at 16 A: a 3 A house, Charge now for a car whose only
// step is 6 A on three phases, and a 6 kW battery charge hold beside it. The
// battery may charge up to the fuse's working limit (fuseLineA) beside the
// car. Its fuse guard must then land it on that line and keep it there; a
// guard that cuts below the line and lets it rise again after its hold makes
// the battery hunt, and where it stands when the house moves is chance. The
// rig judges that move: 0.1 A more house must come off the battery.
func TestBatteryBesideMinimumCarHoldsTheFuseLine(t *testing.T) {
	const carW = 6 * 690.0
	start := simStart
	p := simPlant(start)
	p.Fuse.RatingA, p.HouseW = 16, houseA(3, 3, 3)
	p.EVSE.Delay, p.EVSE.ReadbackLag = 2*time.Second, 0
	p.Car.MaxW = carW
	p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 6000, MaxDischargeW: 6000, CapacityWh: 10000, SoC: .4, Latency: 2 * time.Second}
	p.Events = []sitesim.Event{{At: 8 * time.Minute, Kind: sitesim.Change, Do: func(p *sitesim.Plant) { p.HouseW = houseA(3.1, 3.1, 3.1) }}}
	lp := simLoadpoint()
	lp.MinChargeW, lp.MaxChargeW, lp.AllowedStepsW = carW, carW, []float64{0, carW}
	// The rig's car draws a few watts either side of its offer, so the meter
	// reads the phases a hundredth of an amp either side of the line.
	jitter := rand.New(rand.NewPCG(18, 16))
	s := &simSite{p: p, lp: lp, meterNoise: func() float64 { return (jitter.Float64() - .5) * .02 },
		setup: func(s *simSite) {
			s.state.SetBatteryManualHold(BatteryManualHold{PowerW: 6000, ExpiresAt: start.Add(time.Hour)})
		}}
	s.boot()

	// The battery each second, from 3 min, once car and battery run.
	var battery []float64
	s.run(3 * time.Minute)
	for s.p.Now.Before(start.Add(11*time.Minute)) && s.failure == "" {
		s.step()
		battery = append(battery, s.p.Battery.W)
	}
	if s.failure != "" {
		t.Fatal(s.failure)
	}
	if s.p.EVSE.DrawW < carW-.1 {
		t.Fatalf("car draws %.0f W, want its %.0f W step", s.p.EVSE.DrawW, carW)
	}
	// Five minutes of a steady house: the battery stays where it is.
	steady := battery[:5*60]
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, w := range steady {
		lo, hi = math.Min(lo, w), math.Max(hi, w)
	}
	if hi-lo > 60 {
		var moves []string
		last := math.NaN()
		for _, c := range s.commands {
			if c.device == "battery" && c.at.After(start.Add(3*time.Minute)) && c.at.Before(start.Add(8*time.Minute)) && !(math.Abs(c.w-last) < 50) {
				moves, last = append(moves, fmt.Sprintf("%s %.0f", c.at.Sub(start), c.w)), c.w
			}
		}
		t.Errorf("battery hunts under a steady house: %.0f..%.0f W; commands %v", lo, hi, moves)
	}
	// The rig's judge: the 20 s before the step against the 20 s before 11 min.
	mean := func(w []float64) float64 {
		var sum float64
		for _, x := range w {
			sum += x
		}
		return sum / float64(len(w))
	}
	before, after := mean(battery[5*60-20:5*60]), mean(battery[len(battery)-20:])
	if after > before-40 {
		t.Errorf("battery %.0f W before 0.1 A more house, %.0f W after; want at least 40 W less", before, after)
	}
	t.Logf("battery %.0f..%.0f W steady, %.0f -> %.0f W across the step; peak phase %.2f A",
		lo, hi, before, after, s.peakA)
}
