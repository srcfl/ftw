package telemetry

import (
	"encoding/json"
	"math"
	"time"
)

// Short, bounded raw observations for independent response checks. These are
// never model estimates or the house-load value derived from energy balance.
type ControlObservation struct {
	PowerW float64
	At     time.Time
}
type ControlWindow struct {
	MeanW, MinW, MaxW float64
	First, Last       time.Time
	Count             int
}
type ControlBaseline struct {
	Driver string
	Kind   DerType
	Window ControlWindow
	Points []ControlObservation
}

const ControlWindowDuration = 20 * time.Second

// Caller holds mu. At most one point per second and 64 points per DER.
func (s *Store) recordControlObservation(driver string, kind DerType, power float64, data json.RawMessage, now time.Time) {
	if kind == DerVehicle {
		return
	}
	k := key(driver, kind)
	invalidate := func() { delete(s.controlObservations, k) }
	p, known := ControlPowerObservation(power, data, now)
	if !known || now.Sub(p.At) > 10*time.Second {
		invalidate()
		return
	}
	at := p.At
	if s.controlObservations == nil {
		s.controlObservations = map[string][]ControlObservation{}
	}
	points := s.controlObservations[k]
	if len(points) > 0 && !at.After(points[len(points)-1].At) {
		return
	}
	if len(points) > 0 && at.Unix() == points[len(points)-1].At.Unix() {
		points[len(points)-1] = p
	} else {
		points = append(points, p)
	}
	if len(points) > 64 {
		copy(points, points[len(points)-64:])
		points = points[:64]
	}
	s.controlObservations[k] = points
}

func observationWindow(points []ControlObservation, from, now time.Time) ControlWindow {
	var w ControlWindow
	for _, p := range points {
		if p.At.Before(from) || p.At.After(now) {
			continue
		}
		if w.Count == 0 {
			w.MinW, w.MaxW, w.First = p.PowerW, p.PowerW, p.At
		}
		w.MeanW += p.PowerW
		w.MinW = math.Min(w.MinW, p.PowerW)
		w.MaxW = math.Max(w.MaxW, p.PowerW)
		w.Last = p.At
		w.Count++
	}
	if w.Count > 0 {
		w.MeanW /= float64(w.Count)
	}
	return w
}
func (w ControlWindow) Usable(now time.Time) bool {
	return w.Count >= 2 && w.Last.Sub(w.First) >= 5*time.Second && !w.Last.After(now) && now.Sub(w.Last) <= 10*time.Second
}
func (s *Store) controlBaseline(now time.Time) map[string]ControlBaseline {
	result := map[string]ControlBaseline{}
	for k, rd := range s.readings {
		if rd.DerType == DerVehicle {
			continue
		}
		from := now.Add(-ControlWindowDuration)
		w := observationWindow(s.controlObservations[k], from, now)
		if w.Usable(now) {
			result[k] = ControlBaseline{rd.Driver, rd.DerType, w, copyControlPoints(s.controlObservations[k], from, now)}
		}
	}
	return result
}
func (s *Store) ControlWindows(from, now time.Time) map[string]ControlBaseline {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := map[string]ControlBaseline{}
	for k, rd := range s.readings {
		if rd.DerType == DerVehicle {
			continue
		}
		result[k] = ControlBaseline{rd.Driver, rd.DerType, observationWindow(s.controlObservations[k], from, now), copyControlPoints(s.controlObservations[k], from, now)}
	}
	return result
}

func copyControlPoints(points []ControlObservation, from, until time.Time) []ControlObservation {
	var out []ControlObservation
	for _, p := range points {
		if !p.At.Before(from) && !p.At.After(until) {
			out = append(out, p)
		}
	}
	return out
}

// ControlPowerObservation preserves measurement time even when a cloud/status
// poll is fresh. OCPP status frames and cached vendor observations are not new
// power samples. Legacy local drivers retain their fresh-read receipt contract.
func ControlPowerObservation(power float64, data json.RawMessage, receivedAt time.Time) (ControlObservation, bool) {
	var d struct {
		ControlPowerW          *float64             `json:"control_power_w"`
		ControlPowerAvailable  *bool                `json:"control_power_available"`
		ControlPowerObservedAt string               `json:"control_power_observed_at"`
		PowerObservedAt        string               `json:"power_observed_at"`
		PowerMaxAgeS           float64              `json:"power_max_age_s"`
		ForecastPower          *ForecastPowerSample `json:"forecast_power"`
	}
	p := ControlObservation{PowerW: power, At: receivedAt}
	if len(data) > 0 && json.Unmarshal(data, &d) != nil {
		return p, false
	}
	if d.ControlPowerAvailable != nil && !*d.ControlPowerAvailable {
		return p, false
	}
	if d.ForecastPower != nil {
		f := d.ForecastPower
		if f.Version != 1 || !f.Known || f.MeasuredAtMS <= 0 || f.ReceivedAtMS < f.MeasuredAtMS || f.ReceivedAtMS > receivedAt.UnixMilli() {
			return p, false
		}
		p.PowerW, p.At = f.Watts, time.UnixMilli(f.MeasuredAtMS)
	}
	if d.ControlPowerW != nil {
		p.PowerW = *d.ControlPowerW
	}
	stamp := d.PowerObservedAt
	if d.ControlPowerObservedAt != "" {
		stamp = d.ControlPowerObservedAt
	}
	if stamp != "" {
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return p, false
		}
		p.At = at
	}
	age := time.Duration(d.PowerMaxAgeS * float64(time.Second))
	if age <= 0 || age > 3*time.Minute {
		age = time.Minute
	}
	return p, finite(p.PowerW) && !p.At.After(receivedAt) && receivedAt.Sub(p.At) <= age
}
