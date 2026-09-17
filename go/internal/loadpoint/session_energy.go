package loadpoint

import "time"

// PowerWindow bounds the source's declared reporting cadence. Easee can leave
// power unchanged for two minutes; Core permits one extra minute of margin.
// This does not extend the separate transport/driver watchdog.
func (s EVSample) PowerWindow() time.Duration {
	if s.PowerMaxAge <= 0 {
		return 30 * time.Second
	}
	return min(s.PowerMaxAge, 3*time.Minute)
}

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
	coverageAt   time.Time
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
		if !e.coverageAt.IsZero() && at.After(p.at) {
			return p.wh + p.w*float64(max(0, min(at.UnixNano(), e.coverageAt.UnixNano())-p.at.UnixNano()))/float64(time.Hour)
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
	if !s.PowerUnavailable && finite(s.PowerW) && !powerAt.After(now.Add(time.Second)) && now.Sub(powerAt) <= s.PowerWindow() {
		n := len(e.points)
		if n == 0 {
			e.points = append(e.points, sessionPowerPoint{at: powerAt, w: max(0, s.PowerW)})
		} else if powerAt.After(e.points[n-1].at) {
			prev := e.points[n-1]
			wh := prev.wh
			if dt := powerAt.Sub(prev.at); dt <= s.PowerWindow() {
				wh += prev.w * dt.Hours()
			} else if e.coverageAt.After(prev.at) {
				// Retain an estimate already made while the source was valid,
				// without filling the later unobserved gap.
				wh += prev.w * e.coverageAt.Sub(prev.at).Hours()
			}
			e.points = append(e.points, sessionPowerPoint{at: powerAt, w: max(0, s.PowerW), wh: wh})
		}

		if n := len(e.points); n > 0 && !powerAt.Before(e.points[n-1].at) {
			e.coverageAt = e.points[n-1].at
			if s.PowerMaxAge > 0 && powerAt.Equal(e.points[n-1].at) && now.After(powerAt) {
				// The driver declares a bounded reporting interval. Between
				// its source updates this is explicitly power-estimated energy.
				e.coverageAt = now
			}
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
		total = e.integralAt(e.coverageAt)
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
		if !e.counterKnown || e.counterAt.Before(e.points[1].at) {
			e.floorWh, e.floorAt = estimate, e.coverageAt
		}
	}
	for len(e.points) > 2048 || (len(e.points) > 2 && e.points[1].at.Before(now.Add(-2*time.Hour))) {
		e.points = e.points[1:]
	}
	return estimate
}
