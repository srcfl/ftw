package control

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// randomHouseKnown names the seeds that break an invariant on master, by
// kind, and the finding. A listed kind that passes fails the test until its
// line goes. The fuse's own kind is never listed.
var randomHouseKnown = map[uint64]map[string]string{
	// 25 A, 40 s charger, no declared response time (Core assumes 180 s). L2
	// steps to 30.8 A with the car at 8 A; a cut of 5 A a phase that landed
	// 94 s earlier, in the same run over the limit, still counts as on its
	// way, so Core cuts to 6 A, not 0, and L2 holds 28.8 A for another 40 s.
	2: {failLimit: "netting: a landed cut still covers the next overage, ev_fuse_budget.go:436"},
}

// Core keeps a random house under the fuse. For each fixed seed the house
// load on each phase steps at random to anywhere up to the fuse less 2 A,
// with the fuse warm from the first load, while a car charges on Charge now
// through a charger that answers in 2, 5 or 40 s. simSite.check holds the
// fuse's invariants every plant second for an hour: the fuse never blows,
// and no phase stays over the fuse less its margin longer than the
// charger's response plus 3 s, or 3 s after a raise of Core's own. Idle
// time with room is logged, not judged: the field matrix judges it with the
// fuse's own memory.
func TestSiteSimRandomHouse(t *testing.T) {
	for seed := uint64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(seed, 0))
			p := simPlant(simStart)
			p.Fuse.RatingA = []float64{16, 20, 25}[rng.IntN(3)]
			p.EVSE.Delay = []time.Duration{2 * time.Second, 5 * time.Second, 40 * time.Second}[rng.IntN(3)]
			house := func() [3]float64 {
				var a [3]float64
				for i := range a {
					a[i] = rng.Float64() * (p.Fuse.RatingA - 2)
				}
				return a
			}
			a := house()
			p.HouseW = houseA(a[0], a[1], a[2])
			if err := p.Fuse.Preheat(a); err != nil {
				t.Fatal(err)
			}
			s := &simSite{p: p, lp: simLoadpoint()}
			s.boot()
			failures, steps, damage := map[string]string{}, 0, 0.0
			next := func() time.Time { return p.Now.Add(time.Duration(15+rng.IntN(166)) * time.Second) }
			for at, end := next(), simStart.Add(time.Hour); p.Now.Before(end); {
				s.step()
				if s.failure != "" {
					// Keep each kind's first and run on, so a later blow still shows.
					if k := failureKind(s.failure); failures[k] == "" {
						failures[k] = s.failure
					}
					s.failure, s.overSince, s.limitSince, s.needSince = "", time.Time{}, time.Time{}, time.Time{}
				}
				damage = max(damage, p.Fuse.Damage[0], p.Fuse.Damage[1], p.Fuse.Damage[2])
				if !p.Now.Before(at) {
					a, at = house(), next()
					p.HouseW, s.outsideAt = houseA(a[0], a[1], a[2]), p.Now
					steps++
				}
			}
			t.Logf("%.0f A fuse, charger %v, %d house steps: peak phase %.1f A, %v over %.0f A, fuse damage at most %.2f, car idle with room %v, %.1f kWh to the car",
				p.Fuse.RatingA, p.EVSE.Delay, steps, s.peakA, s.overLimit, p.Fuse.RatingA-evPhaseMarginA, damage, s.idleWithRoom, p.EVSE.SessionWh/1000)
			delete(failures, failIdle)
			known := randomHouseKnown[seed]
			for _, k := range slices.Sorted(maps.Keys(failures)) {
				if fix, ok := known[k]; ok {
					t.Logf("fails as known (%s): %s", fix, failures[k])
				} else {
					t.Error(failures[k])
				}
			}
			for _, k := range slices.Sorted(maps.Keys(known)) {
				if failures[k] == "" {
					t.Errorf("%s passes now: delete its known line (%s)", k, known[k])
				}
			}
		})
	}
}
