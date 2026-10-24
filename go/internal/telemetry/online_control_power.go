package telemetry

import "time"

// onlineControlPower qualifies a device observation with receipt age, source
// age and driver health under one read lock. Cached power alone is not proof
// that a commanded import increase reached the site meter.
func (s *Store) onlineControlPower(driver string, kind DerType, now time.Time, maxAge time.Duration) (ControlObservation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.readings[key(driver, kind)]
	h := s.health[driver]
	if r == nil || h == nil || !h.TelemetryLive() || h.DeviceFault || maxAge <= 0 || r.UpdatedAt.After(now) || now.Sub(r.UpdatedAt) > maxAge {
		return ControlObservation{}, false
	}
	observation, known := ControlPowerObservation(r.RawW, r.Data, r.UpdatedAt)
	age := now.Sub(observation.Seen())
	if !known || age < 0 || age > maxAge || age > ControlPowerMaxAge(r.Data) {
		return ControlObservation{}, false
	}
	return observation, true
}
