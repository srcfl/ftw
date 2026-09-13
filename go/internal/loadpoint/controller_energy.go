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
	powerWh, counterWh      float64
	points                  []energyPoint
}

// observeEnergy uses the charger counter when available, otherwise integrates
// successive fresh power readings. It never applies today's power to an entire
// elapsed slot. A transport/session change or an unmeasured gap breaks coverage.
func (c *Controller) observeEnergy(cfg Config, sample EVSample, now time.Time) {
	if c.energySamples == nil {
		c.energySamples = make(map[string]*meteredEnergy)
	}
	if !sample.Connected || sample.ConnectionUnknown || math.IsNaN(sample.PowerW) || math.IsInf(sample.PowerW, 0) {
		delete(c.energySamples, cfg.ID)
		return
	}
	e := c.energySamples[cfg.ID]
	if e == nil || e.driver != cfg.DriverName || e.device != sample.DeviceID || e.session != sample.SessionID || e.generation != sample.ConnectionGeneration {
		e = &meteredEnergy{driver: cfg.DriverName, device: sample.DeviceID, session: sample.SessionID, generation: sample.ConnectionGeneration}
		c.energySamples[cfg.ID] = e
	}
	if len(e.points) == 0 {
		e.points = []energyPoint{{at: now}}
		e.last = sample
		return
	}
	previous := e.points[len(e.points)-1]
	if !now.After(previous.at) {
		return
	}
	elapsed := now.Sub(previous.at)
	counterKnown := finite(sample.SessionWh) && finite(e.last.SessionWh) && sample.SessionWh >= e.last.SessionWh && (sample.SessionWh > 0 || e.last.SessionWh > 0)
	if elapsed > 30*time.Second && !counterKnown {
		e.points = nil
		e.powerWh, e.counterWh = 0, 0
	} else {
		if elapsed <= 30*time.Second {
			e.powerWh += max(0, e.last.PowerW) * elapsed.Hours()
		}
		if counterKnown {
			e.counterWh += sample.SessionWh - e.last.SessionWh
		}
	}
	// Counters can update less often than power. Stop conservatively on
	// either measured signal; adding their deltas would count energy twice.
	e.points = append(e.points, energyPoint{at: now, wh: max(e.powerWh, e.counterWh)})
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
