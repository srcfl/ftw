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
}

// Caller holds mu. At most one point per second and 32 points per DER.
func (s *Store) recordControlObservation(driver string, kind DerType, power float64, data json.RawMessage, now time.Time) {
	if kind == DerVehicle {
		return
	}
	k := key(driver, kind)
	invalidate := func() { delete(s.controlObservations, k) }
	var d struct {
		ControlPowerW         *float64 `json:"control_power_w"`
		ControlPowerAvailable *bool    `json:"control_power_available"`
		PowerObservedAt       string   `json:"power_observed_at"`
	}
	if len(data) > 0 && json.Unmarshal(data, &d) != nil {
		invalidate()
		return
	}
	if d.ControlPowerAvailable != nil && !*d.ControlPowerAvailable {
		invalidate()
		return
	}
	if d.ControlPowerW != nil {
		power = *d.ControlPowerW
	}
	if !finite(power) {
		invalidate()
		return
	}
	at := now
	if d.PowerObservedAt != "" {
		var err error
		at, err = time.Parse(time.RFC3339Nano, d.PowerObservedAt)
		if err != nil || at.After(now) || now.Sub(at) > 10*time.Second {
			invalidate()
			return
		}
	}
	if s.controlObservations == nil {
		s.controlObservations = map[string][]ControlObservation{}
	}
	points := s.controlObservations[k]
	p := ControlObservation{power, at}
	if len(points) > 0 && !at.After(points[len(points)-1].At) {
		return
	}
	if len(points) > 0 && at.Unix() == points[len(points)-1].At.Unix() {
		points[len(points)-1] = p
	} else {
		points = append(points, p)
	}
	if len(points) > 32 {
		copy(points, points[len(points)-32:])
		points = points[:32]
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
		w := observationWindow(s.controlObservations[k], now.Add(-12*time.Second), now)
		if w.Usable(now) {
			result[k] = ControlBaseline{rd.Driver, rd.DerType, w}
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
		result[k] = ControlBaseline{rd.Driver, rd.DerType, observationWindow(s.controlObservations[k], from, now)}
	}
	return result
}
