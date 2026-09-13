package loadpoint

import "time"

// A counter and power can describe different moments. Keep the counter's
// timestamp so its eventual catch-up replaces, rather than adds to, the
// power measured over the same interval.
type sessionPowerPoint struct {
	at    time.Time
	w, wh float64
}

type sessionEnergy struct {
	counterWh    float64
	counterAt    time.Time
	counterKnown bool
	points       []sessionPowerPoint
	floorWh      float64
	floorAt      time.Time
	estimatedWh  float64
	source       string
}

func (e *sessionEnergy) counterRegressed(s EVSample) bool {
	return e.counterKnown && !s.SessionWhUnavailable && finite(s.SessionWh) && s.SessionWh >= 0 &&
		(s.EnergyAt.IsZero() || s.EnergyAt.After(e.counterAt)) && s.SessionWh < e.counterWh
}

func (e *sessionEnergy) integralAt(at time.Time) float64 {
	for i := len(e.points) - 1; i >= 0; i-- {
		p := e.points[i]
		if at.Before(p.at) {
			continue
		}
		if i+1 < len(e.points) {
			next := e.points[i+1]
			return p.wh + (next.wh-p.wh)*at.Sub(p.at).Seconds()/next.at.Sub(p.at).Seconds()
		}
		return p.wh
	}
	if len(e.points) > 0 {
		return e.points[0].wh
	}
	return 0
}

func (e *sessionEnergy) observe(s EVSample, now time.Time) float64 {
	powerAt := s.PowerAt
	if powerAt.IsZero() {
		powerAt = now
	}
	if !s.PowerUnavailable && finite(s.PowerW) && !powerAt.After(now.Add(time.Second)) && now.Sub(powerAt) <= 30*time.Second {
		n := len(e.points)
		if n == 0 {
			e.points = append(e.points, sessionPowerPoint{at: powerAt, w: max(0, s.PowerW)})
		} else if powerAt.After(e.points[n-1].at) {
			prev := e.points[n-1]
			wh := prev.wh
			if dt := powerAt.Sub(prev.at); dt <= 30*time.Second {
				wh += prev.w * dt.Hours()
			}
			e.points = append(e.points, sessionPowerPoint{at: powerAt, w: max(0, s.PowerW), wh: wh})
		}
	}
	counterAt := s.EnergyAt
	if counterAt.IsZero() {
		counterAt = now
	}
	if !s.SessionWhUnavailable && finite(s.SessionWh) && s.SessionWh >= 0 && !counterAt.After(now.Add(time.Second)) &&
		(!e.counterKnown || (counterAt.After(e.counterAt) && (!s.EnergyAt.IsZero() || s.SessionWh != e.counterWh))) {
		e.counterWh, e.counterAt, e.counterKnown = s.SessionWh, counterAt, true
	}
	total := 0.0
	if len(e.points) > 0 {
		total = e.points[len(e.points)-1].wh
	}
	e.source = "unavailable"
	estimate := total
	if total > 0 {
		e.source = "power"
	}
	if e.counterKnown {
		extra := max(0, total-e.integralAt(e.counterAt))
		estimate = e.counterWh + extra
		e.source = "meter"
		if extra > 0 {
			e.source = "power"
		}
	}
	if !e.floorAt.IsZero() && (!e.counterKnown || e.counterAt.Before(e.floorAt)) {
		estimate = max(estimate, e.floorWh+max(0, total-e.integralAt(e.floorAt)))
		e.source = "power"
	}
	e.estimatedWh = estimate
	// Two hours covers delayed cloud counters, with bounded storage. A gap is
	// left unmeasured; it never receives the next reading's power retroactively.
	if len(e.points) > 2048 || (len(e.points) > 2 && e.points[1].at.Before(now.Add(-2*time.Hour))) {
		// Retain accumulated work before trimming the time line. An overdue
		// counter must not make the oldest measured energy disappear.
		if last := e.points[len(e.points)-1]; !e.counterKnown || e.counterAt.Before(e.points[1].at) {
			e.floorWh, e.floorAt = estimate, last.at
		}
	}
	for len(e.points) > 2048 || (len(e.points) > 2 && e.points[1].at.Before(now.Add(-2*time.Hour))) {
		e.points = e.points[1:]
	}
	return estimate
}
