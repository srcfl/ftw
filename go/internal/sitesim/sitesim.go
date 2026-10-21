// Package sitesim is a simulated house behind one main fuse, for testing Core
// in a closed loop. It holds the physics and the devices' own behaviour; Core
// reads what the devices report and sends them commands. A test advances it
// one second at a time on a pinned clock. Nothing here sleeps or starts a
// goroutine.
//
// Signs follow docs/site-convention.md: positive W flows into the site.
// Phase currents are signed the same way unless a meter drops the sign.
package sitesim

import (
	"math"
	"time"
)

// Step is the plant's time step.
const Step = time.Second

// Plant is one site. Set its fields before the first Step and change them
// from events.
type Plant struct {
	Now     time.Time
	Voltage float64
	HouseW  [3]float64 // house load per phase
	PVW     float64    // three-phase PV, zero or negative
	Battery *Battery   // nil: none
	EVSE    *EVSE      // nil: none
	Car     *Car       // the car on the EVSE; nil or unplugged: none
	Meter   Meter
	Fuse    Fuse
	Events  []Event // in time order; each fires once

	A       [3]float64 // physical phase currents after the last step
	start   time.Time
	offline map[string]time.Time
}

// New returns a plant starting at start with a fuse of fuseA per phase.
func New(start time.Time, fuseA float64) *Plant {
	return &Plant{Now: start, start: start, Voltage: 230, Fuse: Fuse{RatingA: fuseA}, offline: map[string]time.Time{}}
}

// EventKind says what an event does.
type EventKind int

const (
	Change        EventKind = iota // run Do on the plant
	CoreRestart                    // Core stops for For and starts with no memory
	MeterSilent                    // the site meter sends nothing for For
	DriverOffline                  // Device sends nothing for For; Offline says so, and callers send it nothing
)

// Event is one change on the timeline, At after the plant started.
type Event struct {
	At     time.Duration
	Kind   EventKind
	For    time.Duration
	Device string // "battery" or "evse"
	Do     func(*Plant)
}

// Tick is what one second produced. A nil reading means the device sent
// nothing this second.
type Tick struct {
	Meter   *MeterReading
	Battery *BatteryReading
	EV      *EVReading
	Restart time.Duration // > 0: Core goes down now for this long
}

// MeterReading is one site meter reading. Phase power keeps its sign
// when the currents lose theirs, as on P1.
type MeterReading struct {
	W      float64
	PhaseA [3]float64
	PhaseW [3]float64
}

// BatteryReading is what the battery reports.
type BatteryReading struct{ W, SoC float64 }

// EVReading is what the charger reports. W is ReadbackLag late.
type EVReading struct {
	W, SessionWh float64
	Plugged      bool
}

// Offline reports whether device is unreachable now.
func (p *Plant) Offline(device string) bool { return p.Now.Before(p.offline[device]) }

// Step advances one second: events, device responses, physics, fuse and
// readings, in that order.
func (p *Plant) Step() Tick {
	p.Now = p.Now.Add(Step)
	var tick Tick
	for len(p.Events) > 0 && p.Now.Sub(p.start) >= p.Events[0].At {
		e := p.Events[0]
		p.Events = p.Events[1:]
		switch e.Kind {
		case Change:
			e.Do(p)
		case CoreRestart:
			tick.Restart = e.For
		case MeterSilent:
			p.Meter.silentUntil = p.Now.Add(e.For)
		case DriverOffline:
			p.offline[e.Device] = p.Now.Add(e.For)
		}
	}
	var batW [3]float64
	if b := p.Battery; b != nil {
		b.step(p.Now)
		batW = spread(b.W, b.Phases)
	}
	for i := range p.A {
		p.A[i] = (p.HouseW[i] + p.PVW/3 + batW[i]) / p.Voltage
	}
	if e := p.EVSE; e != nil {
		evA := spread(e.step(p.Now, p.Car, p.A, p.Voltage), e.DrawPhases)
		for i := range p.A {
			p.A[i] += evA[i] / p.Voltage
		}
	}
	p.Fuse.step(p.A, Step.Seconds())
	if p.Meter.due(p.Now) {
		r := &MeterReading{PhaseA: p.A}
		for i, a := range p.A {
			r.PhaseW[i] = a * p.Voltage
			r.W += r.PhaseW[i]
			if p.Meter.Unsigned {
				r.PhaseA[i] = math.Abs(a)
			}
		}
		r.W += p.Meter.TotalBiasW
		tick.Meter = r
	}
	if b := p.Battery; b != nil && !p.Offline("battery") {
		tick.Battery = &BatteryReading{W: b.W, SoC: b.SoC}
	}
	if e := p.EVSE; e != nil && !p.Offline("evse") {
		tick.EV = &EVReading{W: e.readback(p.Now), SessionWh: e.SessionWh, Plugged: p.Car != nil && p.Car.Plugged}
	}
	return tick
}

// spread puts w on L1 alone or evenly on three phases.
func spread(w float64, phases int) [3]float64 {
	if phases == 1 {
		return [3]float64{w, 0, 0}
	}
	return [3]float64{w / 3, w / 3, w / 3}
}

type timedW struct {
	at time.Time
	w  float64
}

// Meter is the site meter. It reads the boundary every Every, or every
// second when Every is zero.
type Meter struct {
	Every       time.Duration
	Unsigned    bool    // phase currents as magnitudes
	TotalBiasW  float64 // added to the total alone, as a Pixii total that disagrees with its phases
	last        time.Time
	silentUntil time.Time
}

func (m *Meter) due(now time.Time) bool {
	if now.Before(m.silentUntil) || (!m.last.IsZero() && now.Sub(m.last) < m.Every) {
		return false
	}
	m.last = now
	return true
}

// Battery is a battery inverter that follows a power command after Latency.
type Battery struct {
	Phases                    int // 1 (on L1) or 3
	MaxChargeW, MaxDischargeW float64
	CapacityWh, SoC           float64
	Latency                   time.Duration
	W                         float64 // power now, positive charging
	queue                     []timedW
}

// Command asks for w from now + Latency.
func (b *Battery) Command(now time.Time, w float64) {
	b.queue = append(b.queue, timedW{now.Add(b.Latency), w})
}

func (b *Battery) step(now time.Time) {
	for len(b.queue) > 0 && !b.queue[0].at.After(now) {
		b.W, b.queue = b.queue[0].w, b.queue[1:]
	}
	b.W = math.Max(-b.MaxDischargeW, math.Min(b.MaxChargeW, b.W))
	if (b.W > 0 && b.SoC >= 1) || (b.W < 0 && b.SoC <= 0) {
		b.W = 0
	}
	if b.CapacityWh > 0 {
		b.SoC = math.Max(0, math.Min(1, b.SoC+b.W*Step.Hours()/b.CapacityWh))
	}
}

// EVSE is a charger with its driver: it turns a power command into a
// current limit, as the Easee driver does, and applies it after Delay.
type EVSE struct {
	Phases        int // wired phases, 1 or 3
	MinA, MaxA    float64
	Delay         time.Duration // from command to current at the car
	ReadbackLag   time.Duration // the charger reports the draw this late
	LoadBalancerA float64       // > 0: its own balancer keeps each phase under this
	OfferA        float64       // current limit in force
	OfferPhases   int
	DrawW         float64 // the car's draw now
	DrawPhases    int
	SessionWh     float64 // since the car plugged in
	queue         []evCommand
	draws         []timedW
}

type evCommand struct {
	at     time.Time
	a      float64
	phases int
}

// Command sends powerW in phaseMode ("1p", "3p", "auto" or empty). Auto
// picks one phase below splitW, as the Easee driver does without its
// hysteresis. Below MinA the charger pauses.
func (e *EVSE) Command(now time.Time, powerW float64, phaseMode string, splitW, voltage float64) {
	phases := e.Phases
	if phaseMode == "1p" || (phaseMode == "auto" && powerW < math.Max(splitW, 3*voltage*e.MinA)) {
		phases = 1
	}
	a := math.Min(math.Floor(powerW/voltage/float64(phases)+.5), e.MaxA)
	if a < e.MinA {
		a = 0
	}
	e.queue = append(e.queue, evCommand{now.Add(e.Delay), a, phases})
}

// step applies due commands and returns the car's draw. The car takes the
// offer, its own maximum and the balancer's room, whichever is least.
func (e *EVSE) step(now time.Time, car *Car, houseA [3]float64, voltage float64) float64 {
	for len(e.queue) > 0 && !e.queue[0].at.After(now) {
		e.OfferA, e.OfferPhases, e.queue = e.queue[0].a, e.queue[0].phases, e.queue[1:]
	}
	w := 0.0
	if car != nil && car.Plugged && e.OfferA > 0 {
		e.DrawPhases = min(max(e.OfferPhases, 1), e.Phases, car.Phases)
		a := math.Min(e.OfferA, car.MaxW/voltage/float64(e.DrawPhases))
		if e.LoadBalancerA > 0 {
			for i := range e.DrawPhases {
				a = math.Min(a, e.LoadBalancerA-houseA[i])
			}
		}
		if a >= e.MinA {
			w = a * voltage * float64(e.DrawPhases)
		}
	}
	e.DrawW, e.SessionWh = w, e.SessionWh+w*Step.Hours()
	if car == nil || !car.Plugged {
		e.SessionWh = 0
	}
	if len(e.draws) == 0 || e.draws[len(e.draws)-1].w != w {
		e.draws = append(e.draws, timedW{now, w})
	}
	return w
}

// readback is the draw as it was ReadbackLag ago.
func (e *EVSE) readback(now time.Time) float64 {
	seen := now.Add(-e.ReadbackLag)
	for len(e.draws) > 1 && !e.draws[1].at.After(seen) {
		e.draws = e.draws[1:]
	}
	if len(e.draws) == 0 || e.draws[0].at.After(seen) {
		return 0
	}
	return e.draws[0].w
}

// Car is the vehicle on the EVSE.
type Car struct {
	Phases  int     // phases its onboard charger uses
	MaxW    float64 // the most it takes, whatever it is offered
	Plugged bool
}
