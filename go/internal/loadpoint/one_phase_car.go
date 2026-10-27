package loadpoint

import (
	"math"
	"time"
)

// A one-phase car, as many plug-in hybrids are, draws on one phase of a
// three-phase charger: the order's current per phase, on that phase alone.
// The charger stays on three phases. The fuse already counts a three-phase
// order's current on every phase, which covers whichever phase the car
// uses. What changes is what the car gets: the plan counts one phase, and
// the order names the current per phase the car should take.
//
// The site meter shows it. When FTW's three-phase order rises by at least
// onePhaseStepA a phase, the meter's change from just before is watched over
// the charger's response time. One phase rising by about the order's change
// while the other two stay put is a one-phase car; all three rising is not.
// A house load does not follow FTW's orders, and the charger's own reading
// must have changed by what the phases show, so a kettle cannot mark a car
// and a heat pump cannot clear one. A two-phase car moves two phases and is
// never marked. With no reading from before the car drew, as after a Core
// restart, the charger's own draw is the reference: one phase carrying all
// of it and the other two less than a third, which no three- or two-phase
// car can show.

const (
	onePhaseStepA = 2.0              // the smallest order rise that is a test
	onePhaseHold  = 30 * time.Second // a verdict holds this long on the meter
	// onePhaseSettle is the least response time a test waits, and
	// onePhaseUnknownSettle the response time of a charger that declares
	// none, as the fuse limit counts it.
	onePhaseSettle        = time.Minute
	onePhaseUnknownSettle = 3 * time.Minute
)

// SitePhases is the site meter's current on each phase and when it read
// them.
type SitePhases struct {
	A  [3]float64
	At time.Time
}

// phaseTest is one order rise being watched on the meter.
type phaseTest struct {
	at, until time.Time
	baseline  [3]float64 // the meter just before the order took effect
	baseDrawA float64    // the charger's draw then, per one phase's voltage
	fromA     float64    // the order per phase before the rise
	verdict   int        // 1-3: one phase, that one; -1: three phases; 0: none
	since     time.Time  // when the meter began to show the verdict
}

// carPhases is one session's evidence. phase is 1-3 once the car is seen on
// that phase alone.
type carPhases struct {
	orderA float64 // per phase, the order the next rise is measured from
	// rising is set once the order has gone above orderA, and before is the
	// meter read before that first raise went out: a fast charger has
	// applied it by the next reading, and a fuse ramp climbs 1 A at a time.
	rising      bool
	before      [3]float64
	beforeDrawA float64
	// last and lastDrawA are the meter and the charger's draw read before
	// this tick's command; seen is false until the session has one.
	last      [3]float64
	lastDrawA float64
	seen      bool
	test      *phaseTest
	steady    int // the one phase the charger's whole draw shows on, 0 none
	since     time.Time
	phase     int
}

// phaseVerdict reads one meter reading against the baseline of a rise of
// riseA a phase. drawA is the change in the charger's own power since the
// baseline, over one phase's voltage.
func phaseVerdict(amps, baseline [3]float64, riseA, drawA float64) int {
	var moved [3]float64
	k, big := 0, math.Inf(-1)
	for i := range amps {
		moved[i] = amps[i] - baseline[i]
		if moved[i] > big {
			k, big = i, moved[i]
		}
	}
	// The car's phase rises by the order's rise at most, and by at least
	// half of it once the charger has applied it.
	if !finite(big) || big < math.Max(onePhaseStepA, riseA/2) || big > riseA+onePhaseStepA {
		return 0
	}
	quiet, rose := 0, 0
	for i, m := range moved {
		if i == k {
			continue
		}
		if math.Abs(m) <= math.Max(1, big/4) {
			quiet++
		}
		if m >= big/2 {
			rose++
		}
	}
	sum := moved[0] + moved[1] + moved[2]
	switch {
	case !finite(drawA):
		return 0
	case rose == 2 && drawA >= 0.6*sum && drawA <= 1.5*sum:
		return -1
	case quiet == 2 && drawA >= 0.6*big && drawA <= 1.5*big:
		return k + 1
	}
	return 0
}

// steadyVerdict is the check with no baseline: the phase (1-3) that carries
// all of the charger's draw while the other two carry less than a third of
// it, or 0. drawA is the charger's draw over one phase's voltage.
func steadyVerdict(amps [3]float64, drawA float64) int {
	if !finite(drawA) || drawA < 6 {
		return 0
	}
	phase, quiet := 0, 0
	for i, a := range amps {
		switch {
		case a >= drawA-1:
			phase = i + 1
		case a < drawA/3-1:
			quiet++
		}
	}
	if quiet == 2 {
		return phase
	}
	return 0
}

// SetSitePhases wires the site meter's phase currents for the one-phase car
// check. nil turns the check off.
func (c *Controller) SetSitePhases(f func() (SitePhases, bool)) {
	if c != nil {
		c.sitePhases = f
	}
}

// observePhases runs this tick's one-phase car check on a fresh meter
// reading. The response time is the charger's declared power age, at least
// onePhaseSettle, or onePhaseUnknownSettle when it declares none.
func (c *Controller) observePhases(cfg Config, sample EVSample, now time.Time) {
	site := c.siteFuse()
	if c.sitePhases == nil || !sample.Connected || site.Phases() != 3 {
		return
	}
	meter, ok := c.sitePhases()
	if !ok || meter.At.After(now.Add(time.Second)) || now.Sub(meter.At) > 10*time.Second {
		return
	}
	voltage := site.Voltage
	if !finite(voltage) || voltage <= 0 {
		voltage = 230
	}
	drawA := math.NaN()
	if !sample.PowerUnavailable && finite(sample.PowerW) {
		drawA = sample.PowerW / voltage
	}
	settle := onePhaseUnknownSettle
	if sample.PowerMaxAge > 0 {
		settle = max(onePhaseSettle, sample.PowerWindow())
	}
	c.manager.observePhases(cfg.ID, meter, drawA, voltage, settle, now)
}

// observePhases starts a test when the session's three-phase order rises
// by onePhaseStepA a phase, and judges it on the meter until the charger's
// response time and onePhaseHold have passed. The order may rise further
// meanwhile, as a plan catching up does; the test then expects that rise.
// A test whose order falls back is dropped. A one-phase verdict marks the
// session's car; a three-phase one clears the mark.
func (m *Manager) observePhases(id string, meter SitePhases, drawA, voltage float64, settle time.Duration, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok || !lp.pluggedIn {
		return
	}
	s := &lp.carPhases
	previous, previousDrawA, seen := s.last, s.lastDrawA, s.seen
	s.last, s.lastDrawA, s.seen = meter.A, drawA, true
	if !lp.commandedKnown || lp.commandedPhases != 3 || !finite(lp.commandedW) {
		s.test, s.rising, s.steady = nil, false, 0
		return
	}
	orderA := math.Max(0, lp.commandedW) / (3 * voltage)
	if t := s.test; t != nil && (orderA-t.fromA < onePhaseStepA || now.After(t.until)) {
		s.test = nil
	}
	if s.test == nil {
		switch {
		case orderA <= s.orderA:
			s.orderA, s.rising = orderA, false // A cut is the base of the next rise.
		case !seen:
			s.orderA = orderA // The order was out before the first reading.
		case !s.rising:
			s.rising, s.before, s.beforeDrawA = true, previous, previousDrawA
		}
		if !s.rising || orderA-s.orderA < onePhaseStepA {
			m.observeSteadyPhase(s, meter.A, drawA, now)
			return
		}
		s.test = &phaseTest{at: now, until: now.Add(settle + onePhaseHold), baseline: s.before,
			baseDrawA: s.beforeDrawA, fromA: s.orderA}
		s.orderA, s.rising, s.steady = orderA, false, 0
	}
	t := s.test
	s.orderA = max(s.orderA, orderA)
	if meter.At.Before(t.at) {
		return
	}
	if v := phaseVerdict(meter.A, t.baseline, orderA-t.fromA, drawA-t.baseDrawA); v != t.verdict {
		t.verdict, t.since = v, now
	}
	if t.verdict == 0 || now.Sub(t.since) < onePhaseHold {
		return
	}
	s.phase = max(t.verdict, 0)
	s.test = nil
}

// observeSteadyPhase marks an unmarked car whose charger's whole draw has
// shown on one phase alone for onePhaseHold. Caller holds m.mu.
func (m *Manager) observeSteadyPhase(s *carPhases, amps [3]float64, drawA float64, now time.Time) {
	if s.phase != 0 {
		return
	}
	if v := steadyVerdict(amps, drawA); v != s.steady {
		s.steady, s.since = v, now
	}
	if s.steady != 0 && now.Sub(s.since) >= onePhaseHold {
		s.phase = s.steady
	}
}

// chargerPhaseMaxA is the most current a charger offers on each phase of a
// three-phase order: its maximum over three phases, within the site fuse.
// A maximum below 6 A a phase still runs at 6 A (srcfl/ftw#1861).
func chargerPhaseMaxA(cfg Config, site SiteFuse) float64 {
	voltage := site.Voltage
	if !finite(voltage) || voltage <= 0 {
		voltage = 230
	}
	maxA := 32.0
	if finite(cfg.MaxChargeW) && cfg.MaxChargeW > 0 {
		maxA = math.Max(6, math.Floor(cfg.MaxChargeW/(3*voltage)+1e-9))
	}
	if finite(site.MaxAmps) && site.MaxAmps > 0 {
		maxA = math.Min(maxA, math.Floor(site.MaxAmps))
	}
	return maxA
}

// OnePhaseCarSteps are the levels a one-phase car takes from a three-phase
// charger: 6 A up to the charger's current per phase, on one phase.
func OnePhaseCarSteps(cfg Config, site SiteFuse) []float64 {
	voltage := site.Voltage
	if !finite(voltage) || voltage <= 0 {
		voltage = 230
	}
	steps := []float64{0}
	for a := 6.0; a <= chargerPhaseMaxA(cfg, site); a++ {
		steps = append(steps, a*voltage)
	}
	return steps
}

// OnePhaseCarMaxW is the most a one-phase car takes from the charger: its
// current per phase on one phase.
func OnePhaseCarMaxW(cfg Config, site SiteFuse) float64 {
	steps := OnePhaseCarSteps(cfg, site)
	return steps[len(steps)-1]
}
