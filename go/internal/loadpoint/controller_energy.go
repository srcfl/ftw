package loadpoint

import (
	"math"
	"time"
)

type energyPoint struct {
	at time.Time
	wh float64
}

type meteredEnergy struct {
	driver, device, session string
	generation              uint64
	last                    EVSample
	meter                   sessionEnergy
	points                  []energyPoint
}

// observeEnergy uses the charger counter when available, otherwise integrates
// successive fresh power readings. It never applies today's power to an entire
// elapsed slot. A transport/session change or an unmeasured gap breaks coverage.
func (c *Controller) observeEnergy(cfg Config, sample EVSample, now time.Time) {
	if c.energySamples == nil {
		c.energySamples = make(map[string]*meteredEnergy)
	}
	if !sample.Connected || sample.ConnectionUnknown || sample.PowerUnavailable || math.IsNaN(sample.PowerW) || math.IsInf(sample.PowerW, 0) {
		delete(c.energySamples, cfg.ID)
		return
	}
	e := c.energySamples[cfg.ID]
	if e == nil || e.driver != cfg.DriverName || e.device != sample.DeviceID || e.session != sample.SessionID || e.generation != sample.ConnectionGeneration || e.meter.counterRegressed(sample) {
		e = &meteredEnergy{driver: cfg.DriverName, device: sample.DeviceID, session: sample.SessionID, generation: sample.ConnectionGeneration}
		c.energySamples[cfg.ID] = e
	}
	measuredAt := now
	if !sample.PowerAt.IsZero() {
		measuredAt = sample.PowerAt
		if !sample.SessionWhUnavailable && sample.EnergyAt.After(measuredAt) {
			measuredAt = sample.EnergyAt
		}
	}
	if sample.PowerMaxAge > 0 && !sample.PowerUnavailable && now.Sub(sample.PowerAt) <= sample.PowerWindow() {
		measuredAt = now
	}
	if measuredAt.After(now) {
		return
	}
	if len(e.points) > 0 {
		previous := e.points[len(e.points)-1]
		if !measuredAt.After(previous.at) {
			return
		}
		counterAdvanced := !sample.SessionWhUnavailable && sample.SessionWh > e.last.SessionWh && (sample.EnergyAt.IsZero() || sample.EnergyAt.After(previous.at))
		if measuredAt.Sub(previous.at) > sample.PowerWindow() && !counterAdvanced {
			e.points = nil
		}
	}
	counterWasKnown := e.meter.counterKnown
	wh := e.meter.observe(sample, now)
	if !counterWasKnown && e.meter.counterKnown {
		// A first counter includes energy from before this slot. Align prior
		// power points to its baseline before calculating slot delivery.
		baseline := e.meter.counterWh - e.meter.integralAt(e.meter.counterAt)
		for i := range e.points {
			e.points[i].wh += baseline
		}
	}
	e.points = append(e.points, energyPoint{at: measuredAt, wh: wh})
	e.last = sample
	// Keep one boundary reading for a two-hour slot, with a hard cap for
	// callers ticking faster than production's five-second loop.
	for len(e.points) > 2048 || (len(e.points) > 2 && e.points[1].at.Before(now.Add(-2*time.Hour))) {
		e.points = e.points[1:]
	}
}

// The second result is time before the first measurement. Dispatch reserves
// the planned share for that time rather than claiming it measured delivery.
func (c *Controller) energySince(id string, start, now time.Time) (float64, float64) {
	e := c.energySamples[id]
	if e == nil || len(e.points) == 0 || now.Before(start) {
		return 0, max(0, now.Sub(start).Seconds())
	}
	points := e.points
	last := points[len(points)-1]
	if start.Before(points[0].at) {
		return max(0, last.wh-points[0].wh), points[0].at.Sub(start).Seconds()
	}
	for i := len(points) - 1; i >= 0; i-- {
		p := points[i]
		if p.at.After(start) {
			continue
		}
		initial := p.wh
		if !p.at.Equal(start) && i+1 < len(points) {
			next := points[i+1]
			initial += (next.wh - p.wh) * start.Sub(p.at).Seconds() / next.at.Sub(p.at).Seconds()
		}
		return max(0, last.wh-initial), 0
	}
	return 0, max(0, now.Sub(start).Seconds())
}
